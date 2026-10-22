package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/anivaryam/proc-compose/internal/ansi"
	"github.com/anivaryam/proc-compose/internal/config"
	"github.com/anivaryam/proc-compose/internal/daemon"
	"github.com/anivaryam/proc-compose/internal/ipc"
	"github.com/anivaryam/proc-compose/internal/paths"
	"github.com/anivaryam/proc-compose/internal/runner"
	"github.com/anivaryam/proc-compose/internal/session"
	"github.com/anivaryam/proc-compose/internal/update"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"golang.org/x/term"
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

// preflightCheck verifies no daemon is already running for this config, that
// no crashed run left process groups that may still be running, and that the
// ports required by the merge section are all available.
// A clear, actionable error is returned if anything is wrong.
//
// It runs without the session lease, as an early answer for the user, so it
// changes nothing: stale state is removed only by acquireRun, under the lease.
// Removing it here could delete the PID file of another `up` that took the
// lease and wrote its file while this check ran.
func preflightCheck(pidPath string, cfg *config.Config) error {
	// A live daemon, or a crashed one whose recorded groups may still run,
	// refuses the start.
	if err := runner.CheckCrashedRun(pidPath); err != nil {
		return err
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

// acquireRun takes the session for `up` and then, under its instance lease,
// runs the authoritative crashed-run check: a crashed run's record is never
// overwritten while its groups may run, and stale state is removed. The check
// lives here so that no caller can hold the plan without having passed it. On
// refusal the plan is closed and the lease released.
func acquireRun(cfg *config.Config, absConfig, hash, sessionName string, args []string, pidPath, socketPath string) (*session.Plan, error) {
	plan, err := session.Acquire(cfg, absConfig, hash, sessionName, args)
	if err != nil {
		return nil, err
	}
	if err := runner.ResolveCrashedRun(pidPath, socketPath); err != nil {
		plan.Close()
		return nil, err
	}
	return plan, nil
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
func buildChildArgs(osArgs []string, absConfigFile, logFile string, parsed ...*cobra.Command) []string {
	if len(parsed) > 0 {
		args := []string{"up"}
		parsed[0].Flags().Visit(func(flag *pflag.Flag) {
			switch flag.Name {
			case "silent", "json", "wait-ready", "wait-timeout", "file", "session", "daemon-child", "no-banner":
				return
			}
			if flag.Value.Type() == "bool" {
				args = append(args, "--"+flag.Name+"="+flag.Value.String())
			} else {
				args = append(args, "--"+flag.Name, flag.Value.String())
			}
		})
		if sessionName != "" {
			args = append(args, "--session", sessionName)
		}
		osArgs = append(args, osArgs...)
	}
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
	out = append(out, "--daemon-child")

	return out
}

// waitForDaemonReady polls until the daemon is healthy (IPC socket accepts
// connections) or has died. Returns a descriptive error with a log tail if
// the daemon does not come up within timeout, so users see the real failure
// instead of "daemon started" followed by "no daemon found".
func waitForDaemonReady(pid int, socketPath, logPath string, timeout time.Duration, pidPath ...string) error {
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
			if len(pidPath) == 0 {
				return nil
			}
			owner, address, readErr := daemon.ReadPID(pidPath[0])
			if readErr == nil {
				if owner != pid || address != socketPath {
					return fmt.Errorf("startup lost session ownership to another daemon; this invocation did not start that session")
				}
				return nil
			}
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
			if teardownOnly(res.detail) {
				return fmt.Errorf("stopped daemon (PID %d); termination was verified, but teardown did not pass:\n  %s",
					pid, strings.ReplaceAll(res.detail, "\n", "\n  "))
			}
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

// teardownOnly reports whether every line of a shutdown verdict reports a
// teardown process: the managed processes were verified terminated.
func teardownOnly(detail string) bool {
	if detail == "" {
		return false
	}
	for _, line := range strings.Split(detail, "\n") {
		if !strings.HasPrefix(line, ipc.TeardownPrefix) {
			return false
		}
	}
	return true
}

// stopBudget is the stop budget: timeout, plus the teardown budget when
// timeout was not set explicitly and the session metadata at metaPath
// declares teardown processes, which run after termination is verified.
func stopBudget(metaPath string, timeout time.Duration, explicit bool) time.Duration {
	if explicit {
		return timeout
	}
	if project, err := session.Read(metaPath); err == nil && len(project.Teardown) > 0 {
		return timeout + runner.TeardownBudget
	}
	return timeout
}

// teardownNotRun is the note a crash report carries when the session
// declared teardown processes: the daemon died before it could run them. It
// is empty when none was declared.
func teardownNotRun(hash string) string {
	project, err := session.Read(paths.Metadata(hash))
	if err != nil || len(project.Teardown) == 0 {
		return ""
	}
	return "teardown did not run: " + strings.Join(project.Teardown, ", ")
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

// stopDeadDaemon handles a PID file whose daemon is gone — it crashed or was
// killed before it could run its shutdown. pid is 0 when the PID file could not
// be parsed.
//
// On Unix the PID file records the process groups the daemon launched. Under
// the session's instance lease, each recorded group is proven empty, proven
// ours and terminated (Linux only), or reported unknown; see
// runner.RecoverGroups. Success, which removes the PID file, needs every group
// proven empty. Without a record (an older daemon, or recording disabled) or
// with an unreadable file nothing can be proven, so this fails and keeps the
// PID file. Force then clears a readable file without a record with a warning,
// as before records existed; an unreadable file may have held a record, so
// force clears it but still fails. With a record, force SIGKILLs proven groups
// instead of waiting, and a group still unproven clears the state but fails
// with the evidence.
//
// Windows is the exception: every managed tree runs in a kill-on-close Job
// Object whose handle only the daemon held, so the kernel ended them with it.
//
// On every platform, a session that declared teardown processes is told they
// did not run: a daemon that died never reached its teardown phase.
func stopDeadDaemon(pidPath, socketPath, hash string, pid int, force bool, timeout time.Duration) error {
	label := fmt.Sprintf("daemon (PID %d)", pid)
	if pid == 0 {
		label = "daemon (unreadable PID file)"
	}
	notRun := teardownNotRun(hash)
	// withNote adds the teardown note to a crash verdict; a success prints it
	// as a warning instead.
	withNote := func(err error) error {
		if notRun == "" {
			return err
		}
		if err == nil {
			fmt.Fprintf(os.Stderr, "warning: %s\n", notRun)
			return nil
		}
		return fmt.Errorf("%w\n  %s", err, notRun)
	}
	if runtime.GOOS == "windows" {
		daemon.Cleanup(pidPath, socketPath)
		fmt.Printf("%s is no longer running; its managed processes ended with its job objects\n", label)
		return withNote(nil)
	}
	release, err := session.LeaseInstance(hash)
	if err != nil {
		var lease *session.LeaseError
		if errors.As(err, &lease) {
			return fmt.Errorf("another proc-compose command owns this session; nothing was signalled.\n" +
				"  rerun stop once it has finished")
		}
		return fmt.Errorf("cannot take the session lease, so nothing was signalled: %w", err)
	}
	defer release()

	// The PID file may have changed hands before the lease was taken.
	if alive, livePID := daemon.IsAliveFromPIDFile(pidPath); alive {
		return fmt.Errorf("a daemon (PID %d) is now running for this config; rerun stop", livePID)
	}
	rec, err := daemon.ReadRecord(pidPath)
	if errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("no running proc-compose found for this config (tried %s)", sanitizePath(pidPath))
	}
	if err != nil || rec.Groups == nil {
		return withNote(stopUnrecordedDeadDaemon(pidPath, socketPath, label, force, err != nil))
	}

	mode := runner.RecoverGraceful
	if force {
		mode = runner.RecoverForce
	}
	results := runner.RecoverGroups(*rec.Groups, rec.Host, mode, time.Now().Add(timeout))
	evidence := runner.FormatRecovery(results)
	verified := true
	for _, r := range results {
		if r.Outcome != runner.OutcomeEmpty && r.Outcome != runner.OutcomeTerminated {
			verified = false
		}
	}
	if verified {
		daemon.Cleanup(pidPath, socketPath)
		if evidence != "" {
			fmt.Println(evidence)
		}
		fmt.Printf("%s had exited without a shutdown verdict; every recorded process group is verified empty\n", label)
		return withNote(nil)
	}
	if !force {
		return withNote(fmt.Errorf("%s exited without a shutdown verdict, and some of its process groups are NOT verified\n"+
			"  as terminated and may still be running:\n%s\n"+
			"  inspect for leftovers: ps -eo pid,ppid,pgid,args\n"+
			"  then clear the stale daemon state: proc-compose stop --force", label, indentLines(evidence)))
	}
	daemon.Cleanup(pidPath, socketPath)
	return withNote(fmt.Errorf("%s exited without a shutdown verdict; cleared the stale daemon state, but some of its\n"+
		"  process groups are NOT verified as terminated and may still be running:\n%s\n"+
		"  inspect for leftovers: ps -eo pid,ppid,pgid,args", label, indentLines(evidence)))
}

// stopUnrecordedDeadDaemon is the crash path for a PID file that names no
// process groups or cannot be read: nothing can be proven, so without force it
// fails and keeps the file. Force clears the stale state; for a readable legacy
// file it only warns, as before records existed, but an unreadable file may
// have held a group record, so its groups are unknown and force still fails.
func stopUnrecordedDeadDaemon(pidPath, socketPath, label string, force, unreadable bool) error {
	if !force {
		return fmt.Errorf("%s is no longer running and exited without a shutdown verdict;\n"+
			"  its managed processes were NOT verified as terminated and may still be running.\n"+
			"  inspect for leftovers: ps -eo pid,ppid,pgid,args\n"+
			"  then clear the stale daemon state: proc-compose stop --force", label)
	}
	daemon.Cleanup(pidPath, socketPath)
	if unreadable {
		return fmt.Errorf("%s exited without a shutdown verdict; cleared the stale daemon state, but its\n"+
			"  managed processes are NOT verified as terminated and may still be running.\n"+
			"  inspect for leftovers: ps -eo pid,ppid,pgid,args", label)
	}
	fmt.Fprintf(os.Stderr,
		"warning: %s exited without a shutdown verdict; its managed processes were NOT verified\n"+
			"         as terminated. Cleared the stale daemon state.\n"+
			"         inspect for leftovers: ps -eo pid,ppid,pgid,args\n", label)
	return nil
}

// indentLines indents each line of s for inclusion in a multi-line message.
func indentLines(s string) string {
	return "    " + strings.ReplaceAll(s, "\n", "\n    ")
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
	for _, n := range config.SelectedNames(cfg, filter) {
		want[n] = struct{}{}
	}
	return want
}

// waitForProcessesReady streams state events from the daemon's IPC socket
// until every name in `want` has been observed in a "ready" state, or until
// timeout elapses (whichever comes first). Used by `up --silent --wait-ready`
// to give scripts a hard guarantee that probes have passed before the next
// step runs.
func waitForProcessesReady(socketPath string, want map[string]struct{}, timeout time.Duration, minimum ...map[string]uint64) error {
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
		if len(minimum) > 0 && p.Generation < minimum[0][p.Name] {
			return false
		}
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
		if len(minimum) > 0 && p.Generation < minimum[0][p.Name] {
			delete(ready, p.Name)
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
func printStatusJSON(procs []ipc.ProcState, tunnelURL string, project ...*ipc.Project) error {
	sortProcs(procs)
	out := map[string]any{
		"processes": procs,
	}
	if tunnelURL != "" {
		out["tunnel_url"] = tunnelURL
	}
	if len(project) > 0 && project[0] != nil {
		out["project"] = project[0]
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
	hash, err = paths.FromConfig(absConfig, sessionName)
	if err != nil {
		return "", "", "", "", err
	}
	pidPath, socketPath = liveDaemonPaths(hash)
	return absConfig, hash, socketPath, pidPath, nil
}

// ── update notice ────────────────────────────────────────────────────────────
//
// A "new release available" hint is guidance for a person watching a terminal,
// so it is limited to commands whose output is human-facing. Cobra returns for
// --help and --version before running any hook, so those never get here; the
// rest of the machine-readable surfaces are listed in updateNoticeSkip.

var updateNoticeSkip = map[string]bool{
	"agent-guide":      true,
	"help":             true,
	"completion":       true,
	"__complete":       true,
	"__completeNoDesc": true,
	"man":              true,
}

// topLevelName returns the name of the command's direct child of the root.
// "completion bash" is executed as the `bash` subcommand, so matching on the
// executed command's own name would miss the excluded `completion` parent.
func topLevelName(cmd *cobra.Command) string {
	if cmd.HasParent() && cmd.Parent().HasParent() {
		return topLevelName(cmd.Parent())
	}
	return cmd.Name()
}

// updateNoticeEligible reports whether this invocation may print an update
// notice.
//
// interactive must report whether stderr is a terminal: the notice is
// diagnostics, so stderr decides. A command whose stdout is redirected to a
// file or a pipe still belongs to someone watching the screen, while a cron
// job or CI step writing stderr to a log gets nothing.
func updateNoticeEligible(cmd *cobra.Command, interactive bool) bool {
	if cmd == nil || !interactive {
		return false
	}
	name := topLevelName(cmd)
	if updateNoticeSkip[name] {
		return false
	}
	// --json is this CLI's marker for "this output is for a machine".
	if f := cmd.Flags().Lookup("json"); f != nil {
		if machine, err := cmd.Flags().GetBool("json"); err == nil && machine {
			return false
		}
	}
	// "up --survive" prints a systemd unit that callers write to a file, and
	// "up --silent" hands off to a daemon that re-runs this same binary
	// detached from the terminal (so the daemon child never sees a terminal
	// either). Neither is a person watching output.
	if name == "up" {
		for _, flag := range []string{"silent", "survive"} {
			if v, err := cmd.Flags().GetBool(flag); err == nil && v {
				return false
			}
		}
		// The runner streams machine-readable log lines in JSON mode.
		if v, err := cmd.Flags().GetString("log-format"); err == nil && v == "json" {
			return false
		}
	}
	return true
}

// printUpdateNotice shows the release notice when the shared cache proves a
// newer release exists, and otherwise schedules a background refresh so a
// later invocation can announce it.
//
// Nothing here can change a command's output or exit status: the notice is
// built only from a cached release tag, and the refresh it may start writes
// only to the user cache directory.
func printUpdateNotice(cmd *cobra.Command, currentVersion string, interactive bool) {
	if !updateNoticeEligible(cmd, interactive) {
		return
	}
	latest := checkerForUpdate().Notice(currentVersion)
	if latest == "" {
		return
	}
	if noColor, err := cmd.Flags().GetBool("no-color"); err == nil && noColor {
		ansi.SetDisabled(true)
	}
	fmt.Fprint(os.Stderr, update.Message(currentVersion, latest))
}

// updateChecker is the Checker this process uses for the release lookup. It is
// created on first use and then kept: update.Default() returns a new Checker
// per call, and only the Checker that scheduled a refresh knows that one is in
// flight.
//
// It is deliberately not created at init time: resolving the cache path
// creates the cache directory, and an invocation that is not eligible for the
// notice must not create directories.
var (
	updateCheckerMu   sync.Mutex
	updateChecker     *update.Checker
	updateCheckerPath string
)

// updateRefreshBudget bounds the wait for the background release lookup at the
// end of a command. It is the lookup's own HTTP timeout, so a cold round trip
// to the release API is inside the budget: the cache is written even when the
// first request of a session has to establish the connection. A slower or
// unreachable release API still leaves the cache unwritten — the wait is
// silent and best-effort, exactly as it is today.
const updateRefreshBudget = update.HTTPTimeout

// checkerForUpdate returns the Checker this process uses for the release
// lookup, creating it on first use. The Checker follows the cache path, so a
// process whose cache location changes gets a Checker for the new one.
func checkerForUpdate() *update.Checker {
	updateCheckerMu.Lock()
	defer updateCheckerMu.Unlock()
	path := update.CachePath()
	if updateChecker == nil || updateCheckerPath != path {
		updateChecker = update.New(path)
		updateCheckerPath = path
	}
	return updateChecker
}

// waitForUpdateRefresh joins the background release lookup the notice
// scheduled, so a short command still records the result for the next
// invocation. It returns at once when no lookup was scheduled.
func waitForUpdateRefresh() {
	updateCheckerMu.Lock()
	c := updateChecker
	updateCheckerMu.Unlock()
	if c == nil {
		return
	}
	c.WaitRefresh(updateRefreshBudget)
}

// stderrIsTerminal reports whether diagnostics are going to a person.
func stderrIsTerminal() bool {
	return term.IsTerminal(int(os.Stderr.Fd()))
}
