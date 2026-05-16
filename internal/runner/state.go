package runner

import (
	"sync"
	"time"

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

	// Readiness signaling for depends_on.
	// All fields below are guarded by mu so that resetReady (called between
	// run iterations) cannot race with markReady or with depends_on waiters.
	readyCh     chan struct{} // closed once the process is ready (or exits before becoming ready)
	readyClosed bool          // true once readyCh has been closed for the current cycle
	readyOK     bool          // true if the process actually became ready (vs exiting before ready)

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
func (st *procState) markReady(ok bool) {
	st.mu.Lock()
	if st.readyClosed {
		st.mu.Unlock()
		return
	}
	st.readyClosed = true
	st.readyOK = ok
	ch := st.readyCh
	st.mu.Unlock()
	close(ch)
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
		info := s.infoByName[name]
		out = append(out, ipc.ProcState{
			Name:       name,
			State:      p.state,
			Mode:       info.proc.EffectiveMode(),
			Ready:      p.readyClosed && p.readyOK,
			PID:        p.pid,
			Restarts:   p.restarts,
			StartedAt:  p.startedAt,
			ColorIndex: p.colorIndex,
			CPUPercent: p.cpuPercent,
			MemoryMB:   p.memoryMB,
		})
		p.mu.Unlock()
	}
	return out
}
