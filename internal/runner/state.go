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
	readyCh     chan struct{} // closed once the process is ready (or exits before becoming ready)
	readyClosed bool          // true once readyCh has been closed for the current cycle
	readyOK     bool          // true if the process actually became ready (vs exiting before ready)

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
			readyCh:    make(chan struct{}),
			restartCh:  make(chan struct{}, 1),
		}
		s.infoByName[p.name] = p
	}
	return s
}

// markReady closes readyCh so that any depends_on waiters unblock.
// ok=true means the process became genuinely ready; ok=false means it
// exited/failed before becoming ready (waiters should treat this as failure).
// Safe to call multiple times within a cycle; only the first call has any
// effect. After resetReady the cycle is renewed and markReady can fire again.
func (st *procState) markReady(ok bool) bool {
	return st.latchReady(ok, nil)
}

// latchReady closes the current cycle's readyCh. admit, when set, is evaluated
// under st.mu and can refuse the latch.
func (st *procState) latchReady(ok bool, admit func() bool) bool {
	st.mu.Lock()
	if st.readyClosed || (admit != nil && !admit()) {
		st.mu.Unlock()
		return false
	}
	st.readyClosed = true
	st.readyOK = ok
	ch := st.readyCh
	st.mu.Unlock()
	close(ch)
	return true
}

// markReadyFor is markReady for a probe bound to one incarnation. It latches
// only while ctx (the incarnation's context) is live, gen is still the current
// generation, and the incarnation has not failed readiness, all checked under
// st.mu. The run loop cancels the incarnation's context before it calls
// resetReady, and resetReady runs before the next generation++, so the context
// check is what keeps a probe that outlived its incarnation from latching the
// next cycle; a generation check alone would miss that window.
func (st *procState) markReadyFor(ctx context.Context, gen uint64, ok bool) bool {
	return st.latchReady(ok, func() bool {
		return !st.probeFailed && ctx.Err() == nil && st.generation == gen
	})
}

// failReadinessFor records a readiness failure for one incarnation, under the
// same guard as markReadyFor, and refuses once the incarnation is ready. It
// returns true when the failure was recorded; the caller must then cancel the
// incarnation. The run loop treats the next exit as a failure whatever status
// the reap reports.
func (st *procState) failReadinessFor(ctx context.Context, gen uint64, msg string) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	if ctx.Err() != nil || st.generation != gen || (st.readyClosed && st.readyOK) {
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
	if !st.readyClosed || !st.readyOK {
		return false
	}
	switch st.state {
	case "running", "completed":
		return true
	default:
		return false
	}
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
func (st *procState) resetReady() {
	st.mu.Lock()
	st.readyCh = make(chan struct{})
	st.readyClosed = false
	st.readyOK = false
	st.mu.Unlock()
}

// readyChannel returns the current readiness channel under lock so callers
// see a coherent view that resetReady cannot tear out from under them.
func (st *procState) readyChannel() chan struct{} {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.readyCh
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
	}
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
