package runner

import (
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// GroupState reports what is known about a managed command's process tree.
type GroupState int

const (
	// GroupEmpty means the command's process group has no live members.
	//
	// Members that have terminated but have not been reaped are not live: a
	// descendant whose parent exited is reparented to init, and a process
	// group whose only remaining members are such zombies would otherwise be
	// reported as a survivor of a shutdown that actually succeeded.
	GroupEmpty GroupState = iota

	// GroupOwned means the group has at least one live member and its identity
	// is established, so it may be signalled.
	//
	// Identity is established while the group leader is unreaped: the kernel
	// keeps a zombie process-table slot (and therefore its PID, and therefore
	// the process-group ID that equals it) reserved until a wait(2) collects it.
	// While the leader is unreaped no unrelated group can hold that numeric
	// identifier, so a signal sent to -pgid provably reaches only this command's
	// own descendants.
	GroupOwned

	// GroupUnavailable means containment could not be established when the
	// command was started, so nothing about the group can be reasoned about.
	GroupUnavailable
)

func (s GroupState) String() string {
	switch s {
	case GroupEmpty:
		return "empty"
	case GroupOwned:
		return "owned"
	case GroupUnavailable:
		return "unavailable"
	default:
		return "unknown"
	}
}

// ProcessGroup is the runner's authority over one managed command's process tree.
//
// The ownership invariant is a reaping discipline, not a time window:
//
//	The group may be signalled while the leader is unreaped, and the leader is
//	reaped only after every intended signal has been sent.
//
// Withdraw ends that authority. It is one-way, and it is called before the sole
// reap, so no signal source — the graceful escalation, a forced shutdown, or the
// stack sweep — can outlive the identity it relies on. Every signalling path
// serialises through the same lock that Withdraw takes, so a signal either
// completes entirely before the authority is withdrawn or is refused afterwards;
// there is no interleaving in which a signal follows the reap.
type ProcessGroup interface {
	// Setup prepares the command so its process tree can be managed. It is
	// called before the command is started.
	Setup(cmd *exec.Cmd)

	// Track records the identity of the started command. It is called once,
	// immediately after a successful start.
	Track(cmd *exec.Cmd) error

	// Terminate asks the group to stop gracefully. It returns
	// os.ErrProcessDone when the group can no longer be signalled.
	Terminate() error

	// Kill terminates the group immediately. It returns os.ErrProcessDone when
	// the group can no longer be signalled.
	Kill() error

	// State reports what is known about the group. It performs no signalling.
	State() GroupState

	// Identify returns the group's identifier for operator-facing messages.
	Identify() string

	// Withdraw ends signalling authority, one-way. It must be called before the
	// leader is reaped. After it returns, Terminate and Kill refuse.
	Withdraw()

	// Close releases the group's resources.
	Close() error
}

// GroupResult pairs a verdict about the group with the members that outlived it.
type GroupResult struct {
	// Name is the managed process the group belongs to.
	Name string
	// PGID is the group's identifier, or 0 when the group was never established.
	PGID int
	// State is what is known about the group after the attempt.
	State GroupState
	// Members describes processes that are still running and outside the
	// runner's control.
	Members []string
}

// Shutdown budget. See the notes on Withdraw and on exec for why these bound
// the escalation but not the reap.
const (
	// defaultShutdownGrace is how long a group is given to stop politely before it
	// is killed, when the process does not configure shutdown_timeout.
	defaultShutdownGrace = 5 * time.Second

	// killSettle bounds how long the runner waits to observe the effect of a
	// group-wide kill. It is a liveness bound, not a termination guarantee: a
	// process in uninterruptible sleep ignores SIGKILL until its I/O returns.
	killSettle = 2 * time.Second
)

// survivorsError describes processes that outlived a shutdown attempt.
//
// It is returned whenever termination could not be verified, so that an
// unverified outcome can never be reported as success.
func survivorsError(results []GroupResult) error {
	var b strings.Builder
	for _, r := range results {
		if len(r.Members) == 0 && r.State == GroupEmpty {
			continue
		}
		fmt.Fprintf(&b, "\n  %s:", r.Name)
		switch r.State {
		case GroupUnavailable:
			b.WriteString("\n    process group containment was unavailable, so termination could not be attempted")
		case GroupOwned:
			fmt.Fprintf(&b, "\n    process group %s", describeGroup(r))
			if len(r.Members) > 0 {
				fmt.Fprintf(&b, ": %s still running", strings.Join(r.Members, ", "))
			} else {
				// State() said the group still had a live member but the
				// members could not be enumerated. Saying so keeps the
				// diagnostic useful instead of rendering an empty list.
				b.WriteString(" still has a live member that could not be enumerated")
			}
			b.WriteString("\n    could not terminate")
		default:
			fmt.Fprintf(&b, "\n    process group %s reported %s", describeGroup(r), r.State)
		}
	}
	if b.Len() == 0 {
		return nil
	}
	return errors.New("managed process tree could not be verified as terminated:" + b.String())
}

// describeGroup renders a group's identifier for an operator-facing message,
// falling back to a placeholder when no identifier is available.
func describeGroup(r GroupResult) string {
	if r.PGID > 1 {
		return identifyPID(r.PGID)
	}
	return "(identifier unavailable)"
}

// identifyPID renders a pid for an operator-facing message.
func identifyPID(pid int) string { return strconv.Itoa(pid) }

// shutdownGrace is how long a group is given to stop politely before it is
// killed. A per-process shutdown_timeout overrides the default.
func shutdownGrace(timeout int) time.Duration {
	if timeout > 0 {
		return time.Duration(timeout) * time.Second
	}
	return defaultShutdownGrace
}

// singleReap runs cmd.Wait on a goroutine and hands the result to exactly one
// caller.
//
// It exists because the lifecycle legitimately wants the exit status in two
// places: once when a natural completion is detected, and once at the end where
// the reap is sequenced after signalling has finished. A plain channel cannot
// serve both, because a single send is drained by a single receive — the second
// receive blocks forever, which would hang every naturally completing run.
//
// recv is safe to call more than once and returns the same result each time; the
// result is published and closed rather than sent.
type singleReap struct {
	once   sync.Once
	done   chan struct{}
	wait   func() error
	result error
}

// start arranges for the wait to run on a goroutine and to publish its result
// when it returns.
func (r *singleReap) start(wait func() error) {
	r.wait = wait
	r.done = make(chan struct{})
	go func() {
		err := wait()
		r.once.Do(func() {
			r.result = err
			close(r.done)
		})
	}()
}

// started reports whether a wait is in flight. A reap that was never started has
// no result to receive, which is the case where the lifecycle must call cmd.Wait
// itself.
func (r *singleReap) started() bool { return r.done != nil }

// taken reports whether the result has already been published, which is how the
// lifecycle knows not to wait for it a second time.
func (r *singleReap) taken() bool {
	if r.done == nil {
		return false
	}
	select {
	case <-r.done:
		return true
	default:
		return false
	}
}

// recv returns the exit status, waiting for it if it has not arrived.
func (r *singleReap) recv() error {
	if r.done == nil {
		return nil
	}
	<-r.done
	return r.result
}
