package runner

import (
	"sync"
	"testing"

	"github.com/anivaryam/proc-compose/internal/config"
)

// TestProcState_RaceMarkReadyAndReset exercises concurrent markReady/resetReady
// to surface unsynchronised writes via the race detector. Run with -race.
func TestProcState_RaceMarkReadyAndReset(t *testing.T) {
	st := &procState{readyCh: make(chan struct{})}
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(3)
		go func() {
			defer wg.Done()
			st.markReady(true)
		}()
		go func() {
			defer wg.Done()
			st.resetReady()
		}()
		go func() {
			defer wg.Done()
			// Simulate a depends_on waiter reading the channel field.
			_ = st.readyChannel()
		}()
	}
	wg.Wait()
}

// TestProcState_ResetReadyDoesNotCloseStaleChannel verifies that markReady
// from a previous run cycle does not affect the newly-reset channel.
func TestProcState_ResetReadyDoesNotCloseStaleChannel(t *testing.T) {
	st := &procState{readyCh: make(chan struct{})}
	st.markReady(false)
	// First channel should be closed.
	select {
	case <-st.readyCh:
	default:
		t.Fatal("first readyCh should be closed after markReady")
	}

	st.resetReady()
	// New channel should be open.
	newCh := st.readyChannel()
	select {
	case <-newCh:
		t.Fatal("readyCh should be open after resetReady")
	default:
	}

	st.markReady(true)
	select {
	case <-newCh:
	default:
		t.Fatal("new readyCh should be closed after markReady on fresh cycle")
	}
}

// TestProcState_CurrentlyReadyRequiresLiveState pins the rule that a latched
// readiness only counts while the process can actually serve traffic.
func TestProcState_CurrentlyReadyRequiresLiveState(t *testing.T) {
	tests := []struct {
		state string
		want  bool
	}{
		{"running", true},
		{"completed", true}, // successful task
		{"starting", false},
		{"restarting", false},
		{"exited", false},
		{"failed", false},
	}
	for _, tc := range tests {
		t.Run(tc.state, func(t *testing.T) {
			st := &procState{state: "starting", readyCh: make(chan struct{})}
			st.markReady(true)
			st.state = tc.state
			st.mu.Lock()
			got := st.currentlyReady()
			st.mu.Unlock()
			if got != tc.want {
				t.Errorf("currentlyReady() = %t after ready latch with state %q, want %t", got, tc.state, tc.want)
			}
		})
	}
}

// TestProcState_CurrentlyReadyFalseWhenNeverReady covers the failed-before-ready
// case: a closed latch with readyOK=false must never read as ready.
func TestProcState_CurrentlyReadyFalseWhenNeverReady(t *testing.T) {
	st := &procState{state: "running", readyCh: make(chan struct{})}
	st.markReady(false)
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.currentlyReady() {
		t.Fatal("currentlyReady() = true after a failed readiness probe, want false")
	}
}

// TestStoreSnapshot_ExitedServiceNotReady is the end-to-end regression for the
// stale-readiness bug: a service that became ready and then exited must not be
// advertised as ready by the snapshot the waiter consumes.
func TestStoreSnapshot_ExitedServiceNotReady(t *testing.T) {
	store := newStateStore([]procInfo{
		{name: "svc", proc: config.Process{Cmd: "sleep 1"}},
		{name: "crashy", proc: config.Process{Cmd: "sleep 1"}},
		{name: "migrate", proc: config.Process{Cmd: "true", Mode: config.ProcessModeTask}},
	})

	svc := store.get("svc")
	svc.markReady(true)
	svc.mu.Lock()
	svc.state = "exited"
	svc.mu.Unlock()

	crashy := store.get("crashy")
	crashy.markReady(true)
	crashy.mu.Lock()
	crashy.state = "failed"
	crashy.mu.Unlock()

	task := store.get("migrate")
	task.markReady(true)
	task.mu.Lock()
	task.state = "completed"
	task.mu.Unlock()

	byName := map[string]bool{}
	for _, st := range store.snapshot() {
		byName[st.Name] = st.Ready
	}
	if byName["svc"] {
		t.Error("exited service reported Ready=true, want false")
	}
	if byName["crashy"] {
		t.Error("failed service reported Ready=true, want false")
	}
	if !byName["migrate"] {
		t.Error("completed task reported Ready=false, want true")
	}
}
