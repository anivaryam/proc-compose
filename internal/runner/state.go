package runner

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/anivaryam/proc-compose/internal/config"
	"github.com/anivaryam/proc-compose/internal/ipc"
)

type procState struct {
	mu         sync.Mutex
	state      string // starting | running | restarting | exited | completed | failed
	pid        int
	restarts   int
	startedAt  time.Time
	colorIndex int
	cpuPercent float64
	memoryMB   float64
	generation uint64
	lastError  string
	exitCode   *int
	active     bool

	// Readiness signaling for depends_on.
	// All fields below are guarded by mu so that resetReady (called between
	// run iterations) cannot race with markReady or with depends_on waiters.
	readyCycle *readyCycle // the current readiness cycle
	// readyDisposition is the process's final lifecycle verdict: readyTerminal
	// once its lifecycle is over and it will not run again, readyStopped once
	// the run is stopping. It is published together with the state transition
	// it belongs to, so a dependent can ask what became of the dependency
	// without racing that transition. A new incarnation clears it, because that
	// incarnation can run again.
	readyDisposition readyOutcome

	// probeFailed records that this incarnation was cancelled because its
	// readiness probe failed. The cancel alone is not a verdict: a command that
	// handles SIGTERM by exiting 0 can be reaped as a clean exit, so the run
	// loop reads this flag to count the incarnation as failed. Reset with the
	// generation at the start of every incarnation.
	probeFailed bool

	// Ongoing health (health_when) of the current incarnation, reset with the
	// generation. health is "" when the process has no health_when, otherwise
	// healthUnknown until the first verdict.
	health         string
	healthFailures int    // consecutive failed checks
	healthError    string // the last failed check, cleared by a pass

	// Who answered this incarnation's readiness probe: the answer that made
	// it ready, or, while foreign_listener: reject waits and after it
	// failed, the last answer it refused. healthOwner is who answered its
	// last passing health check. Both are http and tcp probes only, reset
	// with the generation, and never mutated once stored, so snapshots may
	// share them.
	readinessOwner *ipc.EndpointOwner
	healthOwner    *ipc.EndpointOwner

	// Restart signaling for restart command.
	restartCh chan struct{} // buffered; signaled to request a restart
}

type stateStore struct {
	mu         sync.RWMutex
	procs      map[string]*procState
	infoByName map[string]procInfo
}

func newStateStore(infos []procInfo) *stateStore {
	s := &stateStore{
		procs:      make(map[string]*procState, len(infos)),
		infoByName: make(map[string]procInfo, len(infos)),
	}
	for _, p := range infos {
		s.procs[p.name] = &procState{
			state:      "starting",
			active:     true,
			colorIndex: p.colorIndex,
			readyCycle: newReadyCycle(),
			restartCh:  make(chan struct{}, 1),
		}
		s.infoByName[p.name] = p
	}
	return s
}

// readyOutcome is the verdict a readiness cycle settles with.
//
// A cycle is the unit a depends_on waiter waits on. A cycle that ends without
// a verdict (readyAgain) is not the end of the wait: the dependency's
// incarnation ended and another one follows, so the next cycle carries the
// readiness.
type readyOutcome int

const (
	// readyUnset means the cycle has not settled yet.
	readyUnset readyOutcome = iota
	// readySatisfied means the process became ready.
	readySatisfied
	// readyTerminal means the process's lifecycle is over and it will not run
	// again, so a dependent must fail rather than wait for a readiness that
	// will never come.
	readyTerminal
	// readyAgain means the cycle ended without a verdict; a later cycle may
	// carry the readiness.
	readyAgain
	// readyStopped means the run is stopping; no verdict will come.
	readyStopped
)

// readyCycle is one readiness cycle.
//
// The channel is closed when the cycle settles, and the outcome is written
// before the close, so a waiter that wakes on the channel reads the verdict of
// the cycle it waited for. The outcome is guarded by the owning procState's
// mutex, like the pointer to the cycle itself.
type readyCycle struct {
	ch      chan struct{}
	outcome readyOutcome
}

// newReadyCycle returns an unsettled readiness cycle.
func newReadyCycle() *readyCycle { return &readyCycle{ch: make(chan struct{})} }

// neverReadyCycle is the fallback a process without a cycle waits on. Its
// channel is never closed, so the wait ends on cancellation alone.
var neverReadyCycle = &readyCycle{ch: make(chan struct{})}

// markReady settles the current readiness cycle.
// ok=true means the process became genuinely ready; ok=false means its
// lifecycle is over and waiters must treat this as a failure (readyTerminal).
// Safe to call multiple times within a cycle; only the first call has any
// effect. After resetReady the cycle is renewed and markReady can fire again.
func (st *procState) markReady(ok bool) bool {
	if ok {
		return st.latchReady(readySatisfied, nil)
	}
	return st.latchReady(readyTerminal, nil)
}

// latchReady settles the current cycle with outcome. admit, when set, is
// evaluated under st.mu and can refuse the latch.
func (st *procState) latchReady(outcome readyOutcome, admit func() bool) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	cycle := st.readyCycle
	if cycle == nil || cycle.outcome != readyUnset {
		return false
	}
	if admit != nil && !admit() {
		return false
	}
	cycle.outcome = outcome
	close(cycle.ch)
	return true
}

// markReadyFor is markReady for a probe bound to one incarnation. It latches
// only while ctx (the incarnation's context) is live, gen is still the current
// generation, and the incarnation has not failed readiness, all checked under
// st.mu. The run loop cancels the incarnation's context before it calls
// resetReady, and resetReady runs before the next generation++, so the context
// check is what keeps a probe that outlived its incarnation from latching the
// next cycle; a generation check alone would miss that window. owner, when
// set, is recorded in the same critical section, so no snapshot shows the
// readiness without who answered it.
func (st *procState) markReadyFor(ctx context.Context, gen uint64, ok bool, owner *ipc.EndpointOwner) bool {
	outcome := readyTerminal
	if ok {
		outcome = readySatisfied
	}
	return st.latchReady(outcome, func() bool {
		if st.probeFailed || ctx.Err() != nil || st.generation != gen {
			return false
		}
		st.readinessOwner = owner
		return true
	})
}

// recordRefusedOwner stores a readiness answer foreign_listener: reject
// refused for incarnation gen, unless that incarnation became ready, so the
// refusal stays visible after the incarnation fails. Unlike the readiness
// latch it does not require the incarnation to be live: a server that
// fails its own bind can exit before the refusal is recorded, and the
// refusal is what explains it. The generation check alone keeps a late
// refusal out of the next incarnation, whose start resets the owner under
// st.mu together with generation++. It reports whether it was stored.
func (st *procState) recordRefusedOwner(gen uint64, owner *ipc.EndpointOwner) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.generation != gen || st.readySettled() {
		return false
	}
	st.readinessOwner = owner
	return true
}

// recordHealthOwner stores who answered incarnation gen's last passing
// health check, guarded like recordHealth. It reports whether the answer has
// just become foreign, or moved to another foreign holder, so a foreign
// holder is logged once per change rather than on every check.
func (st *procState) recordHealthOwner(ctx context.Context, gen uint64, owner *ipc.EndpointOwner) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	if owner == nil || ctx.Err() != nil || st.generation != gen || st.state != "running" || st.exitCode != nil {
		return false
	}
	known := st.healthOwner != nil && st.healthOwner.Result == ipc.OwnerForeign && st.healthOwner.PID == owner.PID
	st.healthOwner = owner
	return owner.Result == ipc.OwnerForeign && !known
}

// failReadinessFor records a readiness failure for one incarnation, under the
// same guard as markReadyFor, and refuses once the incarnation is ready. It
// returns true when the failure was recorded; the caller must then cancel the
// incarnation. The run loop treats the next exit as a failure whatever status
// the reap reports.
func (st *procState) failReadinessFor(ctx context.Context, gen uint64, msg string) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	if ctx.Err() != nil || st.generation != gen || st.readySettled() {
		return false
	}
	st.probeFailed = true
	st.lastError = msg
	return true
}

// Health verdicts reported in ProcState.Health.
const (
	healthUnknown   = "unknown"
	healthHealthy   = "healthy"
	healthUnhealthy = "unhealthy"
)

// healthThreshold is how many consecutive failed health checks make a running
// incarnation unhealthy.
const healthThreshold = 3

// healthTransition is what one applied health check changed.
type healthTransition int

const (
	healthNone            healthTransition = iota // dropped, or a pass that changed nothing
	healthCheckFailed                             // a failure that did not change the verdict
	healthBecameHealthy                           // first pass of the incarnation
	healthBecameUnhealthy                         // healthThreshold consecutive failures
	healthRecovered                               // a pass while unhealthy
)

// recordHealth applies one health check result for incarnation gen. It is
// dropped unless ctx (the incarnation's context) is live, gen is current, and
// the incarnation is running and not yet reaped, all checked under st.mu, so a
// check that outlived its incarnation cannot change the next one. With
// restart set, the unhealthy transition also records the failure the run
// loop reads (probeFailed and lastError); the caller then cancels the
// incarnation.
func (st *procState) recordHealth(ctx context.Context, gen uint64, ok bool, detail string, restart bool) healthTransition {
	st.mu.Lock()
	defer st.mu.Unlock()
	if ctx.Err() != nil || st.generation != gen || st.state != "running" || st.exitCode != nil {
		return healthNone
	}
	if ok {
		previous := st.health
		st.health, st.healthFailures, st.healthError = healthHealthy, 0, ""
		switch previous {
		case healthHealthy:
			return healthNone
		case healthUnhealthy:
			return healthRecovered
		default:
			return healthBecameHealthy
		}
	}
	st.healthFailures++
	st.healthError = detail
	if st.healthFailures < healthThreshold || st.health == healthUnhealthy {
		return healthCheckFailed
	}
	st.health = healthUnhealthy
	if restart {
		st.probeFailed = true
		st.lastError = fmt.Sprintf("health check failed (%d consecutive): %s", healthThreshold, detail)
	}
	return healthBecameUnhealthy
}

// recordHealthSkipped marks incarnation gen's verdict unknown, with detail as
// health_error, when its health checks could not be launched: a verdict no
// check backs any more is not reported as current. It is guarded like
// recordHealth and counts neither as a pass nor as a failure, so the failure
// streak is kept. It reports whether anything changed.
func (st *procState) recordHealthSkipped(ctx context.Context, gen uint64, detail string) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	if ctx.Err() != nil || st.generation != gen || st.state != "running" || st.exitCode != nil {
		return false
	}
	if st.health == healthUnknown && st.healthError == detail {
		return false
	}
	st.health, st.healthError = healthUnknown, detail
	return true
}

// currentlyReady reports whether readiness should be advertised right now.
// markReady latches per run cycle, so a latched success has to be combined
// with the process's current state: an exited, failed, or restarting
// service is no longer ready, while a task that completed successfully
// stays ready. Callers must hold st.mu.
func (st *procState) currentlyReady() bool {
	if !st.readySettled() {
		return false
	}
	switch st.state {
	case "running", "completed":
		return true
	default:
		return false
	}
}

// readySettled reports whether the current cycle already settled with the
// ready verdict. Callers must hold st.mu.
func (st *procState) readySettled() bool {
	return st.readyCycle != nil && st.readyCycle.outcome == readySatisfied
}

// currentHealth is the health verdict to advertise right now. A verdict
// belongs to the incarnation whose checks earned it, so once the process is no
// longer running (exited, failed, restarting) it reads unknown, with no
// health_error, until the next incarnation's checks run; like currentlyReady,
// it never describes a process that has gone. Callers must hold st.mu.
func (st *procState) currentHealth() (health, healthError string) {
	switch {
	case st.state == "running":
		return st.health, st.healthError
	case st.health == "": // no health_when
		return "", ""
	}
	return healthUnknown, ""
}

// requestRestart sends a non-blocking restart signal to the process goroutine.
func (st *procState) requestRestart() {
	select {
	case st.restartCh <- struct{}{}:
	default: // already pending
	}
}

// resetReady prepares readiness state for a new run cycle (e.g., after restart).
//
// The cycle a depends_on waiter may already be holding is settled with
// readyAgain and never replaced silently: the waiter is woken with a verdict
// it can act on, and only then does the fresh cycle become current. A cycle
// that already settled keeps its verdict — a published outcome is immutable —
// and only the fresh cycle is installed.
//
// The lifecycle verdict is cleared with it: the cycle that ends here is not the
// end of the process's life, so the previous verdict no longer describes what
// became of it. A dependent cancelled before the next incarnation starts must
// end as "exited", not be failed by a verdict that belongs to the cycle that
// just ended.
func (st *procState) resetReady() {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.settleLocked(readyAgain)
	st.readyCycle = newReadyCycle()
	st.readyDisposition = readyUnset
}

// settleLocked publishes outcome on the current cycle, unless it already
// settled: a published outcome is never overwritten. Callers must hold st.mu.
func (st *procState) settleLocked(outcome readyOutcome) {
	cycle := st.readyCycle
	if cycle == nil || cycle.outcome != readyUnset {
		return
	}
	cycle.outcome = outcome
	close(cycle.ch)
}

// currentReadyCycle returns the cycle a depends_on waiter must wait on.
func (st *procState) currentReadyCycle() *readyCycle {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.readyCycle == nil {
		return neverReadyCycle
	}
	return st.readyCycle
}

// outcomeOf returns the verdict the given cycle settled with.
func (st *procState) outcomeOf(cycle *readyCycle) readyOutcome {
	if cycle == nil {
		return readyUnset
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	return cycle.outcome
}

// disposition returns the process's final lifecycle verdict: readyTerminal
// once its lifecycle is over, readyStopped once the run is stopping, and
// readyUnset while it can still run again.
func (st *procState) disposition() readyOutcome {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.readyDisposition
}

// failProcess transitions the process to the failed state, records msg as the
// last error and publishes the terminal readiness verdict.
//
// The transition and the verdict are one atomic step, so a dependent that asks
// what became of the dependency can never see the verdict before the
// transition it belongs to.
func (st *procState) failProcess(msg string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.state = "failed"
	if msg != "" {
		st.lastError = msg
	}
	st.readyDisposition = readyTerminal
	st.settleLocked(readyTerminal)
}

// endProcess transitions the process to the exited state and publishes the
// terminal readiness verdict: the lifecycle is over and the process will not
// run again, so a dependent must fail rather than wait for a readiness that
// will never come.
//
// The state stays "exited": a clean exit is not a failure of the process
// itself, only of the dependent that can no longer start.
func (st *procState) endProcess() {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.state = "exited"
	st.readyDisposition = readyTerminal
	st.settleLocked(readyTerminal)
}

// stopProcess transitions the process to the exited state and publishes the
// stopped readiness verdict: the run is stopping, so no verdict about
// readiness will come. msg, when set, becomes the last error.
func (st *procState) stopProcess(msg string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.state = "exited"
	if msg != "" {
		st.lastError = msg
	}
	st.readyDisposition = readyStopped
	st.settleLocked(readyStopped)
}

func (s *stateStore) get(name string) *procState {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.procs[name]
}

// info returns the definition the store reports for name.
func (s *stateStore) info(name string) (procInfo, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	info, ok := s.infoByName[name]
	return info, ok
}

// updateMetrics collects CPU and memory metrics for all running processes and
// stores them in the corresponding procState. Returns names of processes
// that are currently running (have a valid PID).
func (s *stateStore) updateMetrics() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var running []string
	for name, p := range s.procs {
		p.mu.Lock()
		if p.state == "running" && p.pid > 0 {
			cpu, mem, _ := collectMetrics(p.pid)
			p.cpuPercent = cpu
			p.memoryMB = mem
			running = append(running, name)
		}
		p.mu.Unlock()
	}
	return running
}

// snapshot returns the current state of all processes as IPC structs.
func (s *stateStore) snapshot() []ipc.ProcState {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]ipc.ProcState, 0, len(s.procs))
	for name, p := range s.procs {
		p.mu.Lock()
		out = append(out, p.ipcStateLocked(name, s.infoByName[name].proc))
		p.mu.Unlock()
	}
	return out
}

// ipcStateLocked is the only ProcState builder. snapshot (status, /ready,
// /health) and broadcastState (the event stream and the IPC cache) both call
// it, so they cannot report different fields. proc is the definition of the
// current incarnation. Callers must hold st.mu.
func (st *procState) ipcStateLocked(name string, proc config.Process) ipc.ProcState {
	health, healthError := st.currentHealth()
	return ipc.ProcState{
		Name:        name,
		State:       st.state,
		Mode:        proc.EffectiveMode(),
		Ready:       st.currentlyReady(),
		PID:         st.pid,
		Restarts:    st.restarts,
		StartedAt:   st.startedAt,
		ColorIndex:  st.colorIndex,
		CPUPercent:  st.cpuPercent,
		MemoryMB:    st.memoryMB,
		Generation:  st.generation,
		LastError:   st.lastError,
		ExitCode:    st.exitCode,
		DependsOn:   append([]string(nil), proc.DependsOn...),
		Health:      health,
		HealthError: healthError,
		// Like last_error, the readiness owner describes the latest
		// incarnation until the next one starts, so a refused answer is
		// still there after readiness failed. Like health, the health owner
		// describes only a running incarnation.
		ReadinessOwner: st.readinessOwner,
		HealthOwner:    healthOwnerIf(st.healthOwner, st.state == "running"),
	}
}

func healthOwnerIf(owner *ipc.EndpointOwner, running bool) *ipc.EndpointOwner {
	if !running {
		return nil
	}
	return owner
}

// setProc records the definition an incarnation picked up after a reload, so
// snapshot describes the same definition broadcastState does instead of the
// one loaded at startup.
func (s *stateStore) setProc(name string, proc config.Process) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if info, ok := s.infoByName[name]; ok {
		info.proc = proc
		s.infoByName[name] = info
	}
}

func (s *stateStore) anyActive() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, p := range s.procs {
		p.mu.Lock()
		active := p.active
		p.mu.Unlock()
		if active {
			return true
		}
	}
	return false
}
