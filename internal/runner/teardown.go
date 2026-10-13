package runner

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/anivaryam/proc-compose/internal/config"
	"github.com/anivaryam/proc-compose/internal/ipc"
)

// TeardownBudget bounds the whole teardown phase, each teardown process's
// termination grace included. With a service's default grace (5s) and the
// kill settle (2s) ahead of it, a stop still answers within
// shutdownAckTimeout.
const TeardownBudget = 15 * time.Second

// teardownMinStart is the least time, beyond its termination grace, that a
// teardown process must have left in the budget to be started at all.
const teardownMinStart = time.Second

// Teardown outcomes reported in TeardownResult.Status.
const (
	TeardownPassed    = "passed"     // exited 0 within the budget
	TeardownFailed    = "failed"     // could not start, or exited non-zero or by a signal
	TeardownTimedOut  = "timed_out"  // still running when its share of the budget ran out
	TeardownHandedOff = "handed_off" // not reaped within the budget; left to the final seal
	TeardownNotRun    = "not_run"    // the budget ran out before it could start
	TeardownSkipped   = "skipped"    // the phase did not run; Error says why
)

// Reasons a skipped teardown records in TeardownResult.Error.
const (
	TeardownSkipUnverified = "managed_processes_unverified"
	TeardownSkipHandoff    = "cleanup_handoff"
)

// TeardownResult is what the teardown phase observed for one teardown
// process.
type TeardownResult struct {
	Name       string
	Status     string
	ExitCode   *int
	StartedAt  time.Time // zero when it never started
	FinishedAt time.Time
	Error      string // why it did not pass; empty when it passed
}

// TeardownResults returns the current run's teardown outcomes in run order.
func (r *Runner) TeardownResults() []TeardownResult {
	r.stopMu.Lock()
	defer r.stopMu.Unlock()
	return append([]TeardownResult(nil), r.teardownResults...)
}

// TeardownError joins every teardown process that did not pass, one line
// each, every line starting with ipc.TeardownPrefix. It is nil when every
// teardown passed or none is configured.
func (r *Runner) TeardownError() error {
	var lines []string
	for _, res := range r.TeardownResults() {
		if res.Status != TeardownPassed {
			lines = append(lines, fmt.Sprintf("%s%s %s: %s", ipc.TeardownPrefix, res.Name, res.Status, res.Error))
		}
	}
	if len(lines) == 0 {
		return nil
	}
	return errors.New(strings.Join(lines, "\n"))
}

func (r *Runner) addTeardownResult(res TeardownResult) {
	r.stopMu.Lock()
	r.teardownResults = append(r.teardownResults, res)
	r.stopMu.Unlock()
}

// runTeardown runs the configured teardown processes one at a time, in name
// order, within the teardown budget. It runs only once every managed process
// is verified terminated: after a cleanup handoff or with any termination
// problem it records every teardown process as skipped instead, because
// releasing resources beside a process that may still use them is worse than
// leaving them. colorBase is the first color index after the managed
// processes'.
//
// Its own process groups are sealed and their reaps checked before it
// returns, so a teardown process that could not be terminated is reported
// like any managed process that could not be.
func (r *Runner) runTeardown(handedOff bool, colorBase int) {
	r.cfgMu.RLock()
	names := config.TeardownNames(r.Config)
	procs := make([]config.Process, len(names))
	for i, name := range names {
		procs[i] = r.Config.Processes[name]
	}
	r.cfgMu.RUnlock()
	if len(names) == 0 {
		return
	}
	// A stack that ended on its own has not closed admission yet; nothing
	// else may start from here on.
	r.closeAdmission()
	budget := TeardownBudget
	if r.teardownBudget > 0 {
		budget = r.teardownBudget
	}
	deadline := time.Now().Add(budget)
	if r.TeardownDeadline != nil {
		if clip := r.TeardownDeadline(); !clip.IsZero() && clip.Before(deadline) {
			deadline = clip
		}
	}

	skip, why := "", ""
	switch {
	case handedOff:
		skip, why = TeardownSkipHandoff, "cleanup was handed off before termination was verified"
	case r.ShutdownResult() != nil:
		skip, why = TeardownSkipUnverified, "managed processes were not verified terminated"
	}
	if skip != "" {
		for _, name := range names {
			r.addTeardownResult(TeardownResult{Name: name, Status: TeardownSkipped, Error: skip})
		}
		r.systemLog("error", "teardown.skipped", fmt.Sprintf("teardown skipped, %s: %s did not run", why, strings.Join(names, ", ")))
		return
	}

	r.lifecycleMu.Lock()
	r.teardownOpen = true
	r.lifecycleMu.Unlock()
	for i, name := range names {
		p := procInfo{name: name, proc: procs[i], colorIndex: colorBase + i, teardown: true}
		r.addTeardownResult(r.runTeardownProcess(p, deadline))
	}
	r.lifecycleMu.Lock()
	r.teardownOpen = false
	r.lifecycleMu.Unlock()
	r.sealGroups()
	r.reportPendingReaps()
}

// runTeardownProcess runs one teardown process as a runner-owned execution.
// Its command must finish by deadline minus its termination grace; it is then
// terminated through the same graceful and forced teardown as a managed
// process, and its reap is handed off at deadline.
func (r *Runner) runTeardownProcess(p procInfo, deadline time.Time) TeardownResult {
	result := TeardownResult{Name: p.name}
	grace := shutdownGrace(p.proc.ShutdownTimeout)
	if time.Until(deadline) <= grace+teardownMinStart {
		result.Status, result.Error = TeardownNotRun, "the teardown budget ran out before it could start"
		r.processLog(p, "error", "teardown.failed", "teardown not run: "+result.Error)
		return result
	}
	ctx, cancel := context.WithDeadline(context.Background(), deadline.Add(-grace))
	defer cancel()
	handoff := make(chan struct{})
	handoffTimer := time.AfterFunc(time.Until(deadline), func() { close(handoff) })
	defer handoffTimer.Stop()

	st := &procState{state: "starting", generation: 1, readyCh: make(chan struct{}), restartCh: make(chan struct{}, 1)}
	r.checksMu.Lock()
	if r.checks == nil {
		r.checks = make(map[string]*procState)
	}
	r.checks[p.name] = st
	r.checksMu.Unlock()

	r.processLog(p, "info", "teardown.started", "teardown started")
	res := r.runOwned(ctx, p, st, handoff, cmdProbeOutputLimit)
	passed := exitedZeroInTime(ctx, &res)
	result.ExitCode, result.StartedAt, result.FinishedAt = res.ExitCode, res.StartedAt, res.FinishedAt
	switch {
	case passed:
		result.Status = TeardownPassed
	case res.HandedOff:
		result.Status, result.Error = TeardownHandedOff, "its process group was not reaped within the teardown budget"
	case res.Started && ctx.Err() != nil:
		result.Status, result.Error = TeardownTimedOut, fmt.Sprintf("still running when the teardown budget (%s) ran out", teardownBudgetString(deadline, res.StartedAt, grace))
	default:
		result.Status, result.Error = TeardownFailed, cmdProbeFailure(res)
	}
	if result.Status == TeardownPassed {
		r.processLog(p, "info", "teardown.completed", "teardown completed")
	} else {
		r.processLog(p, "error", "teardown.failed", "teardown "+result.Status+": "+result.Error)
	}
	return result
}

// teardownBudgetString describes the share of the budget a teardown process
// had: from its start to its command deadline.
func teardownBudgetString(deadline, started time.Time, grace time.Duration) string {
	return deadline.Add(-grace).Sub(started).Round(time.Second).String()
}
