package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
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

func validateSurvivePlatform(goos string) error {
	if goos != "linux" {
		return fmt.Errorf("--survive requires Linux with systemd user services")
	}
	return nil
}

func validateUninstallPlatform(goos string) error {
	if goos != "linux" {
		return fmt.Errorf("uninstall is only supported on Linux with systemd user services")
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

	var busy []string
	for _, pr := range mergePreflightPorts(cfg) {
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

type mergePortRole struct {
	port int
	role string
}

func mergePreflightPorts(cfg *config.Config) []mergePortRole {
	if cfg == nil || cfg.Merge == nil {
		return nil
	}

	checks := []mergePortRole{{port: cfg.Merge.Port, role: "proxy (merge-port output)"}}
	checks = append(checks, managedMergeUpstreamPorts(cfg)...)
	return checks
}

func managedMergeUpstreamPorts(cfg *config.Config) []mergePortRole {
	if cfg == nil || cfg.Merge == nil {
		return nil
	}

	portsByProcess := processPorts(cfg.Processes)
	var targets []int
	if len(cfg.Merge.Routes) > 0 {
		for _, route := range cfg.Merge.Routes {
			if port := routeTargetPort(route); port > 0 {
				targets = append(targets, port)
			}
		}
	} else {
		if cfg.Merge.Client > 0 {
			targets = append(targets, cfg.Merge.Client)
		}
		if cfg.Merge.Server > 0 {
			targets = append(targets, cfg.Merge.Server)
		}
	}

	seen := make(map[string]bool)
	var checks []mergePortRole
	for _, target := range targets {
		owners := portsByProcess[target]
		sort.Strings(owners)
		for _, owner := range owners {
			key := fmt.Sprintf("%d/%s", target, owner)
			if seen[key] {
				continue
			}
			seen[key] = true
			checks = append(checks, mergePortRole{port: target, role: "managed upstream " + owner})
		}
	}
	return checks
}

func processPorts(processes map[string]config.Process) map[int][]string {
	out := make(map[int][]string)
	for name, proc := range processes {
		if name == "merge-port" {
			continue
		}
		value := ""
		if proc.Env != nil {
			value = proc.Env["PORT"]
		}
		port, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil || port <= 0 {
			continue
		}
		out[port] = append(out[port], name)
	}
	return out
}

func routeTargetPort(route string) int {
	idx := strings.LastIndex(route, "=")
	if idx < 0 {
		return 0
	}
	port, err := strconv.Atoi(strings.TrimSpace(route[idx+1:]))
	if err != nil || port <= 0 {
		return 0
	}
	return port
}

func isPortFree(port int) bool {
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		return false
	}
	ln.Close()
	return true
}

var mergePortInPath = func() bool {
	_, err := exec.LookPath("merge-port")
	return err == nil
}

func checkOptionalBinaries(cfg *config.Config) error {
	if cfg.Merge != nil {
		if !mergePortInPath() {
			return fmt.Errorf(
				"merge-port is required for merge: config but was not found in PATH. " +
					"Install it with brokit install merge-port, " +
					"install from https://github.com/anivaryam/merge-port, " +
					"or remove the merge: section.",
			)
		}
	}
	return nil
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

// stopOutcome is what the daemon told us about its own shutdown.
type stopOutcome int

const (
	// stopVerified means the daemon confirmed every managed process group was
	// emptied and nothing escaped verification.
	stopVerified stopOutcome = iota
	// stopUnverified means the daemon reported that termination could not be
	// fully established, or that it never reported at all.
	stopUnverified
	// stopUnavailable means the daemon could not be asked over IPC, so nothing
	// is known about its managed processes.
	stopUnavailable
)

// stopResult carries the outcome plus the daemon's own explanation, which is the
// only account of what could not be terminated.
type stopResult struct {
	outcome stopOutcome
	detail  string
}

// stopBudget fractions. The whole stop shares one deadline; these decide how
// much of it each phase may consume so no single phase can starve the others.
const (
	// connectBudget caps Dial, which has its own internal bound but would
	// otherwise be unbounded from the caller's point of view.
	connectBudget = 2 * time.Second
	// exitBudget is reserved for confirming the daemon process itself is gone
	// after a successful verdict.
	exitBudget = 2 * time.Second
)

// stopDaemon stops the daemon for this config and returns a verified result.
//
// force asks the daemon to force-terminate its managed process groups instead of
// signalling them first. Both variants go through the daemon's own shutdown so
// the verdict comes from the only process that knows which groups it manages;
// observing the daemon merely disappearing would prove nothing about them.
//
// The whole operation shares one deadline. Connecting, waiting for the verdict
// and confirming the daemon exited all draw from it, so `stop` cannot block
// past --timeout even against a connected but unresponsive daemon.
func stopDaemon(socketPath, pidPath string, pid int, proc *os.Process, force bool, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)

	res := requestShutdown(socketPath, force, deadline)
	switch res.outcome {
	case stopVerified:
		// A verified verdict still requires the daemon to actually finish
		// exiting, within whatever is left of the budget.
		if waitProcessGone(pid, remainingUntil(deadline, exitBudget)) {
			daemon.Cleanup(pidPath, socketPath)
			fmt.Printf("stopped proc-compose daemon (PID %d)\n", pid)
			return nil
		}
		return fmt.Errorf("daemon (PID %d) reported a clean shutdown but is still running %s "+
			"after its verdict; managed processes were not re-checked", pid, timeout)

	case stopUnverified:
		// Finish the job, but never claim the tree is gone. The verdict detail is
		// the only account of what could not be terminated, so it leads the
		// message rather than being buried in it.
		if waitProcessGone(pid, remainingUntil(deadline, exitBudget)) {
			daemon.Cleanup(pidPath, socketPath)
			return fmt.Errorf("stopped daemon (PID %d), but termination was NOT fully verified:\n  %s", pid, res.detail)
		}
		return fmt.Errorf("daemon (PID %d) is still running %s after reporting that termination was "+
			"NOT fully verified:\n  %s", pid, timeout, res.detail)

	default:
		// No usable verdict. Signal the daemon directly so the user still gets
		// a stopped daemon, and label the outcome for what it is.
		fmt.Fprintf(os.Stderr,
			"warning: could not obtain a shutdown verdict from the daemon (%s);\n"+
				"         signalling the daemon directly — its managed processes are NOT verified.\n", res.detail)
		return signalDaemonUnverified(pidPath, socketPath, pid, proc, force, deadline)
	}
}

// requestShutdown asks the daemon to stop itself and waits, under deadline, for
// the verdict.
//
// RecvTimeout bounds the read and is terminal on timeout (bufio.Scanner latches
// the error), so the client is closed on every non-clean exit and no read is
// left blocking on a connection that has gone quiet.
func requestShutdown(socketPath string, force bool, deadline time.Time) stopResult {
	connectBy := remainingUntil(deadline, connectBudget)
	if connectBy <= 0 {
		return stopResult{outcome: stopUnavailable, detail: "no time left in the stop budget to reach the daemon"}
	}
	client, err := ipc.DialTimeout(socketPath, connectBy)
	if err != nil {
		return stopResult{outcome: stopUnavailable, detail: err.Error()}
	}
	// Closed exactly once on every path; RecvTimeout leaves the stream
	// unusable, so the caller must not keep reading it.
	defer client.Close()

	// The write shares the overall deadline. Without this a wedged daemon whose
	// receive buffer is full would block the request past --timeout.
	//
	// The budget is checked before the write, because remainingUntil reports
	// zero once the deadline has passed and a zero budget would otherwise be
	// spent on an unbounded write.
	writeBy := remainingUntil(deadline, connectBudget)
	if writeBy <= 0 {
		return stopResult{outcome: stopUnavailable,
			detail: "no time left in the stop budget to send the shutdown request"}
	}
	if err := client.SendTimeout(ipc.Command{Action: "shutdown", Force: force}, writeBy); err != nil {
		return stopResult{outcome: stopUnavailable, detail: err.Error()}
	}

	for {
		budget := time.Until(deadline)
		if budget <= 0 {
			return stopResult{outcome: stopUnverified,
				detail: "no verdict arrived within the stop budget"}
		}
		ev, recvErr := client.RecvTimeout(budget)
		if recvErr != nil {
			// A connected but silent daemon, or a dropped connection, must not
			// be read as success.
			return stopResult{outcome: stopUnverified,
				detail: fmt.Sprintf("daemon did not report a shutdown verdict (%v)", recvErr)}
		}
		if ev.Type != ipc.TypeAck {
			continue
		}
		switch ev.Ack {
		case "ok":
			return stopResult{outcome: stopVerified}
		case "partial":
			return stopResult{outcome: stopUnverified, detail: ev.AckDetail}
		case "busy":
			return stopResult{outcome: stopUnavailable, detail: "daemon is busy: " + ev.AckDetail}
		default:
			// Includes an older daemon rejecting the unknown action, and any
			// daemon-side error. Either way there is no verdict to rely on.
			return stopResult{outcome: stopUnavailable,
				detail: fmt.Sprintf("daemon rejected the shutdown request: %s", ev.AckDetail)}
		}
	}
}

// signalDaemonUnverified is the compatibility fallback for daemons that cannot
// be asked to stop themselves — an older proc-compose, or a dead socket. It gets
// the daemon down and says plainly that its managed processes are unknown.
//
// force is preserved here rather than collapsed into the graceful path: `--force`
// means "terminate now", and silently degrading it into SIGTERM-then-wait would
// make an unresponsive daemon take the full timeout to stop even though the user
// asked for it to be killed.
func signalDaemonUnverified(pidPath, socketPath string, pid int, proc *os.Process, force bool, deadline time.Time) error {
	if force {
		if err := daemon.KillProcess(proc); err != nil {
			return fmt.Errorf("failed to SIGKILL %d: %w", pid, err)
		}
		if waitProcessGone(pid, remainingUntil(deadline, exitBudget)) {
			daemon.Cleanup(pidPath, socketPath)
			return fmt.Errorf("killed daemon (PID %d) without asking it to stop, but its managed "+
				"processes were NOT verified as terminated;\n  inspect for leftovers: ps -eo pid,ppid,pgid,args", pid)
		}
		return fmt.Errorf("daemon (PID %d) survived SIGKILL", pid)
	}

	if err := daemon.StopProcess(proc); err != nil {
		return fmt.Errorf("failed to stop process %d: %w", pid, err)
	}
	if waitProcessGone(pid, time.Until(deadline)) {
		daemon.Cleanup(pidPath, socketPath)
		return fmt.Errorf("stopped daemon (PID %d) by signalling it directly, but its managed "+
			"processes were NOT verified as terminated;\n  inspect for leftovers: ps -eo pid,ppid,pgid,args", pid)
	}

	if err := daemon.KillProcess(proc); err != nil {
		return fmt.Errorf("failed to SIGKILL %d after the graceful period: %w", pid, err)
	}
	if waitProcessGone(pid, killSettleTimeout) {
		daemon.Cleanup(pidPath, socketPath)
		return fmt.Errorf("killed daemon (PID %d) after it ignored the graceful stop, but its managed "+
			"processes were NOT verified as terminated;\n  inspect for leftovers: ps -eo pid,ppid,pgid,args", pid)
	}
	return fmt.Errorf("daemon (PID %d) survived SIGKILL", pid)
}

// killSettleTimeout bounds the wait for a SIGKILL to be observed. SIGKILL is
// not catchable, so this only ever covers scheduling delay. It is deliberately
// outside the caller's deadline: a SIGKILL cannot be declined, so waiting for it
// to land is bounded bookkeeping rather than an unbounded request.
const killSettleTimeout = 5 * time.Second

// remainingUntil returns the time left before deadline, capped at cap and
// floored at zero. Every phase of a stop draws from one deadline through this,
// so no single phase can consume the whole budget.
func remainingUntil(deadline time.Time, cap time.Duration) time.Duration {
	left := time.Until(deadline)
	if left <= 0 {
		return 0
	}
	if left > cap {
		return cap
	}
	return left
}

// waitProcessGone polls until pid is no longer running or timeout elapses.
func waitProcessGone(pid int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if !daemon.IsAlive(pid) {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		time.Sleep(50 * time.Millisecond)
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

	// satisfied reports whether a state event should count towards the
	// waiter. Readiness is a per-cycle latch, so it is only honoured while
	// the process is in a state that can serve traffic: a service must be
	// running, a task must have completed. Any other observation (starting,
	// restarting, exited, failed) retracts an earlier success so a stale
	// ready=true can't satisfy the waiter.
	satisfied := func(p ipc.ProcState) bool {
		switch p.State {
		case "completed":
			return true // the runner only reaches "completed" on task success
		case "running":
			return p.Ready
		default:
			return false
		}
	}

	// check folds one state observation into ready/failed. Failure wins over
	// readiness regardless of order, so an earlier ready=true can never hide
	// a later failure.
	check := func(p ipc.ProcState) {
		if _, expected := want[p.Name]; !expected {
			return
		}
		if p.State == "failed" {
			failed = append(failed, p.Name)
			delete(ready, p.Name)
			return
		}
		if satisfied(p) {
			ready[p.Name] = struct{}{}
		} else {
			delete(ready, p.Name)
		}
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

		ev, err := client.RecvTimeout(left)
		if err != nil {
			if errors.Is(err, ipc.ErrRecvTimeout) {
				return fmt.Errorf("wait-ready: timed out after %s; not ready: %s", timeout, strings.Join(notReadyNames(want, ready), ", "))
			}
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

// starterTemplateForPlatform is the platform-aware version of starterTemplate.
// It delegates to starterTemplate for non-Windows platforms and for all
// non-minimal templates. On Windows with minimal aliases ("", "minimal", "default"),
// it returns a template using PowerShell commands that are native to Windows.
func starterTemplateForPlatform(name, goos string) (string, error) {
	if (name == "" || strings.ToLower(name) == "minimal" || strings.ToLower(name) == "default") && goos == "windows" {
		return `# proc-compose minimal starter — replace with your real commands.
# Run "proc-compose up" to start everything; Ctrl+C stops cleanly.

processes:
  hello:
    cmd: powershell -NoProfile -Command "Write-Host 'hello from proc-compose'; Start-Sleep -Seconds 5"

  ticker:
    cmd: powershell -NoProfile -Command "while ($true) { Get-Date; Start-Sleep -Seconds 2 }"
    restart: always
`, nil
	}
	return starterTemplate(name)
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
