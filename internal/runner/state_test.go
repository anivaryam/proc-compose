package runner

import (
	"context"
	"io"
	"regexp"
	"sync"
	"testing"

	"github.com/anivaryam/proc-compose/internal/config"
	"github.com/anivaryam/proc-compose/internal/ipc"
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

// TestProcState_HealthDescribesOnlyTheRunningIncarnation pins that the ProcState
// builder never reports a dead incarnation's health verdict: once the process
// is not running, health reads unknown with no health_error, and a process
// without health_when keeps an empty health.
func TestProcState_HealthDescribesOnlyTheRunningIncarnation(t *testing.T) {
	tests := []struct {
		state, health, wantHealth, wantError string
	}{
		{"running", healthUnhealthy, healthUnhealthy, "exit 1"},
		{"running", healthHealthy, healthHealthy, "exit 1"},
		{"restarting", healthUnhealthy, healthUnknown, ""},
		{"exited", healthHealthy, healthUnknown, ""},
		{"failed", healthHealthy, healthUnknown, ""},
		{"failed", "", "", ""}, // no health_when
	}
	for _, tc := range tests {
		t.Run(tc.state+"/"+tc.health, func(t *testing.T) {
			st := &procState{state: tc.state, health: tc.health, healthError: "exit 1"}
			st.mu.Lock()
			got := st.ipcStateLocked("api", config.Process{})
			st.mu.Unlock()
			if got.Health != tc.wantHealth || got.HealthError != tc.wantError {
				t.Errorf("health = %q, health_error = %q in state %q, want %q, %q", got.Health, got.HealthError, tc.state, tc.wantHealth, tc.wantError)
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

// TestProcState_MarkReadyForRejectsStaleCycle pins the probe race fix. A probe
// goroutine that outlives its incarnation must not latch the next cycle: the
// iteration cancels procCtx before resetReady, and resetReady runs before the
// next generation++, so only a context check made under st.mu closes the window.
func TestProcState_MarkReadyForRejectsStaleCycle(t *testing.T) {
	st := &procState{readyCh: make(chan struct{}), generation: 1}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	st.resetReady()
	if st.markReadyFor(ctx, 1, true, nil) {
		t.Fatal("markReadyFor latched a cycle after its incarnation context was cancelled")
	}
	st.mu.Lock()
	closed := st.readyClosed
	st.mu.Unlock()
	if closed {
		t.Fatal("readiness cycle latched by a stale probe")
	}
	select {
	case <-st.readyChannel():
		t.Fatal("readyCh closed by a stale probe")
	default:
	}
}

// TestProcState_OwnersFollowTheirIncarnation pins that who answered a probe
// is recorded with the readiness latch and refused with it for a stale
// generation; that the readiness owner, like last_error, outlives the
// incarnation's readiness while the health owner describes only a running
// one; and that a refused answer is kept only until the readiness latch.
func TestProcState_OwnersFollowTheirIncarnation(t *testing.T) {
	refused := &ipc.EndpointOwner{Result: ipc.OwnerForeign, Address: "127.0.0.1:3000", PID: 41}
	waiting := &procState{readyCh: make(chan struct{}), generation: 1, state: "running"}
	if waiting.recordRefusedOwner(2, refused) || waiting.readinessOwner != nil {
		t.Fatal("a stale generation's refusal was recorded")
	}
	// The incarnation already exited before ready (its server failed its own
	// bind): the refusal is still what explains it.
	waiting.state = "failed"
	waiting.markReady(false)
	if !waiting.recordRefusedOwner(1, refused) {
		t.Fatal("the refusal of an incarnation that exited unready was not recorded")
	}
	if got := waiting.ipcStateLocked("api", config.Process{}); got.ReadinessOwner != refused || got.Ready {
		t.Fatalf("failed incarnation: readiness_owner = %+v, want the refused answer", got.ReadinessOwner)
	}

	owner := &ipc.EndpointOwner{Result: ipc.OwnerForeign, Address: "127.0.0.1:3000", PID: 42, Command: "node"}
	st := &procState{readyCh: make(chan struct{}), generation: 2, state: "running"}
	if st.markReadyFor(context.Background(), 1, true, owner) || st.readinessOwner != nil {
		t.Fatal("a stale generation's probe recorded its owner")
	}
	if !st.markReadyFor(context.Background(), 2, true, owner) {
		t.Fatal("markReadyFor refused the live incarnation")
	}
	if st.recordRefusedOwner(2, refused) || st.readinessOwner != owner {
		t.Fatal("a refusal after readiness replaced the answer that made the incarnation ready")
	}
	health := &ipc.EndpointOwner{Result: ipc.OwnerOwned, Address: "127.0.0.1:3000", PID: 7}
	if st.recordHealthOwner(context.Background(), 2, health) {
		t.Fatal("an owned health answer was reported as foreign")
	}
	got := st.ipcStateLocked("api", config.Process{})
	if got.ReadinessOwner != owner || got.HealthOwner != health {
		t.Fatalf("live incarnation: owners = %+v / %+v", got.ReadinessOwner, got.HealthOwner)
	}
	st.state = "restarting"
	if got := st.ipcStateLocked("api", config.Process{}); got.ReadinessOwner != owner || got.HealthOwner != nil {
		t.Fatalf("restarting process: owners = %+v / %+v, want the readiness owner only", got.ReadinessOwner, got.HealthOwner)
	}
}

// TestProcState_ForeignHealthOwnerIsReportedOncePerHolder keeps the warning
// from repeating every health interval: it is reported when the answer
// becomes foreign or moves to another foreign holder.
func TestProcState_ForeignHealthOwnerIsReportedOncePerHolder(t *testing.T) {
	st := &procState{generation: 1, state: "running"}
	ctx := context.Background()
	foreign := func(pid int) *ipc.EndpointOwner { return &ipc.EndpointOwner{Result: ipc.OwnerForeign, PID: pid} }
	owned := &ipc.EndpointOwner{Result: ipc.OwnerOwned, PID: 7}
	for i, step := range []struct {
		owner *ipc.EndpointOwner
		gen   uint64
		want  bool
	}{
		{owned, 1, false},
		{foreign(42), 1, true},
		{foreign(42), 1, false},
		{foreign(43), 1, true},
		{owned, 1, false},
		{foreign(43), 1, true},
		{foreign(44), 2, false}, // a stale generation's check changes nothing
	} {
		if became := st.recordHealthOwner(ctx, step.gen, step.owner); became != step.want {
			t.Fatalf("step %d (%+v, gen %d): became foreign = %v, want %v", i, step.owner, step.gen, became, step.want)
		}
	}
	if st.healthOwner.PID != 43 {
		t.Fatalf("health owner = %+v, want the last live answer (PID 43)", st.healthOwner)
	}
}

// TestProcState_MarkReadyForRejectsOtherGeneration covers a probe whose context
// is still live but whose incarnation is no longer current.
func TestProcState_MarkReadyForRejectsOtherGeneration(t *testing.T) {
	st := &procState{readyCh: make(chan struct{}), generation: 2}
	if st.markReadyFor(context.Background(), 1, true, nil) {
		t.Fatal("markReadyFor latched a cycle for a previous generation")
	}
	if !st.markReadyFor(context.Background(), 2, true, nil) {
		t.Fatal("markReadyFor refused the live incarnation")
	}
	st.mu.Lock()
	ok := st.readyClosed && st.readyOK
	st.mu.Unlock()
	if !ok {
		t.Fatal("live incarnation not latched ready")
	}
}

// TestProcState_FailReadinessForIsGuarded pins the probe-failure half: a
// readiness failure is recorded only for the live, not-yet-ready incarnation,
// and it is what makes a graceful exit after the cancel count as a failure.
func TestProcState_FailReadinessForIsGuarded(t *testing.T) {
	live := context.Background()
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()

	st := &procState{readyCh: make(chan struct{}), generation: 3}
	if st.failReadinessFor(cancelled, 3, "late") {
		t.Fatal("failReadinessFor applied after the incarnation was cancelled")
	}
	if st.failReadinessFor(live, 2, "stale") {
		t.Fatal("failReadinessFor applied to a previous generation")
	}
	st.markReady(true)
	if st.failReadinessFor(live, 3, "after ready") {
		t.Fatal("failReadinessFor applied after the incarnation became ready")
	}
	st.mu.Lock()
	failed, lastError := st.probeFailed, st.lastError
	st.mu.Unlock()
	if failed || lastError != "" {
		t.Fatalf("refused failures left state behind: probeFailed=%t lastError=%q", failed, lastError)
	}

	st.resetReady()
	if !st.failReadinessFor(live, 3, "readiness probe (tcp) timed out") {
		t.Fatal("failReadinessFor refused the live, unready incarnation")
	}
	st.mu.Lock()
	failed, lastError = st.probeFailed, st.lastError
	st.mu.Unlock()
	if !failed || lastError != "readiness probe (tcp) timed out" {
		t.Fatalf("probeFailed=%t lastError=%q, want true and the probe message", failed, lastError)
	}
	if st.markReadyFor(live, 3, true, nil) {
		t.Fatal("a failed readiness cycle was later latched ready")
	}
}

// TestReadinessWiring_StaleIncarnationCannotLatchNextCycle pins the probe race
// fix where it is wired in, not only in markReadyFor itself. Each readiness
// path that reports success (the HTTP/TCP probe result, the no-probe onStart
// and the log onLine hook) must use the incarnation guard: once procCtx is
// cancelled and resetReady has opened the next cycle, a late success from the
// old incarnation must leave readyCh open. The live control proves each path
// does latch when its incarnation is still current.
func TestReadinessWiring_StaleIncarnationCannotLatchNextCycle(t *testing.T) {
	logRe := regexp.MustCompile("listening")
	paths := []struct {
		name  string
		ready *config.ReadyWhen
		fire  func(r *Runner, p procInfo, st *procState, ctx context.Context, cancel context.CancelFunc)
	}{
		{"probe result", &config.ReadyWhen{TCP: "127.0.0.1:1"}, func(r *Runner, p procInfo, st *procState, ctx context.Context, cancel context.CancelFunc) {
			r.handleProbeResult(p, st, 1, "tcp", true, "", nil, ctx, cancel)
		}},
		{"no-probe onStart", nil, func(r *Runner, p procInfo, st *procState, ctx context.Context, cancel context.CancelFunc) {
			onStart, _ := r.buildReadinessCallbacks(p, 0, st, 1, nil, 0, ctx, cancel, nil, nil, nil)
			onStart()
		}},
		{"log onLine", &config.ReadyWhen{Log: "listening"}, func(r *Runner, p procInfo, st *procState, ctx context.Context, cancel context.CancelFunc) {
			_, onLine := r.buildReadinessCallbacks(p, 0, st, 1, logRe, 0, ctx, cancel, nil, nil, nil)
			onLine("server listening on :8080")
		}},
	}
	for _, path := range paths {
		for _, stale := range []bool{true, false} {
			name := path.name + "/live"
			if stale {
				name = path.name + "/stale"
			}
			t.Run(name, func(t *testing.T) {
				r := &Runner{Output: io.Discard}
				p := procInfo{name: "web", proc: config.Process{Cmd: "true", ReadyWhen: path.ready}}
				st := &procState{readyCh: make(chan struct{}), generation: 1, state: "running"}
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				if stale {
					// Run loop order: cancel the incarnation, then open the next
					// cycle; generation++ has not happened yet.
					cancel()
					st.resetReady()
				}
				path.fire(r, p, st, ctx, cancel)
				latched := false
				select {
				case <-st.readyChannel():
					latched = true
				default:
				}
				if stale && latched {
					t.Fatal("a success from a cancelled incarnation latched the next readiness cycle")
				}
				if !stale && !latched {
					t.Fatal("a success from the live incarnation did not latch readiness")
				}
			})
		}
	}
}

// TestProcState_RecordHealthTransitions pins the health state machine: the
// first pass is healthy, healthThreshold consecutive failures make the
// incarnation unhealthy once per episode, one pass recovers it, and a result
// for an incarnation that is no longer current and running is dropped. Only
// the restart action marks the incarnation failed for the run loop.
func TestProcState_RecordHealthTransitions(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	st := &procState{state: "running", generation: 1, health: healthUnknown, readyCh: make(chan struct{})}
	step := func(ok bool, detail string, want healthTransition, wantHealth string, wantFailures int) {
		t.Helper()
		if got := st.recordHealth(ctx, 1, ok, detail, false); got != want {
			t.Fatalf("recordHealth(ok=%t) = %v, want %v", ok, got, want)
		}
		st.mu.Lock()
		defer st.mu.Unlock()
		if st.health != wantHealth || st.healthFailures != wantFailures {
			t.Fatalf("health=%q failures=%d, want %q/%d", st.health, st.healthFailures, wantHealth, wantFailures)
		}
	}
	step(true, "", healthBecameHealthy, healthHealthy, 0)
	step(true, "", healthNone, healthHealthy, 0)
	step(false, "exit 1", healthCheckFailed, healthHealthy, 1)
	step(false, "exit 1", healthCheckFailed, healthHealthy, 2)
	step(false, "exit 2", healthBecameUnhealthy, healthUnhealthy, healthThreshold)
	step(false, "exit 3", healthCheckFailed, healthUnhealthy, healthThreshold+1)
	st.mu.Lock()
	if st.healthError != "exit 3" || st.probeFailed || st.lastError != "" {
		t.Fatalf("report action: healthError=%q probeFailed=%t lastError=%q", st.healthError, st.probeFailed, st.lastError)
	}
	st.mu.Unlock()
	step(true, "", healthRecovered, healthHealthy, 0)
	st.mu.Lock()
	if st.healthError != "" {
		t.Fatalf("recovery kept healthError %q", st.healthError)
	}
	st.mu.Unlock()

	// A failure before any pass is unhealthy at the threshold too.
	st = &procState{state: "running", generation: 1, health: healthUnknown, readyCh: make(chan struct{})}
	for i := 1; i < healthThreshold; i++ {
		step(false, "refused", healthCheckFailed, healthUnknown, i)
	}
	step(false, "refused", healthBecameUnhealthy, healthUnhealthy, healthThreshold)

	// The restart action marks the incarnation failed for the run loop.
	st = &procState{state: "running", generation: 1, health: healthHealthy, readyCh: make(chan struct{})}
	for i := 1; i < healthThreshold; i++ {
		st.recordHealth(ctx, 1, false, "refused", true)
	}
	if got := st.recordHealth(ctx, 1, false, "refused", true); got != healthBecameUnhealthy {
		t.Fatalf("restart threshold = %v", got)
	}
	if !st.probeFailed || st.lastError != "health check failed (3 consecutive): refused" {
		t.Fatalf("restart action: probeFailed=%t lastError=%q", st.probeFailed, st.lastError)
	}

	// Stale results are dropped without touching the state.
	st = &procState{state: "running", generation: 2, health: healthHealthy, readyCh: make(chan struct{})}
	if got := st.recordHealth(ctx, 1, false, "old", true); got != healthNone || st.healthFailures != 0 {
		t.Fatalf("stale generation applied: %v failures=%d", got, st.healthFailures)
	}
	st.state = "restarting"
	if got := st.recordHealth(ctx, 2, false, "x", true); got != healthNone || st.healthFailures != 0 {
		t.Fatalf("non-running incarnation applied: %v", got)
	}
	st.state = "running"
	code := 0
	st.exitCode = &code
	if got := st.recordHealth(ctx, 2, false, "x", true); got != healthNone || st.healthFailures != 0 {
		t.Fatalf("exited incarnation applied: %v", got)
	}
	st.exitCode = nil
	cancel()
	if got := st.recordHealth(ctx, 2, false, "x", true); got != healthNone || st.healthFailures != 0 || st.probeFailed {
		t.Fatalf("cancelled incarnation applied: %v", got)
	}
}
