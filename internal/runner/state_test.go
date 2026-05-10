package runner

import (
	"sync"
	"testing"
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
