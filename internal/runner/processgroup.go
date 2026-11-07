package runner

import (
	"errors"
	"fmt"
	"os/exec"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/anivaryam/proc-compose/internal/daemon"
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
	//
	// Setup assigns cmd.Cancel, so the command must have been created with
	// exec.CommandContext. Go treats a non-nil Cancel on a command that was not
	// created that way as a mistake and refuses to start it: "exec: command
	// with a non-nil Cancel was not created with CommandContext".
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

	// Seal ends signalling authority and releases platform resources. The
	// observation includes identity captured before release invalidates it. State is
	// the last observation made while containment is still queryable; on
	// platforms where release destroys that evidence, it is deliberately not
	// recomputed after sealing. A non-nil error means the seal was not clean.
	Seal() (SealObservation, error)

	// Close releases the group's resources.
	Close() error
}

// SealObservation is the last state and stable operator-facing identity captured
// while a process group's containment resources are still available.
type SealObservation struct {
	State    GroupState
	Identity string
	// Members are the live members the seal's own observation enumerated, or
	// nil when that observation could not enumerate them.
	Members []string
	// Reason explains why the group's containment could not be established,
	// when the group knows one. It belongs to the group it was captured from.
	Reason string
}

// GroupResult pairs a verdict about the group with the members that outlived it.
type GroupResult struct {
	// Name is the managed process the group belongs to.
	Name string
	// PGID is the group's identifier, or 0 when the group was never established.
	PGID int
	// Identity is the immutable platform-specific identifier captured at seal.
	Identity string
	// State is what is known about the group after the attempt.
	State GroupState
	// Members describes processes that are still running and outside the
	// runner's control.
	//
	// It is populated only where the platform can enumerate a group's
	// membership, and only when the enumeration succeeded: a group whose
	// members could not be enumerated reports no members, so the verdict can
	// say that they could not be enumerated instead of rendering an empty
	// list that would read as "no members".
	Members []string
	// Reason explains why the group's containment could not be established,
	// when the group knows one. It belongs to the group it is reported for: a
	// group that has no reason of its own reports none.
	Reason string
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
			if reason := r.Reason; reason != "" {
				fmt.Fprintf(&b, ": %s", reason)
			}
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

// groupMemberLister is implemented by a process group that can enumerate its
// live members.
//
// It is an optional interface, so a group that cannot enumerate its
// membership simply does not implement it: the verdict then says the members
// could not be enumerated instead of naming an empty list.
type groupMemberLister interface {
	memberPIDs() []string
}

// groupReasonReporter is implemented by a process group that knows why its
// containment could not be established.
type groupReasonReporter interface {
	failureReason() string
}

// groupMembersOf returns the live members the group enumerated, or nil when it
// cannot enumerate them.
func groupMembersOf(pg ProcessGroup) []string {
	lister, ok := pg.(groupMemberLister)
	if !ok {
		return nil
	}
	return lister.memberPIDs()
}

// unavailableReason explains why a containment could not be established, when the
// group knows. It exists so an operator sees the actual cause instead of a bare
// "unavailable".
//
// It takes the group, so the reason belongs to the group it is reported for: a
// group that has no reason of its own reports none, and one group's failure
// never explains another's verdict.
func unavailableReason(pg ProcessGroup) string {
	reporter, ok := pg.(groupReasonReporter)
	if !ok {
		return ""
	}
	return reporter.failureReason()
}

// describeGroup renders a group's identifier for an operator-facing message,
// falling back to a placeholder when no identifier is available.
func describeGroup(r GroupResult) string {
	if r.Identity != "" && r.Identity != "unknown" {
		return r.Identity
	}
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

// trackedReap keeps an unresolved wait associated with the immutable process
// identity captured before its group is sealed. The waiter itself only updates
// singleReap and never mutates Runner state.
type trackedReap struct {
	process  string
	identity string
	reap     *singleReap
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

// RecoveryMode selects what RecoverGroups may do to a group it proves ours.
type RecoveryMode int

const (
	// RecoverGraceful sends SIGTERM to proven members, waits until the
	// deadline, then sends SIGKILL to whatever is left.
	RecoverGraceful RecoveryMode = iota
	// RecoverForce sends SIGKILL to proven members at once.
	RecoverForce
	// RecoverAssessOnly classifies groups without opening a pidfd or sending
	// any signal; a proven group with live members reports OutcomeLive.
	RecoverAssessOnly
)

// Outcomes of a recovered group.
const (
	OutcomeEmpty      = "empty"      // proven empty without a signal
	OutcomeTerminated = "terminated" // proven ours, signalled, then proven empty
	OutcomeUnknown    = "unknown"    // could not be proven empty; may still be running
	OutcomeLive       = "live"       // RecoverAssessOnly: proven ours and still running
)

// RecoveredGroup is the evidence for one process group recorded by a daemon
// that exited without a shutdown verdict.
type RecoveredGroup struct {
	GroupResult // Name, PGID and State in the shutdown vocabulary
	// LeaderStart is the recorded leader start time, 0 when none was recorded.
	LeaderStart uint64
	Outcome     string
	// Reason names the rule that decided Outcome: rebooted, boot_unknown,
	// no_members, pgid_reused, leader_proven, launch_pending, invalid_pgid,
	// own_group, leader_gone, leader_left_group, leader_start_unknown,
	// namespace_mismatch, enumeration_failed, signal_unavailable,
	// survived_sigkill or unproven_members.
	Reason string
	// Signals lists the signals delivered to the group's proven members.
	Signals []string
	// PIDs are the members signalled for a terminated group, and the live
	// members found for a live or unknown one.
	PIDs []int
	// LiveMembers counts the members still running when the group stayed
	// unknown or live.
	LiveMembers int
}

// CrashedRunError refuses a start over a run whose daemon exited without a
// shutdown verdict while some of its recorded process groups may still be
// running. Starting would overwrite the only record of those groups.
type CrashedRunError struct {
	PID    int
	Groups []RecoveredGroup
}

func (e *CrashedRunError) Error() string {
	return fmt.Sprintf("a previous daemon (PID %d) exited without a shutdown verdict and left process groups "+
		"that may still be running:\n    %s\n  stop them first: proc-compose stop",
		e.PID, strings.ReplaceAll(FormatRecovery(e.Groups), "\n", "\n    "))
}

// CheckCrashedRun reports whether a new run may take over pidPath, without
// changing anything: nil when there is no PID file, or when its daemon is gone
// and nothing it recorded can still be running. It refuses a live daemon and,
// on Unix, a crashed run whose recorded groups may still run or whose PID file
// is unreadable. It never signals.
//
// It is the early answer `up` gives before taking the session lease. Removing
// stale state there could delete the PID file of a run that took the lease and
// wrote it meanwhile, so only ResolveCrashedRun, under the lease, removes it.
func CheckCrashedRun(pidPath string) error {
	_, err := assessCrashedRun(pidPath)
	return err
}

// ResolveCrashedRun is CheckCrashedRun followed, when the PID file is stale, by
// removing it and its socket. Callers hold the session's instance lease, so no
// other run can have written the file being removed.
func ResolveCrashedRun(pidPath, socketPath string) error {
	stale, err := assessCrashedRun(pidPath)
	if err != nil {
		return err
	}
	if stale {
		daemon.Cleanup(pidPath, socketPath)
	}
	return nil
}

// alreadyRunningError is the refusal to start a second daemon for a config.
func alreadyRunningError(pid int) error {
	return fmt.Errorf(
		"proc-compose is already running for this config (PID %d)\n"+
			"  stop it first:  proc-compose stop\n"+
			"  or monitor it:  proc-compose monitor",
		pid,
	)
}

// FormatRecovery renders one stable key=value evidence line per group, sorted
// by name and then pgid. Fields with nothing to report are left out.
func FormatRecovery(groups []RecoveredGroup) string {
	sorted := slices.Clone(groups)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].Name != sorted[j].Name {
			return sorted[i].Name < sorted[j].Name
		}
		return sorted[i].PGID < sorted[j].PGID
	})
	lines := make([]string, len(sorted))
	for i, g := range sorted {
		lines[i] = g.evidence()
	}
	return strings.Join(lines, "\n")
}

func (g RecoveredGroup) evidence() string {
	var b strings.Builder
	name := g.Name
	if name == "" || strings.ContainsFunc(name, func(r rune) bool { return r <= ' ' || r == '"' || r == '=' || r == 0x7f }) {
		name = strconv.Quote(name)
	}
	fmt.Fprintf(&b, "recovered group=%s pgid=%d", name, g.PGID)
	if g.LeaderStart != 0 {
		fmt.Fprintf(&b, " leader_start=%d", g.LeaderStart)
	}
	fmt.Fprintf(&b, " outcome=%s reason=%s", g.Outcome, g.Reason)
	if len(g.Signals) > 0 {
		fmt.Fprintf(&b, " signals=%s", strings.Join(g.Signals, ","))
	}
	if g.LiveMembers > 0 {
		fmt.Fprintf(&b, " live_members=%d", g.LiveMembers)
	}
	if len(g.PIDs) > 0 {
		pids := make([]string, len(g.PIDs))
		for i, pid := range g.PIDs {
			pids[i] = strconv.Itoa(pid)
		}
		fmt.Fprintf(&b, " members=%s", strings.Join(pids, ","))
	}
	return b.String()
}
