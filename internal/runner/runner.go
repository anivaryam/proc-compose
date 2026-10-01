// Package runner manages the lifecycle of configured processes: starting, restarting, and stopping.
package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/anivaryam/proc-compose/internal/ansi"
	"github.com/anivaryam/proc-compose/internal/config"
	"github.com/anivaryam/proc-compose/internal/ipc"
)

// Runner starts and manages all configured processes.
type Runner struct {
	Config     *config.Config
	ConfigPath string // absolute path to config file (used for reload)
	Filter     []string
	LogFile    io.Writer   // if non-nil, all output is also written here (no ANSI)
	IPC        *ipc.Server // if non-nil, state + log events are broadcast to monitors
	NoColor    bool        // disable ANSI color output (also set via NO_COLOR env var)
	Verbose    bool        // enable verbose debug output
	LogFormat  string      // output format: "text" (default) or "json"
	HealthPort int         // if > 0, HTTP server listens on this port for /health
	Silent     bool        // suppress startup banner (daemon child sets this)

	store atomic.Pointer[stateStore] // set during Run for command dispatch

	// cfgMu guards concurrent access to Config.Processes between Reload (writer)
	// and the per-process restart loop (reader). Run-time startup and
	// ListProcesses are single-threaded relative to Reload and need no lock.
	cfgMu sync.RWMutex

	// groups holds the live process group of every running managed process.
	// It exists so shutdown can act on all of them, not only the ones whose
	// goroutine is still parked in cmd.Wait: a descendant that escapes the
	// log pipe can keep a command alive long after its goroutine would
	// otherwise return. Entries live only for the duration of one run
	// iteration and are dropped as soon as the group is torn down — nothing
	// here is persisted, and nothing outlives the process.
	groupsMu sync.Mutex
	groups   map[string]ProcessGroup

	// stopping is set once the run context is cancelled, so every process
	// goroutine learns that shutdown is in progress and treats leftovers in
	// its group as orphans to terminate rather than as backgrounded work.
	stopping atomic.Bool

	// stopCancel cancels the run context. It is stored (rather than captured)
	// because the shutdown request arrives on the IPC command goroutine.
	stopMu       sync.Mutex
	stopCancel   context.CancelFunc
	stopped      chan struct{}
	stopVerdict  chan error // buffered; carries the verdict to a waiting client
	stopProblems []string

	// stopAck keeps the daemon alive until an in-flight "shutdown" command has
	// had its verdict written to the client. Run's exit seals and drains it.
	stopAck ackBarrier
}

// ackBarrier holds Run's exit until every in-flight shutdown command has
// finished delivering its verdict.
//
// A sync.WaitGroup cannot express this safely: Add may land concurrently with
// Wait, and Wait is then free to return before the holder it was meant to wait
// for has even registered. This barrier seals instead — once sealed no new
// holder can enter — and then drains to zero, which cannot race.
type ackBarrier struct {
	mu      sync.Mutex
	holders int
	sealed  bool
	drained chan struct{}
}

// enter registers a holder. It fails once the barrier is sealed, which is how a
// late-arriving command learns it must not hold up an already-finishing run.
func (b *ackBarrier) enter() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.sealed {
		return false
	}
	if b.holders == 0 && b.drained == nil {
		b.drained = make(chan struct{})
	}
	b.holders++
	return true
}

// leave releases a holder, satisfying a pending seal once the last one exits.
// Every enter must be matched by exactly one leave, otherwise the seal waits for
// a holder that no longer exists.
func (b *ackBarrier) leave() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.holders--
	if b.sealed && b.holders == 0 && b.drained != nil {
		close(b.drained)
		b.drained = nil
	}
}

// sealAndWait blocks new holders and waits for the current ones to finish. The
// timeout is a liveness backstop only: a holder always releases within its own
// bound, so this cannot release early with a verdict still undelivered.
func (b *ackBarrier) sealAndWait(timeout time.Duration) {
	b.mu.Lock()
	b.sealed = true
	if b.holders == 0 {
		if b.drained != nil {
			close(b.drained)
			b.drained = nil
		}
		b.mu.Unlock()
		return
	}
	ch := b.drained
	b.mu.Unlock()

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-ch:
	case <-timer.C:
	}
}

// getProc returns a snapshot of a process definition under read lock.
func (r *Runner) getProc(name string) (config.Process, bool) {
	r.cfgMu.RLock()
	defer r.cfgMu.RUnlock()
	p, ok := r.Config.Processes[name]
	return p, ok
}

type procInfo struct {
	name       string
	proc       config.Process
	colorIndex int
}

func (r *Runner) Run(ctx context.Context) error {
	if r.NoColor {
		ansi.SetDisabled(true)
	}
	if r.LogFormat == "json" {
		LogFormat = "json"
		ansi.SetDisabled(true)
	}

	procs := r.resolve()
	if len(procs) == 0 {
		return fmt.Errorf("no matching processes to run")
	}

	maxName := 0
	names := make([]string, 0, len(procs))
	for _, p := range procs {
		if len(p.name) > maxName {
			maxName = len(p.name)
		}
		names = append(names, p.name)
	}

	PrintBanner(names, maxName, BannerOptions{Silent: r.Silent})

	// Seed IPC with initial state before any process starts.
	store := newStateStore(procs)
	r.store.Store(store)

	// Start health server if configured. Bind synchronously so a failure
	// surfaces to the caller (and aborts daemon startup) instead of silently
	// disappearing into a goroutine.
	if r.HealthPort > 0 {
		ln, err := net.Listen("tcp", fmt.Sprintf(":%d", r.HealthPort))
		if err != nil {
			return fmt.Errorf("health server bind failed on port %d: %w", r.HealthPort, err)
		}
		go r.serveHealth(ln, store)
	}
	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()
	// The shutdown handle must exist before any command can be dispatched:
	// handleCommands is the only caller of Shutdown, and a command that arrived
	// first would otherwise see a half-initialised runner.
	r.beginRun(runCtx, cancelRun)
	defer r.endRun()

	if r.IPC != nil {
		for _, st := range store.snapshot() {
			r.IPC.SetState(st)
		}
		go r.handleCommands(store)
		go r.collectMetrics(ctx, store, procs)
	}

	var (
		wg          sync.WaitGroup
		failedMu    sync.Mutex
		failedNames []string
	)
	for _, p := range procs {
		wg.Add(1)
		go func(p procInfo) {
			defer wg.Done()
			result := r.runProcess(runCtx, p, maxName, store)
			if result.failed {
				failedMu.Lock()
				failedNames = append(failedNames, p.name)
				failedMu.Unlock()
				if result.failFast {
					cancelRun()
				}
			}
		}(p)
	}

	wg.Wait()

	// Every per-process goroutine has returned, but that only proves each
	// command's group *leader* exited. During shutdown, sweep whatever is still
	// tracked so the daemon never exits — and never lets `stop` report success
	// — while a managed process is still running.
	r.sweepGroups(runCtx)

	// ctx.Err() != nil means a signal triggered shutdown — not a failure.
	var runErr error
	if ctx.Err() == nil && len(failedNames) > 0 {
		runErr = fmt.Errorf("processes failed: %s", strings.Join(failedNames, ", "))
	}
	if stopErr := r.ShutdownResult(); stopErr != nil {
		if runErr != nil {
			return fmt.Errorf("%v; %w", runErr, stopErr)
		}
		return stopErr
	}
	return runErr
}

// beginRun publishes the cancellation handle and the completion channel that
// Shutdown uses. It must run before the IPC command loop starts, so a
// "shutdown" command can never observe a half-initialised runner.
func (r *Runner) beginRun(runCtx context.Context, cancel context.CancelFunc) {
	r.stopping.Store(false)
	r.stopMu.Lock()
	r.stopCancel = cancel
	r.stopped = make(chan struct{})
	// Buffered so a verdict published by endRun never blocks, even when no
	// client is waiting for it.
	r.stopVerdict = make(chan error, 1)
	r.stopProblems = nil
	r.stopMu.Unlock()

	// Mark shutdown as in progress the moment the run context is cancelled.
	// A signal (Ctrl+C, `stop`) cancels the parent context directly, so this
	// is the only place that observes it.
	go func() {
		<-runCtx.Done()
		r.stopping.Store(true)
	}()
}

// endRun publishes the shutdown verdict and unblocks anyone waiting on it.
//
// Ordering matters. `stopped` is closed first so a waiting "shutdown" command
// can send its verdict; Run then waits for that command to finish flushing the
// ack. The daemon's caller is still reading that ack and it carries the list
// of anything that could not be terminated, so it must be on the wire before
// main tears the IPC server down.
func (r *Runner) endRun() {
	verdict := r.ShutdownResult()

	// Publish first, drain second.
	//
	// A waiting shutdown command releases the ack barrier only after its verdict
	// has been written to the client, so draining before publishing would
	// deadlock against the very handler this run is waiting for. Publishing
	// first unblocks it; draining afterwards is what keeps the daemon alive
	// until the verdict has actually been written.
	r.stopMu.Lock()
	stopped := r.stopped
	stopVerdict := r.stopVerdict
	r.stopCancel = nil
	r.stopped = nil
	r.stopVerdict = nil
	r.stopMu.Unlock()

	if stopVerdict != nil {
		// Non-blocking: the channel is buffered and this is the only sender.
		select {
		case stopVerdict <- verdict:
		default:
		}
	}
	if stopped != nil {
		close(stopped)
	}

	r.stopAck.sealAndWait(shutdownFlushTimeout)
}

// shutdownFlushTimeout bounds how long Run's exit waits for an in-flight
// shutdown command to finish delivering its verdict. It is a liveness backstop
// only — the holder releases as soon as its own write attempt completes — so it
// never releases early with a verdict still undelivered.
const shutdownFlushTimeout = 10 * time.Second

func (r *Runner) resolve() []procInfo {
	var procs []procInfo

	names := make([]string, 0, len(r.Config.Processes))
	for name := range r.Config.Processes {
		names = append(names, name)
	}
	sort.Strings(names)

	filterSet := make(map[string]bool)
	for _, f := range r.Filter {
		filterSet[f] = true
	}

	// Expand filter to include transitive dependencies so that
	// "proc-compose up app" also starts anything app depends on.
	if len(filterSet) > 0 {
		var expand func(name string)
		expand = func(name string) {
			proc, ok := r.Config.Processes[name]
			if !ok {
				return
			}
			for _, dep := range proc.DependsOn {
				if !filterSet[dep] {
					filterSet[dep] = true
					expand(dep)
				}
			}
		}
		initial := make([]string, 0, len(filterSet))
		for name := range filterSet {
			initial = append(initial, name)
		}
		for _, name := range initial {
			expand(name)
		}
	}

	colorIdx := 0
	for _, name := range names {
		if len(filterSet) > 0 && !filterSet[name] {
			continue
		}
		procs = append(procs, procInfo{
			name:       name,
			proc:       r.Config.Processes[name],
			colorIndex: colorIdx,
		})
		colorIdx++
	}
	return procs
}

// handleCommands reads IPC commands and dispatches them to process
// goroutines, replying via cmd.Reply so the IPC server can ack the client
// with a real verdict. Every code path must reply (or send a default error
// on unknown actions); otherwise the server's 30s wait timer fires.
func (r *Runner) handleCommands(store *stateStore) {
	for cmd := range r.IPC.Commands() {
		var res ipc.CommandResult
		// holdsBarrier is the single source of truth for acquire/release pairing
		// here. Anything that acquires the barrier in one place and releases it
		// in another can drift out of balance — and an unbalanced barrier either
		// stalls Run's exit or unblocks it early.
		holdsBarrier := false
		switch cmd.Action {
		case "restart":
			if cmd.Process == "" {
				res = ipc.CommandResult{Status: "error", Message: "restart requires a process name"}
				break
			}
			r.cfgMu.RLock()
			proc, known := r.Config.Processes[cmd.Process]
			r.cfgMu.RUnlock()
			if !known {
				res = ipc.CommandResult{Status: "error", Message: fmt.Sprintf("unknown process %q", cmd.Process)}
				break
			}
			st := store.get(cmd.Process)
			if st == nil {
				res = ipc.CommandResult{Status: "error", Message: fmt.Sprintf("process %q is not currently managed", cmd.Process)}
				break
			}
			if proc.EffectiveMode() == config.ProcessModeTask || st.state == "completed" {
				res = ipc.CommandResult{Status: "error", Message: fmt.Sprintf("process %q is a task and cannot be restarted; restart the stack to rerun tasks", cmd.Process)}
				break
			}
			st.requestRestart()
			res = ipc.CommandResult{Status: "ok"}
		case "reload":
			res = r.Reload()
		case "shutdown":
			// Take the barrier before requesting the shutdown, so Run cannot
			// finish — and tear down the IPC server — while this verdict is
			// still undelivered.
			holdsBarrier = r.stopAck.enter()
			if holdsBarrier {
				res = r.handleShutdown(cmd)
			} else {
				res = ipc.CommandResult{
					Status:  "error",
					Message: "daemon is already shutting down and cannot accept another stop request",
				}
			}
		default:
			res = ipc.CommandResult{Status: "error", Message: "unknown action: " + cmd.Action}
		}
		if cmd.Reply != nil {
			cmd.Reply <- res
		}
		if !holdsBarrier {
			continue
		}
		// The verdict is not delivered until the server has written the
		// acknowledgment carrying it, and only this goroutine can observe both
		// the reply and the flush. WaitFlushed returns on disconnect, write
		// failure and timeout alike, so a client that goes away cannot wedge
		// the daemon — and the barrier is released on every path.
		cmd.WaitFlushed(shutdownFlushTimeout)
		r.stopAck.leave()
	}
}

// shutdownAckTimeout bounds how long a "shutdown" command waits for the stack
// to finish tearing itself down. It sits below the IPC server's 30s ack
// timeout so a client gets the daemon's real verdict rather than the server's
// generic "timed out waiting for runner reply".
const shutdownAckTimeout = 25 * time.Second

// handleShutdown stops the whole stack on request and reports the verified
// outcome. It is the path both `proc-compose stop` and `stop --force` use:
// signalling the daemon outright would destroy the only record of which process
// groups it manages, and orphan every one of them.
//
// It does NOT take the ack barrier. The caller owns that, because only the caller
// can also observe the reply and the flush that follow.
func (r *Runner) handleShutdown(cmd ipc.Command) ipc.CommandResult {
	stopped, verdictCh, err := r.Shutdown(cmd.Force)
	if err != nil {
		return ipc.CommandResult{Status: "error", Message: err.Error()}
	}

	select {
	case <-stopped:
	case <-time.After(shutdownAckTimeout):
		return ipc.CommandResult{
			Status:  "error",
			Message: fmt.Sprintf("daemon did not finish shutting down within %s; managed processes were not verified", shutdownAckTimeout),
		}
	}

	// The verdict is published by endRun immediately after the final sweep, so
	// it is available as soon as <-stopped fires. A verdict that never arrives
	// is reported as "not verified" rather than defaulted to success.
	var verdict error
	select {
	case verdict = <-verdictCh:
	case <-time.After(shutdownVerdictTimeout):
		verdict = errors.New("daemon finished shutting down without publishing a verdict")
	}

	if verdict != nil {
		// "partial" rather than "error": the stack is down, but termination
		// could not be fully verified. The caller must not read this as a
		// clean stop.
		return ipc.CommandResult{Status: "partial", Message: verdict.Error()}
	}
	return ipc.CommandResult{Status: "ok"}
}

// shutdownVerdictTimeout bounds the wait for endRun's verdict. It is generous
// because the two are published together, and exists only so a lost verdict
// degrades to "not verified" instead of hanging the daemon.
const shutdownVerdictTimeout = 5 * time.Second

// serveHealth runs the HTTP server on the given pre-bound listener.
// The listener is bound synchronously by Run so bind failures abort startup.
func (r *Runner) serveHealth(ln net.Listener, store *stateStore) {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, req *http.Request) {
		r.cfgMu.RLock()
		defer r.cfgMu.RUnlock()
		procs := store.snapshot()
		type procStatus struct {
			Name     string `json:"name"`
			State    string `json:"state"`
			Restarts int    `json:"restarts"`
		}
		status := make([]procStatus, 0, len(procs))
		allHealthy := true
		for _, p := range procs {
			status = append(status, procStatus{Name: p.Name, State: p.State, Restarts: p.Restarts})
			healthy := false
			if p.Mode == config.ProcessModeTask {
				healthy = p.State == "completed"
			} else {
				healthy = p.State == "running" || p.State == "restarting"
			}
			if !healthy {
				allHealthy = false
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		if allHealthy {
			w.WriteHeader(http.StatusOK)
		} else {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		if err := json.NewEncoder(w).Encode(map[string]interface{}{
			"healthy":   allHealthy,
			"processes": status,
			"timestamp": time.Now().Format(time.RFC3339),
		}); err != nil && r.Verbose {
			fmt.Fprintf(os.Stderr, "health encode: %v\n", err)
		}
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		if err := json.NewEncoder(w).Encode(map[string]string{"error": "not found"}); err != nil && r.Verbose {
			fmt.Fprintf(os.Stderr, "health encode: %v\n", err)
		}
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
		// Always surface errors here — bind already succeeded so anything
		// reaching this branch is a runtime serve failure (clients dropped,
		// listener torn down) that operators want to see.
		fmt.Fprintf(os.Stderr, "health server error: %v\n", err)
	}
}

// collectMetrics periodically gathers CPU and memory metrics for all running
// processes and broadcasts updated state to connected monitors.
func (r *Runner) collectMetrics(ctx context.Context, store *stateStore, procs []procInfo) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	procMap := make(map[string]procInfo, len(procs))
	for _, p := range procs {
		procMap[p.name] = p
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			running := store.updateMetrics()
			for _, name := range running {
				if p, ok := procMap[name]; ok {
					st := store.get(name)
					if st != nil {
						r.broadcastState(p, st)
					}
				}
			}
		}
	}
}
