// Package runner manages the lifecycle of configured processes: starting, restarting, and stopping.
package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
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

	store *stateStore // set during Run for command dispatch

	// cfgMu guards concurrent access to Config.Processes between Reload (writer)
	// and the per-process restart loop (reader). Run-time startup and
	// ListProcesses are single-threaded relative to Reload and need no lock.
	cfgMu sync.RWMutex
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
	r.store = store

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
			if r.runProcess(ctx, p, maxName, store) {
				failedMu.Lock()
				failedNames = append(failedNames, p.name)
				failedMu.Unlock()
			}
		}(p)
	}

	wg.Wait()

	// ctx.Err() != nil means a signal triggered shutdown — not a failure.
	if ctx.Err() == nil && len(failedNames) > 0 {
		return fmt.Errorf("processes failed: %s", strings.Join(failedNames, ", "))
	}
	return nil
}

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
		switch cmd.Action {
		case "restart":
			if cmd.Process == "" {
				res = ipc.CommandResult{Status: "error", Message: "restart requires a process name"}
				break
			}
			r.cfgMu.RLock()
			_, known := r.Config.Processes[cmd.Process]
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
			st.requestRestart()
			res = ipc.CommandResult{Status: "ok"}
		case "reload":
			res = r.Reload()
		default:
			res = ipc.CommandResult{Status: "error", Message: "unknown action: " + cmd.Action}
		}
		if cmd.Reply != nil {
			cmd.Reply <- res
		}
	}
}

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
			if p.State != "running" && p.State != "restarting" {
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
