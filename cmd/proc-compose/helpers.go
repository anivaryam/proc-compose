package main

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/anivaryam/proc-compose/internal/config"
	"github.com/anivaryam/proc-compose/internal/daemon"
	"github.com/anivaryam/proc-compose/internal/ipc"
	"github.com/anivaryam/proc-compose/internal/paths"
)

// unitNameRe restricts --name to characters safe for both a systemd unit
// filename and the unit file body (no newlines, slashes, or shell metachars).
var unitNameRe = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

func validateUnitName(name string) error {
	if name == "" {
		return fmt.Errorf("--name required")
	}
	if !unitNameRe.MatchString(name) {
		return fmt.Errorf("--name must match [A-Za-z0-9._-]{1,64}, got %q", name)
	}
	return nil
}

// preflightCheck verifies no daemon is already running for this config and
// that the ports required by the merge section are all available.
// A clear, actionable error is returned if anything is wrong.
func preflightCheck(pidPath, socketPath string, cfg *config.Config) error {
	// Is a daemon already running for this config?
	if alive, pid := daemon.IsAliveFromPIDFile(pidPath); alive {
		return fmt.Errorf(
			"proc-compose is already running for this config (PID %d)\n"+
				"  stop it first:  proc-compose stop\n"+
				"  or monitor it:  proc-compose monitor",
			pid,
		)
	}
	// Stale or missing PID file (crashed daemon, reused PID, never started) — clean up.
	if _, err := os.Stat(pidPath); err == nil {
		daemon.Cleanup(pidPath, socketPath)
	}

	if cfg.Merge == nil {
		return nil
	}

	type portRole struct {
		port int
		role string
	}
	check := []portRole{
		{cfg.Merge.Port, "proxy (merge-port)"},
		{cfg.Merge.Client, "client"},
		{cfg.Merge.Server, "server"},
	}

	var busy []string
	for _, pr := range check {
		if pr.port == 0 {
			continue
		}
		if !isPortFree(pr.port) {
			busy = append(busy, fmt.Sprintf("  :%d  (%s)", pr.port, pr.role))
		}
	}

	if len(busy) > 0 {
		msg := "port(s) already in use — stop the conflicting processes first:\n"
		for _, b := range busy {
			msg += b + "\n"
		}
		msg += "\nTip: run  lsof -i :<port>  to find what's using a port."
		return fmt.Errorf("%s", msg)
	}
	return nil
}

func isPortFree(port int) bool {
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		return false
	}
	ln.Close()
	return true
}

func checkOptionalBinaries(cfg *config.Config) {
	if cfg.Merge != nil {
		if _, err := exec.LookPath("merge-port"); err != nil {
			fmt.Fprintf(os.Stderr, "warning: merge-port not found in PATH — merge feature disabled\n")
		}
	}
}

// buildChildArgs constructs the args slice for the daemonized child process.
// It strips --silent/-s, replaces any -f/--file with the resolved absolute
// path, and injects --log-file if not already present.
func buildChildArgs(osArgs []string, absConfigFile, logFile string) []string {
	stripped := daemon.StripFlag(osArgs, "--silent", false)
	stripped = daemon.StripFlag(stripped, "-s", false)

	// Remove any existing -f / --file flags so we can inject the absolute path.
	stripped = daemon.StripFlag(stripped, "-f", true)
	stripped = daemon.StripFlag(stripped, "--file", true)

	// Inject resolved config path.
	out := append([]string{"--file", absConfigFile}, stripped...)

	// Inject --log-file if not already present.
	hasLogFile := false
	for _, a := range out {
		if a == "--log-file" {
			hasLogFile = true
			break
		}
	}
	if !hasLogFile {
		out = append(out, "--log-file", logFile)
	}

	// Tell the child to skip the colourful startup banner — under --silent
	// the banner ends up dumped into the log file with terminal control
	// codes and a misleading "Press Ctrl+C" line that doesn't apply to a
	// detached daemon.
	hasNoBanner := false
	for _, a := range out {
		if a == "--no-banner" {
			hasNoBanner = true
			break
		}
	}
	if !hasNoBanner {
		out = append(out, "--no-banner")
	}

	return out
}

// waitForDaemonReady polls until the daemon is healthy (IPC socket accepts
// connections) or has died. Returns a descriptive error with a log tail if
// the daemon does not come up within timeout, so users see the real failure
// instead of "daemon started" followed by "no daemon found".
func waitForDaemonReady(pid int, socketPath, logPath string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if !daemon.IsAlive(pid) {
			return fmt.Errorf(
				"daemon (PID %d) died during startup\n  Logs: %s\n%s",
				pid, logPath, tailLog(logPath, 30),
			)
		}
		c, err := ipc.Dial(socketPath)
		if err == nil {
			c.Close()
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf(
				"daemon (PID %d) did not become ready within %s\n  Logs: %s\n%s",
				pid, timeout, logPath, tailLog(logPath, 30),
			)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// tailLog returns the last N lines of a log file, or "" if the file is
// missing or empty. Used to show context when the daemon fails to start.
func tailLog(path string, lines int) string {
	data, err := os.ReadFile(path)
	if err != nil || len(data) == 0 {
		return ""
	}
	parts := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(parts) > lines {
		parts = parts[len(parts)-lines:]
	}
	return "  " + strings.Join(parts, "\n  ")
}

func installUnit(unit string, name string, force bool) error {
	homeDir, err := getHomeDir()
	if err != nil {
		return err
	}

	unitPath := filepath.Join(homeDir, ".config", "systemd", "user", fmt.Sprintf("proc-compose-%s.service", name))

	if !force {
		if _, err := os.Stat(unitPath); err == nil {
			return fmt.Errorf("unit already exists at %s\n  Use --force to overwrite", unitPath)
		}
	}

	dir := filepath.Dir(unitPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("cannot create directory %s: %w", dir, err)
	}

	if err := os.WriteFile(unitPath, []byte(unit), 0644); err != nil {
		return fmt.Errorf("cannot write unit file: %w", err)
	}

	cmds := []struct {
		cmd  string
		args []string
		msg  string
	}{
		{"systemctl", []string{"--user", "daemon-reload"}, "daemon-reload"},
		{"systemctl", []string{"--user", "enable", "--now", fmt.Sprintf("proc-compose-%s", name)}, "enable --now"},
	}

	for _, c := range cmds {
		cmd := exec.Command(c.cmd, c.args...)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("failed to %s: %w\n  Hint: run manually:\n    %s %s", c.msg, err, c.cmd, strings.Join(c.args, " "))
		}
	}

	fmt.Printf("Installed and enabled: proc-compose-%s\n", name)
	return nil
}

// sanitizePath replaces /run/user/<uid>/ and similar prefixes with ~
// to avoid exposing system internals in user-facing error messages.
func sanitizePath(path string) string {
	home, err := os.UserHomeDir()
	if err == nil && strings.HasPrefix(path, home) {
		return strings.Replace(path, home, "~", 1)
	}
	// Handle /run/user/<uid>/ paths by extracting uid from path
	if strings.HasPrefix(path, "/run/user/") {
		rest := strings.TrimPrefix(path, "/run/user/")
		if idx := strings.Index(rest, "/"); idx > 0 {
			uid := rest[:idx]
			return strings.Replace(path, "/run/user/"+uid, "~/.proc-compose", 1)
		}
	}
	return path
}

func getHomeDir() (string, error) {
	homeDir := os.Getenv("HOME")
	if homeDir != "" {
		return homeDir, nil
	}
	user, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("HOME not set and cannot determine home directory: %w", err)
	}
	return user, nil
}

// waitReadyProcessNames returns the set of process names whose readiness
// the caller wants to wait on. When the user supplied an explicit positional
// filter (e.g. `proc-compose up frontend backend`) we wait on those plus
// their transitive depends_on, mirroring runner.resolve(). Otherwise we
// wait on every configured process.
func waitReadyProcessNames(cfg *config.Config, filter []string) map[string]struct{} {
	want := make(map[string]struct{})
	if len(filter) == 0 {
		for name := range cfg.Processes {
			want[name] = struct{}{}
		}
		return want
	}
	for _, n := range filter {
		want[n] = struct{}{}
	}
	// Expand transitive depends_on so the set matches what the runner
	// actually started.
	for {
		grew := false
		for name := range want {
			proc, ok := cfg.Processes[name]
			if !ok {
				continue
			}
			for _, dep := range proc.DependsOn {
				if _, seen := want[dep]; !seen {
					want[dep] = struct{}{}
					grew = true
				}
			}
		}
		if !grew {
			break
		}
	}
	return want
}

// waitForProcessesReady streams state events from the daemon's IPC socket
// until every name in `want` has been observed in a "ready" state, or until
// timeout elapses (whichever comes first). Used by `up --silent --wait-ready`
// to give scripts a hard guarantee that probes have passed before the next
// step runs.
func waitForProcessesReady(socketPath string, want map[string]struct{}, timeout time.Duration) error {
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	deadline := time.Now().Add(timeout)

	client, err := ipc.Dial(socketPath)
	if err != nil {
		return fmt.Errorf("wait-ready: cannot dial daemon: %w", err)
	}
	defer client.Close()

	ready := make(map[string]struct{}, len(want))
	failed := make([]string, 0)
	check := func(p ipc.ProcState) bool {
		if _, expected := want[p.Name]; !expected {
			return false
		}
		switch {
		case p.Ready:
			ready[p.Name] = struct{}{}
		case p.State == "failed":
			failed = append(failed, p.Name)
		}
		return true
	}

	allReady := func() bool {
		if len(failed) > 0 {
			return false
		}
		for n := range want {
			if _, ok := ready[n]; !ok {
				return false
			}
		}
		return true
	}

	for {
		// Bound each Recv on the remaining budget so we surface the right
		// timeout error rather than blocking forever on a silent daemon.
		left := time.Until(deadline)
		if left <= 0 {
			return fmt.Errorf("wait-ready: timed out after %s; not ready: %s", timeout, strings.Join(notReadyNames(want, ready), ", "))
		}

		ev, err := client.Recv()
		if err != nil {
			return fmt.Errorf("wait-ready: lost daemon connection: %w", err)
		}
		switch ev.Type {
		case ipc.TypeSnapshot:
			for _, p := range ev.Processes {
				check(p)
			}
		case ipc.TypeState:
			if ev.Proc != nil {
				check(*ev.Proc)
			}
		}
		if len(failed) > 0 {
			return fmt.Errorf("wait-ready: process(es) failed: %s", strings.Join(failed, ", "))
		}
		if allReady() {
			return nil
		}
	}
}

func notReadyNames(want map[string]struct{}, ready map[string]struct{}) []string {
	out := make([]string, 0, len(want))
	for n := range want {
		if _, ok := ready[n]; !ok {
			out = append(out, n)
		}
	}
	// Stable order for the error message.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

func printStatusTable(procs []ipc.ProcState, tunnelURL string) {
	if len(procs) == 0 {
		fmt.Println("(no processes registered)")
		return
	}

	sortProcs(procs)

	maxName := len("PROCESS")
	for _, p := range procs {
		if len(p.Name) > maxName {
			maxName = len(p.Name)
		}
	}

	if tunnelURL != "" {
		fmt.Printf("public: %s\n", tunnelURL)
	}
	fmt.Printf("%-*s  %-7s  %-11s  %-8s  %-7s  %s\n",
		maxName, "PROCESS", "MODE", "STATE", "RESTARTS", "PID", "UPTIME")
	for _, p := range procs {
		uptime := "-"
		if !p.StartedAt.IsZero() && (p.State == "running" || p.State == "restarting") {
			uptime = formatStatusDuration(time.Since(p.StartedAt))
		}
		pid := "-"
		if p.PID > 0 && p.State != "completed" && p.State != "exited" && p.State != "failed" {
			pid = fmt.Sprintf("%d", p.PID)
		}
		mode := p.Mode
		if mode == "" {
			mode = "service"
		}
		fmt.Printf("%-*s  %-7s  %-11s  %-8d  %-7s  %s\n",
			maxName, p.Name, mode, p.State, p.Restarts, pid, uptime)
	}
}

// printStatusJSON emits a stable machine-readable shape that scripts can
// consume without parsing the table. Wraps processes + tunnel into a single
// object so callers get one JSON document, not a stream.
func printStatusJSON(procs []ipc.ProcState, tunnelURL string) error {
	sortProcs(procs)
	out := map[string]any{
		"processes": procs,
	}
	if tunnelURL != "" {
		out["tunnel_url"] = tunnelURL
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

// sortProcs orders procs alphabetically by name. Mutates in place.
func sortProcs(procs []ipc.ProcState) {
	for i := 1; i < len(procs); i++ {
		for j := i; j > 0 && procs[j].Name < procs[j-1].Name; j-- {
			procs[j], procs[j-1] = procs[j-1], procs[j]
		}
	}
}

// formatStatusDuration mirrors the monitor's uptime style (1h2m3s).
func formatStatusDuration(d time.Duration) string {
	d = d.Round(time.Second)
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	s := int(d.Seconds()) % 60
	if h > 0 {
		return fmt.Sprintf("%dh%dm%ds", h, m, s)
	}
	if m > 0 {
		return fmt.Sprintf("%dm%ds", m, s)
	}
	return fmt.Sprintf("%ds", s)
}

// starterTemplate returns the YAML body for a named init template. Names
// are case-insensitive and aliases are accepted ("js"/"javascript" → node).
// The minimal template is intentionally language-agnostic so a fresh user
// can run `proc-compose up` immediately and see something work.
func starterTemplate(name string) (string, error) {
	switch strings.ToLower(name) {
	case "", "minimal", "default":
		return `# proc-compose minimal starter — replace with your real commands.
# Run "proc-compose up" to start everything; Ctrl+C stops cleanly.

processes:
  hello:
    cmd: echo "hello from proc-compose" && sleep 5

  ticker:
    cmd: sh -c 'while true; do date; sleep 2; done'
    restart: always
`, nil
	case "node", "js", "javascript", "ts", "typescript":
		return `# proc-compose Node.js full-stack starter.
# merge-port (https://github.com/anivaryam/merge-port) combines the client
# and server into one URL so you can develop on a single port.

merge:
  client: 5173
  server: 3001
  port: 8080

processes:
  client:
    cmd: npm run dev
    dir: ./client
    env:
      PORT: "5173"
  server:
    cmd: npm run dev
    dir: ./server
    env:
      PORT: "3001"
    restart: on-failure
    ready_when:
      http: http://localhost:3001/health
`, nil
	case "go", "golang":
		return `# proc-compose Go microservices starter.
processes:
  gateway:
    cmd: go run ./cmd/gateway
    env:
      PORT: "8080"
    restart: on-failure
    ready_when:
      tcp: localhost:8080
  worker:
    cmd: go run ./cmd/worker
    restart: always
    depends_on:
      - gateway
`, nil
	case "python", "py":
		return `# proc-compose Python starter.
processes:
  web:
    cmd: python -m uvicorn app.main:app --port 8000 --reload
    env:
      PORT: "8000"
    restart: on-failure
    ready_when:
      http: http://localhost:8000/health
  worker:
    cmd: python -m app.worker
    restart: always
    depends_on:
      - web
`, nil
	default:
		return "", fmt.Errorf("unknown template %q; valid: minimal, node, go, python", name)
	}
}

// candidateRuntimeDirs returns ordered runtime dirs to probe when locating
// a live daemon. Lets the read-side commands (monitor/stop/restart/reload/
// status) find a daemon whose $XDG_RUNTIME_DIR at launch differs from the
// current shell's. Order matches paths.runtimeDir()'s preference, then
// adds /run/user/<uid> for the case where XDG is unset in this shell but
// was set when the daemon launched, and finally os.TempDir() as last
// resort. Duplicates are filtered to keep probing cheap.
func candidateRuntimeDirs() []string {
	seen := map[string]bool{}
	var dirs []string
	add := func(d string) {
		if d == "" || seen[d] {
			return
		}
		seen[d] = true
		dirs = append(dirs, d)
	}
	add(os.Getenv("XDG_RUNTIME_DIR"))
	if cache, err := os.UserCacheDir(); err == nil {
		add(filepath.Join(cache, "proc-compose"))
	}
	if runtime.GOOS == "linux" {
		add(fmt.Sprintf("/run/user/%d", os.Getuid()))
	}
	add(os.TempDir())
	return dirs
}

// liveDaemonPaths locates the PID and socket path of a daemon running for
// hash, probing every candidate runtime dir for a live PID file. The
// recorded socket addr from that PID file is returned — that's the path
// the daemon actually bound to, so dialing works even when the current
// shell's $XDG_RUNTIME_DIR differs from the daemon's. Falls back to the
// env-derived defaults when no live daemon is found, so the caller can
// surface the usual "no daemon running" error.
//
// Windows named pipes live in a global namespace, so the env-derived
// paths are always correct there.
func liveDaemonPaths(hash string) (pidPath, socketPath string) {
	defaultPID := paths.PID(hash)
	defaultSock := paths.Socket(hash)
	if runtime.GOOS == "windows" {
		return defaultPID, defaultSock
	}
	for _, d := range candidateRuntimeDirs() {
		pp := filepath.Join(d, "pc-"+hash+".pid")
		if _, err := os.Stat(pp); err != nil {
			continue
		}
		alive, _ := daemon.IsAliveFromPIDFile(pp)
		if !alive {
			continue
		}
		_, addr, err := daemon.ReadPID(pp)
		if err != nil || addr == "" {
			continue
		}
		return pp, addr
	}
	return defaultPID, defaultSock
}

// liveLogPath locates the log file for hash across candidate runtime dirs.
// Prefers the log living next to a live PID file, so `proc-compose logs`
// follows whatever runtime dir the daemon actually used. Falls back to
// any candidate dir that has the log, then to the env-derived default.
func liveLogPath(hash string) string {
	candidates := candidateRuntimeDirs()
	for _, d := range candidates {
		pp := filepath.Join(d, "pc-"+hash+".pid")
		if _, err := os.Stat(pp); err != nil {
			continue
		}
		if alive, _ := daemon.IsAliveFromPIDFile(pp); !alive {
			continue
		}
		lp := filepath.Join(d, "pc-"+hash+".log")
		if _, err := os.Stat(lp); err == nil {
			return lp
		}
	}
	for _, d := range candidates {
		lp := filepath.Join(d, "pc-"+hash+".log")
		if _, err := os.Stat(lp); err == nil {
			return lp
		}
	}
	return paths.Log(hash)
}

// resolveLiveDaemonPaths is the read-side counterpart to resolveConfigPaths.
// It resolves the absolute config path and hash the same way, but returns
// the PID/socket paths of an already-running daemon (probed across
// candidate runtime dirs) rather than the env-derived write paths.
func resolveLiveDaemonPaths(configFile string) (absConfig, hash, socketPath, pidPath string, err error) {
	configFile = resolveConfigExtension(configFile)
	absConfig, err = filepath.Abs(configFile)
	if err != nil {
		return "", "", "", "", err
	}
	hash, err = paths.FromConfig(absConfig)
	if err != nil {
		return "", "", "", "", err
	}
	pidPath, socketPath = liveDaemonPaths(hash)
	return absConfig, hash, socketPath, pidPath, nil
}
