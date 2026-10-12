package runner

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/anivaryam/proc-compose/internal/config"
	"github.com/anivaryam/proc-compose/internal/ipc"
)

// CheckSpec is a one-shot command owned by the running stack. It is executed
// directly from Argv (no shell), inside a runner-owned process group, so the
// stack's terminal sweep seals it with every other group.
type CheckSpec struct {
	Name           string            // journal process name; must not collide with a configured process
	Argv           []string          // exact argv; Argv[0] is resolved through PATH
	Dir            string            // working directory
	Env            map[string]string // overrides layered over the inherited environment
	Cleanup        time.Duration     // after cancellation, how long teardown/reap may take before handoff to the stack seal
	MaxOutputBytes int               // per-stream capture cap in bytes
	// CleanupDeadline, when set, replaces Cleanup with an absolute deadline. It
	// is called once, when cancellation is first observed, so a caller can start
	// one cleanup budget at that trigger and share it with stack teardown.
	CleanupDeadline func() time.Time
}

// CheckResult is what the runner observed about one check execution.
type CheckResult struct {
	Started          bool
	Generation       uint64
	PID              int
	StartedAt        time.Time
	FinishedAt       time.Time
	ExitCode         *int
	Signal           *string
	Stdout           string
	Stderr           string
	OutputTruncated  bool
	HandedOff        bool // the reap was not observed within Cleanup; the group was left for the stack seal
	Err              error
	AdmissionRefused bool
}

// Snapshot returns the current state of every managed process of the active
// run, or nil before Run has seeded its state.
func (r *Runner) Snapshot() []ipc.ProcState {
	store := r.store.Load()
	if store == nil {
		return nil
	}
	return store.snapshot()
}

// SetProcessWaiter replaces how command leaders are reaped; nil restores
// cmd.Wait. It is a test seam for packages that drive a Runner, so they can
// hold a reap and exercise cleanup budgets. Set it before the teardown it
// should affect begins.
func (r *Runner) SetProcessWaiter(wait func(*exec.Cmd) error) {
	r.waitProcess = wait
}

// stateFor resolves a process's state from the managed store or the auxiliary
// check registry, so check output carries its own generation and PID.
func (r *Runner) stateFor(name string) *procState {
	if store := r.store.Load(); store != nil {
		if st := store.get(name); st != nil {
			return st
		}
	}
	r.checksMu.Lock()
	defer r.checksMu.Unlock()
	return r.checks[name]
}

// RunCheck executes spec while the stack is running. Cancelling ctx (deadline
// or interruption) terminates the check's process group through the same
// graceful/forced teardown as managed processes. Reaping is bounded by the
// cleanup deadline; past it the group stays registered so Run's terminal sweep
// seals it, and the result reports the handoff instead of termination.
//
// The check is runner-owned: stopping the stack cancels it, the stack's cleanup
// handoff also hands it off, and Run does not return until RunCheck has
// finished, so no check event is emitted after Run returns.
func (r *Runner) RunCheck(ctx context.Context, spec CheckSpec) CheckResult {
	if len(spec.Argv) == 0 || spec.Argv[0] == "" {
		return CheckResult{Err: errors.New("empty check command")}
	}
	r.lifecycleMu.Lock()
	if r.checkWG == nil || r.checksClosed || r.launchAdmissionClosedLocked() {
		r.lifecycleMu.Unlock()
		return CheckResult{Err: errors.New("no stack is running"), AdmissionRefused: true}
	}
	checkWG, stackCtx, stackHandoff := r.checkWG, r.checkCtx, r.checkHandoff
	checkWG.Add(1)
	r.activeChecks++
	r.lifecycleMu.Unlock()
	defer func() {
		r.lifecycleMu.Lock()
		r.activeChecks--
		r.lifecycleMu.Unlock()
		checkWG.Done()
	}()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer context.AfterFunc(stackCtx, cancel)()

	st := &procState{state: "starting", generation: 1, readyCh: make(chan struct{}), restartCh: make(chan struct{}, 1)}
	r.checksMu.Lock()
	if r.checks == nil {
		r.checks = make(map[string]*procState)
	}
	if _, exists := r.checks[spec.Name]; exists {
		r.checksMu.Unlock()
		return CheckResult{Err: fmt.Errorf("check %q already ran in this stack", spec.Name)}
	}
	r.checks[spec.Name] = st
	r.checksMu.Unlock()

	stdout := &cappedBuffer{limit: spec.MaxOutputBytes}
	stderr := &cappedBuffer{limit: spec.MaxOutputBytes}
	p := procInfo{
		name: spec.Name,
		proc: config.Process{Dir: spec.Dir, Env: spec.Env, Mode: config.ProcessModeTask},
		argv: append([]string(nil), spec.Argv...),
		capture: func(stream, line string) {
			if stream == "stderr" {
				stderr.writeLine(line)
			} else {
				stdout.writeLine(line)
			}
		},
	}

	handoff := make(chan struct{})
	execDone := make(chan struct{})
	handoffDone := make(chan struct{})
	go func() {
		defer close(handoffDone)
		select {
		case <-execDone:
			return
		case <-ctx.Done():
		}
		deadline := time.Now().Add(spec.Cleanup)
		if spec.CleanupDeadline != nil {
			deadline = spec.CleanupDeadline()
		}
		timer := time.NewTimer(time.Until(deadline))
		defer timer.Stop()
		select {
		case <-execDone:
		case <-timer.C:
			close(handoff)
		case <-stackHandoff:
			close(handoff)
		}
	}()

	result := CheckResult{Generation: 1}
	started := time.Now()
	var startedOnce sync.Once
	onStart := func() { startedOnce.Do(func() { result.Started = true }) }
	err := r.exec(ctx, p, r.maxName, st, onStart, nil, handoff)
	close(execDone)
	<-handoffDone
	result.FinishedAt = time.Now()

	st.mu.Lock()
	result.PID = st.pid
	if st.pid != 0 || st.exitCode != nil {
		result.Started = true
	}
	if st.exitCode != nil {
		code := *st.exitCode
		result.ExitCode = &code
	}
	st.mu.Unlock()
	if result.Started {
		result.StartedAt = started
	}
	result.Stdout, result.Stderr = stdout.String(), stderr.String()
	result.OutputTruncated = stdout.truncated || stderr.truncated

	switch {
	case errors.Is(err, errCleanupHandoff):
		result.HandedOff = true
		result.ExitCode = nil
	case !result.Started && errors.Is(err, context.Canceled):
		result.AdmissionRefused = true
		result.Err = err
	case !result.Started && err != nil:
		result.Err = err
	default:
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == -1 && exitErr.ProcessState != nil {
			signal := strings.TrimPrefix(exitErr.ProcessState.String(), "signal: ")
			result.Signal = &signal
			result.ExitCode = nil
		} else if err != nil && !errors.As(err, &exitErr) {
			result.Err = err
		}
	}

	st.mu.Lock()
	switch {
	case result.ExitCode != nil && *result.ExitCode == 0:
		st.state = "completed"
	case result.Started:
		st.state = "failed"
	default:
		st.state = "failed"
	}
	st.pid = 0
	st.mu.Unlock()
	r.broadcastState(p, st)
	return result
}

// cappedBuffer keeps at most limit bytes of line-oriented output and records
// whether anything was dropped. Truncation never splits a UTF-8 sequence.
type cappedBuffer struct {
	mu        sync.Mutex
	limit     int
	data      []byte
	truncated bool
}

func (b *cappedBuffer) writeLine(line string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.truncated {
		return
	}
	line += "\n"
	if room := b.limit - len(b.data); len(line) > room {
		cut := room
		for cut > 0 && cut < len(line) && line[cut]&0xC0 == 0x80 {
			cut--
		}
		b.data = append(b.data, line[:max(cut, 0)]...)
		b.truncated = true
		return
	}
	b.data = append(b.data, line...)
}

func (b *cappedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.data)
}
