package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	"github.com/anivaryam/proc-compose/internal/bootstrap"
	"github.com/anivaryam/proc-compose/internal/config"
	"github.com/anivaryam/proc-compose/internal/daemon"
	"github.com/anivaryam/proc-compose/internal/doctor"
	"github.com/anivaryam/proc-compose/internal/ipc"
	"github.com/anivaryam/proc-compose/internal/logrotate"
	"github.com/anivaryam/proc-compose/internal/monitor"
	"github.com/anivaryam/proc-compose/internal/paths"
	"github.com/anivaryam/proc-compose/internal/runner"
	"github.com/anivaryam/proc-compose/internal/session"
	"github.com/anivaryam/proc-compose/internal/systemd"
	"github.com/spf13/cobra"
	"github.com/spf13/cobra/doc"
)

var version = "dev"
var sessionName string

func main() {
	var configFile string

	rootCmd := &cobra.Command{
		Use:     "proc-compose",
		Short:   "Run multiple processes from a single config file",
		Version: version,
		Long: `A lightweight process runner that starts, logs, and manages
multiple local services from a single YAML config.

Coding agents:
  proc-compose agent-guide             # offline, version-matched operating guide
  proc-compose status --json           # readiness and resolved session metadata
  proc-compose logs api --json --max-bytes 8192
  proc-compose why api --json          # bounded failure evidence
  proc-compose verify --json -- npm test   # owned stack + check + verified cleanup
  Use -f and --session consistently; see agent-guide for cursors and cleanup.

Config discovery:
  Without -f, proc-compose searches the current directory and walks upward
  for proc-compose.yml (and falls back to proc-compose.yaml). This lets
  commands run from any nested directory of a monorepo.

Daemon files (per absolute config path and optional session):
  Socket: $XDG_RUNTIME_DIR/pc-<hash>.sock (Windows uses a named pipe)
  PID:    $XDG_RUNTIME_DIR/pc-<hash>.pid
  Log:    $XDG_RUNTIME_DIR/pc-<hash>.log (used when --log-file is not set)
  Events: $XDG_RUNTIME_DIR/pc-<hash>.events.jsonl (structured rotating journal)
  Without XDG_RUNTIME_DIR, use the per-user cache/proc-compose directory,
  falling back to the temporary directory when a cache is unavailable.

Environment variables read directly:
  NO_COLOR    Disable ANSI colour output (any non-empty value).
  PORT        Used as merge-port listen port when merge.port is omitted.
  XDG_RUNTIME_DIR   Base dir for socket / PID / log / session leases.

Short-flag conflict:
  -v at the root prints the version. -v on "up" enables verbose debug
  output (cobra subcommand override). Use --version explicitly when in
  doubt.

Example:
  proc-compose up
  proc-compose up --silent              # daemonize
  proc-compose up --log-file ./run.log  # log to file
  proc-compose monitor                  # TUI for a running daemon
  proc-compose stop                     # stop a running daemon
  proc-compose list

Full documentation, config reference, and examples (merge:, tunnel,
cloud deploy, systemd survive):
  https://github.com/anivaryam/proc-compose#readme
Report bugs: https://github.com/anivaryam/proc-compose/issues`,
	}

	// ── up ────────────────────────────────────────────────────────────────────
	var (
		silent        bool
		logFile       string
		noColor       bool
		maxLogSize    int64
		survive       bool
		surviveName   string
		install       bool
		force         bool
		uninstallName string
		dryRun        bool
		verbose       bool
		logFormat     string
		healthPort    int
		noBanner      bool
		waitReady     bool
		waitTimeout   int
		daemonChild   bool
		upJSON        bool
	)

	// ── doctor ────────────────────────────────────────────────────────────────
	var (
		doctorWrite bool
		doctorJSON  bool
	)

	// ── bootstrap ─────────────────────────────────────────────────────────────
	var (
		bootstrapWrite  bool
		bootstrapForce  bool
		bootstrapVerify bool
		bootstrapJSON   bool
	)

	upCmd := &cobra.Command{
		Use:     "up [processes...]",
		Aliases: []string{"u"},
		Short:   "Start all (or named) processes",
		Long: `Start every service in the config (or only the named ones, with
their transitive dependencies). Logs are streamed with one colour
per process; Ctrl+C tears the whole tree down.

Process modes:
  service (default)   Long-running. Ready when started, or when
                      ready_when passes. Honours restart policy.
  task                One-shot. Ready when it exits 0; non-zero exit
                      fails the stack. Cannot use ready_when,
                      ready_timeout, max_restarts, or restart != never.

A task-only invocation always runs in the foreground: --silent
requires at least one service in the started closure so the daemon
has something to manage after tasks finish.

If a previous daemon crashed and the process groups its PID file
records may still be running, up refuses and asks you to run
proc-compose stop first. up never signals anything itself.

merge: and tunnel patterns, restart policies, readiness probes, env
auto-injection — see:
  https://github.com/anivaryam/proc-compose/blob/main/docs/configuration.md`,
		Example: `  proc-compose up                                # start all processes
  proc-compose up frontend backend               # start subset (deps auto-included)
  proc-compose up --dry-run                      # validate + list, do not start
  proc-compose up -s --log-file app.log          # daemonize with log file
  proc-compose up -s --wait-ready --wait-timeout 90   # block until probes pass
  proc-compose up -s --health-port 8081          # expose /health on :8081
  proc-compose up --survive --name myapp --install    # systemd auto-restart
  proc-compose up --log-format json              # JSON log output
  proc-compose up --no-color                     # disable ANSI colour
  proc-compose up --verbose                      # verbose debug output`,
		SilenceUsage:  true,
		SilenceErrors: false,
		RunE: func(cmd *cobra.Command, args []string) error {
			if upJSON && !silent {
				return fmt.Errorf("up --json requires --silent (use --log-format json for foreground event streaming)")
			}
			if survive {
				if err := validateSurvivePlatform(runtime.GOOS); err != nil {
					return err
				}
				if err := validateUnitName(surviveName); err != nil {
					return err
				}
			}

			cfg, err := config.Load(resolveConfigExtension(configFile))
			if err != nil {
				return err
			}

			if err := checkOptionalBinaries(cfg); err != nil {
				return err
			}

			if len(args) > 0 {
				for _, name := range args {
					if err := config.CheckSelector(cfg, name); err != nil {
						return err
					}
				}
			}

			if silent {
				if err := validateSilentSelection(cfg, args); err != nil {
					return err
				}
			}

			// Dry-run: validate config and print processes without starting
			if dryRun {
				r := &runner.Runner{Config: cfg, Filter: args}
				r.ListProcesses()
				return nil
			}

			absConfig, hash, socketPath, pidPath, err := resolveConfigPaths(configFile)
			if err != nil {
				return err
			}

			if survive {
				homeDir, err := getHomeDir()
				if err != nil {
					return err
				}

				execPath, err := os.Executable()
				if err != nil {
					return fmt.Errorf("cannot find proc-compose binary: %w", err)
				}

				unit, err := systemd.GenerateUnit(systemd.UnitOptions{
					Name:       surviveName,
					ExecStart:  execPath,
					ConfigFile: absConfig,
					WorkingDir: filepath.Dir(absConfig),
					HomeDir:    homeDir,
				})
				if err != nil {
					return err
				}

				if install {
					return installUnit(unit, surviveName, force)
				}

				fmt.Print(unit)
				return nil
			}

			// ── Pre-flight checks ─────────────────────────────────────────────
			if err := preflightCheck(pidPath, cfg); err != nil {
				return err
			}

			// ── Daemonize ────────────────────────────────────────────────────
			if silent {
				effectiveLog := logFile
				if effectiveLog == "" {
					effectiveLog = paths.Log(hash)
				}
				childArgs := buildChildArgs(args, absConfig, effectiveLog, cmd)
				pid, err := daemon.Reexec(childArgs, paths.BootLog(hash))
				if err != nil {
					return err
				}
				// Do NOT write the PID file here — the child process writes its
				// own PID after its pre-flight check. Writing it from the parent
				// causes a race where the child reads the file, finds itself alive,
				// and incorrectly aborts with "already running".

				// Wait until the child either becomes ready (IPC socket accepts
				// connections) or dies. Without this handshake we'd report
				// "daemon started" while the child silently crashed during
				// startup, leaving the user with no daemon and no error.
				if err := waitForDaemonReady(pid, socketPath, paths.BootLog(hash), 10*time.Second, pidPath); err != nil {
					return err
				}
				if waitReady {
					expected := waitReadyProcessNames(cfg, args)
					if err := waitForProcessesReady(socketPath, expected, time.Duration(waitTimeout)*time.Second); err != nil {
						return err
					}
				}
				if upJSON {
					project, err := session.Read(paths.Metadata(hash))
					if err != nil {
						return err
					}
					return json.NewEncoder(os.Stdout).Encode(map[string]interface{}{"operation": "up", "pid": pid, "ready": waitReady, "project": project})
				}
				fmt.Printf("proc-compose daemon started\n")
				fmt.Printf("  PID:     %d\n", pid)
				fmt.Printf("  Logs:    %s\n", effectiveLog)
				fmt.Printf("  Monitor: proc-compose monitor\n")
				fmt.Printf("  Stop:    proc-compose stop\n")
				return nil
			}

			// ── Foreground ───────────────────────────────────────────────────
			// Takes the session and, under its lease, the authoritative
			// crashed-run check.
			plan, err := acquireRun(cfg, absConfig, hash, sessionName, args, pidPath, socketPath)
			if err != nil {
				return err
			}
			defer plan.Close()
			cfg = plan.Config
			plan.Project.Owner = session.NewOwner("up", absConfig)
			plan.Project.LogPath = logFile
			if logFile != "" {
				logFile, err = filepath.Abs(logFile)
				if err != nil {
					return err
				}
				plan.Project.LogPath = logFile
			}
			if plan.Project.LogPath == "" {
				plan.Project.LogPath = paths.Log(hash)
			}
			if err := session.Save(paths.Metadata(hash), plan.Project); err != nil {
				return err
			}
			journal, err := logrotate.New(paths.Journal(hash), 10<<20, 3)
			if err != nil {
				return fmt.Errorf("event journal: %w", err)
			}
			defer journal.Close()
			var logWriter io.WriteCloser
			if logFile != "" {
				if maxLogSize > 0 {
					logWriter, err = logrotate.New(logFile, maxLogSize, 3)
				} else {
					logWriter, err = os.OpenFile(logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
				}
				if err != nil {
					return fmt.Errorf("cannot open log file: %w", err)
				}
				defer logWriter.Close()
			}

			// Write PID for stop/monitor to find even in foreground mode.
			if err := daemon.WritePID(pidPath, os.Getpid(), socketPath); err != nil {
				fmt.Fprintf(os.Stderr, "warning: could not write PID file: %v\n", err)
			}

			// Start IPC server so monitor can attach even in foreground mode.
			// IPC is the only way for `stop`, `restart`, `reload`, and `monitor`
			// to reach this process, so a bind failure is fatal in both
			// foreground and daemon-child modes — the latter relies on it for
			// the parent's readiness handshake.
			ipcServer := ipc.NewServer(socketPath)
			ipcServer.SetProject(plan.Project)
			if err := ipcServer.Listen(); err != nil {
				daemon.Cleanup(pidPath, socketPath)
				return fmt.Errorf("IPC bind failed: %w (path: %s)", err, sanitizePath(socketPath))
			}

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			sig := make(chan os.Signal, 2)
			signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
			go func() {
				<-sig
				cancel()
			}()
			defer signal.Stop(sig)

			r := &runner.Runner{
				Config:        cfg,
				ConfigPath:    absConfig,
				Filter:        args,
				LogFile:       logWriter,
				IPC:           ipcServer,
				NoColor:       noColor,
				Verbose:       verbose,
				LogFormat:     logFormat,
				HealthPort:    healthPort,
				Silent:        noBanner,
				Journal:       journal,
				Project:       plan.Project,
				ResolveConfig: plan.Resolve,
				RecordPath:    pidPath,
			}
			if daemonChild {
				r.Output, r.LogFile = logWriter, nil
			}

			runErr := r.Run(ctx)

			if ipcServer != nil {
				ipcServer.Shutdown()
			}
			daemon.Cleanup(pidPath, socketPath)

			return runErr
		},
	}

	upCmd.Flags().BoolVarP(&silent, "silent", "s", false, "daemonize: detach from terminal and run in background")
	upCmd.Flags().StringVar(&logFile, "log-file", "", "write all process output to FILE")
	upCmd.Flags().BoolVar(&noColor, "no-color", false, "disable ANSI color output (also: NO_COLOR env var)")
	upCmd.Flags().Int64Var(&maxLogSize, "max-log-size", 0, "rotate log file when it exceeds this size in bytes (0 = no rotation)")
	upCmd.Flags().BoolVar(&survive, "survive", false, "generate systemd unit for auto-restart on reboot (Linux with systemd only; prints to stdout)")
	upCmd.Flags().StringVar(&surviveName, "name", "", "service name for --survive (required)")
	upCmd.Flags().BoolVar(&install, "install", false, "install and enable systemd unit (with --survive)")
	upCmd.Flags().BoolVar(&force, "force", false, "overwrite existing unit file (with --install)")
	upCmd.Flags().BoolVar(&dryRun, "dry-run", false, "validate config and list processes without starting")
	upCmd.Flags().BoolVarP(&verbose, "verbose", "v", false, "enable verbose debug output")
	upCmd.Flags().StringVar(&logFormat, "log-format", "text", "log output format: text or json")
	upCmd.Flags().IntVar(&healthPort, "health-port", 0, "HTTP port for /health endpoint (0 = disabled)")
	upCmd.Flags().BoolVar(&noBanner, "no-banner", false, "suppress startup banner (set automatically when daemonizing)")
	upCmd.Flags().BoolVar(&daemonChild, "daemon-child", false, "internal daemon output routing")
	_ = upCmd.Flags().MarkHidden("daemon-child")
	upCmd.Flags().BoolVar(&upJSON, "json", false, "emit daemon session metadata as JSON (requires --silent)")
	if err := upCmd.Flags().MarkHidden("no-banner"); err != nil {
		fmt.Fprintf(os.Stderr, "warning: failed to hide --no-banner: %v\n", err)
	}
	upCmd.Flags().BoolVar(&waitReady, "wait-ready", false, "with --silent, block until all started processes pass their readiness checks")
	upCmd.Flags().IntVar(&waitTimeout, "wait-timeout", 60, "seconds to wait for readiness when --wait-ready is set")

	// ── monitor ───────────────────────────────────────────────────────────────
	monitorCmd := &cobra.Command{
		Use:     "monitor",
		Aliases: []string{"m"},
		Short:   "Open a TUI monitor for a running daemon",
		Long: `Open a TUI monitor for a running daemon.

Keys:
  q, Ctrl+C       quit
  ?, h            toggle help overlay
  up/down, k/j    navigate processes
  Enter           toggle log filter for selected process
  a               clear filter (show all logs)
  PgUp, PgDn      scroll log buffer
  G               jump to live tail
  g               jump to top of log buffer

NO_COLOR=1 disables colour output in the TUI as well as the runner.

Screenshots and full key reference:
  https://github.com/anivaryam/proc-compose/blob/main/docs/cli.md#monitor-a-running-daemon`,
		Example:       "  proc-compose monitor           # open TUI\n  proc-compose monitor -f myapp.yml   # monitor specific config",
		SilenceUsage:  true,
		SilenceErrors: false,
		RunE: func(cmd *cobra.Command, args []string) error {
			_, _, socketPath, _, err := resolveLiveDaemonPaths(configFile)
			if err != nil {
				return err
			}
			return monitor.Run(socketPath)
		},
	}

	// ── stop ──────────────────────────────────────────────────────────────────
	var (
		stopTimeoutSeconds int
		stopForce          bool
	)
	stopCmd := &cobra.Command{
		Use:   "stop",
		Short: "Stop a running proc-compose daemon",
		Long: `Stop the proc-compose daemon associated with the resolved config file.

Daemons are scoped per absolute config path. Use -f to stop a daemon
started with a different config file. Asks the daemon to shut down and
waits up to --timeout seconds for its verdict.

A stop is only reported as successful once the daemon has confirmed that
every managed process it started — not just the command leaders — has
actually terminated. If the daemon cannot be stopped gracefully, this
command reports the failure instead of claiming success, because killing
the daemon alone leaves its managed processes running.

--force asks the daemon to force-terminate its managed processes and then
exit, rather than signalling the daemon itself. If the daemon cannot be
reached (an older proc-compose, or a dead socket), it is killed and the
command warns that managed processes could not be verified.

If the daemon already crashed, stop recovers from the process groups its
PID file records, under the session lease. On Linux a group whose leader is
still alive with its recorded start time is terminated through pidfds
(SIGTERM, then SIGKILL at --timeout; --force sends SIGKILL at once). A group
already gone is reported empty. Anything else is reported unknown and never
signalled: a group whose leader was reaped while members remain, pids from
another PID namespace, and every live group on macOS, which only reports.
Processes that left their group with setsid or setpgid are not in the
record, so stop neither reports nor signals them. One "recovered group=..."
evidence line is printed per group. stop exits 0 and removes the PID file only when every group is empty
or terminated; otherwise it keeps the file. --force then clears the stale
state but still exits non-zero. A PID file with no group record (an older
daemon) or one that cannot be read proves nothing: stop fails, and --force
clears it with a warning. --force exits 0 for a readable file with no group
record, and non-zero for an unreadable one, which may have recorded groups.
On Windows the managed processes ended with the
daemon's job objects, so the stale state is simply cleared. A crashed daemon
never ran its teardown processes; stop then reports "teardown did not run".

Teardown processes run after termination is verified, also with --force.
The default --timeout grows by their 15s budget; an explicit one is kept.
If only teardown failed, stop says termination was verified but teardown did
not pass, and exits non-zero.

On Windows the graceful path is unavailable; --timeout is ignored and
managed processes are always terminated with taskkill /T /F.`,
		Example:       "  proc-compose stop              # graceful, 10s timeout\n  proc-compose stop --timeout 30 # graceful, 30s timeout\n  proc-compose stop --force      # force-terminate managed processes\n  proc-compose stop -f myapp.yml # stop daemon for a specific config",
		SilenceUsage:  true,
		SilenceErrors: false,
		RunE: func(cmd *cobra.Command, args []string) error {
			_, hash, socketPath, pidPath, err := resolveLiveDaemonPaths(configFile)
			if err != nil {
				return err
			}

			stopTimeout := time.Duration(stopTimeoutSeconds) * time.Second
			alive, pid := daemon.IsAliveFromPIDFile(pidPath)
			if pid == 0 {
				if _, statErr := os.Stat(pidPath); statErr != nil {
					return fmt.Errorf("no running proc-compose found for this config (tried %s)", sanitizePath(pidPath))
				}
				// A PID file that does not parse proves nothing; --force can clear it.
				return stopDeadDaemon(pidPath, socketPath, hash, 0, stopForce, stopTimeout)
			}
			if !alive {
				return stopDeadDaemon(pidPath, socketPath, hash, pid, stopForce, stopTimeout)
			}

			proc, err := os.FindProcess(pid)
			if err != nil {
				return err
			}

			// Teardown runs after termination is verified, so a default
			// --timeout leaves room for its budget.
			stopTimeout = stopBudget(paths.Metadata(hash), stopTimeout, cmd.Flags().Changed("timeout"))
			return stopDaemon(socketPath, pidPath, pid, proc, stopForce, stopTimeout)
		},
	}
	stopCmd.Flags().IntVar(&stopTimeoutSeconds, "timeout", 10, "seconds to wait for a verified shutdown")
	stopCmd.Flags().BoolVar(&stopForce, "force", false, "force-terminate managed processes instead of shutting down gracefully")

	// ── list ──────────────────────────────────────────────────────────────────
	listCmd := &cobra.Command{
		Use:           "list",
		Aliases:       []string{"l"},
		Short:         "List processes defined in the config file",
		Example:       "  proc-compose list              # show processes with restart policy\n  proc-compose list -f myapp.yml",
		SilenceUsage:  true,
		SilenceErrors: false,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load(resolveConfigExtension(configFile))
			if err != nil {
				return err
			}
			r := &runner.Runner{Config: cfg}
			r.ListProcesses()
			return nil
		},
	}

	// ── init ──────────────────────────────────────────────────────────────────
	var initTemplate string
	initCmd := &cobra.Command{
		Use:     "init",
		Aliases: []string{"i"},
		Short:   "Generate a starter proc-compose.yml",
		Long: `Generate a starter proc-compose.yml in the current directory. Refuses
to overwrite an existing file. Use 'bootstrap' instead if you want
proc-compose to scan the project and propose a config tailored to it.`,
		Example: `  proc-compose init                  # minimal language-agnostic
  proc-compose init --template node  # frontend+backend with merge-port
  proc-compose init --template go    # microservices skeleton
  proc-compose init --template python`,
		SilenceUsage:  true,
		SilenceErrors: false,
		RunE: func(cmd *cobra.Command, args []string) error {
			if _, err := os.Stat(configFile); err == nil {
				return fmt.Errorf("%s already exists", configFile)
			}
			template, err := starterTemplateForPlatform(initTemplate, runtime.GOOS)
			if err != nil {
				return err
			}
			if err := os.WriteFile(configFile, []byte(template), 0600); err != nil {
				return err
			}
			fmt.Printf("Created %s — edit it and run: proc-compose up\n", configFile)
			return nil
		},
	}
	initCmd.Flags().StringVar(&initTemplate, "template", "minimal", "starter template: minimal, node, go, python")

	// ── uninstall ────────────────────────────────────────────────────────────
	uninstallCmd := &cobra.Command{
		Use:     "uninstall",
		Aliases: []string{"un"},
		Short:   "Remove systemd unit installed via --survive --install",
		Long: `Stop, disable, and remove the systemd user unit previously installed
with "up --survive --install --name <n>", then run daemon-reload.

Linux with systemd user services only. The --name flag is required and
must match the name passed to --install.

See: https://github.com/anivaryam/proc-compose/blob/main/docs/cli.md#auto-restart-on-reboot---survive`,
		Example:       "  proc-compose uninstall --name myapp",
		SilenceUsage:  true,
		SilenceErrors: false,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := validateUninstallPlatform(runtime.GOOS); err != nil {
				return err
			}
			if err := validateUnitName(uninstallName); err != nil {
				return err
			}

			homeDir, err := getHomeDir()
			if err != nil {
				return err
			}

			unitPath := filepath.Join(homeDir, ".config", "systemd", "user", fmt.Sprintf("proc-compose-%s.service", uninstallName))

			// Check if unit file exists
			if _, err := os.Stat(unitPath); os.IsNotExist(err) {
				return fmt.Errorf("unit file not found at %s", unitPath)
			}

			// Stop service
			stopCmd := exec.Command("systemctl", "--user", "stop", fmt.Sprintf("proc-compose-%s", uninstallName))
			stopCmd.Stdout = os.Stdout
			stopCmd.Stderr = os.Stderr
			if err := stopCmd.Run(); err != nil {
				fmt.Fprintf(os.Stderr, "warning: failed to stop service: %v\n", err)
			}

			// Disable service
			disableCmd := exec.Command("systemctl", "--user", "disable", fmt.Sprintf("proc-compose-%s", uninstallName))
			disableCmd.Stdout = os.Stdout
			disableCmd.Stderr = os.Stderr
			if err := disableCmd.Run(); err != nil {
				fmt.Fprintf(os.Stderr, "warning: failed to disable service: %v\n", err)
			}

			// Remove unit file
			if err := os.Remove(unitPath); err != nil {
				return fmt.Errorf("failed to remove unit file: %w", err)
			}

			// Reload systemd
			reloadCmd := exec.Command("systemctl", "--user", "daemon-reload")
			reloadCmd.Stdout = os.Stdout
			reloadCmd.Stderr = os.Stderr
			if err := reloadCmd.Run(); err != nil {
				fmt.Fprintf(os.Stderr, "warning: failed to reload systemd: %v\n", err)
			}

			fmt.Printf("Uninstalled: proc-compose-%s\n", uninstallName)
			return nil
		},
	}

	// ── restart ──────────────────────────────────────────────────────────────
	var restartWait bool
	var restartTimeout int
	var restartJSON bool
	restartResult := mutationResponse{Operation: "restart"}
	restartCmd := &cobra.Command{
		Use:     "restart <process>",
		Aliases: []string{"r"},
		Short:   "Restart a single process in a running daemon",
		Long: `Restart one named process in the running daemon without disturbing
others. The daemon (not the local config) is the source of truth for
process names — an unknown name returns "unknown process" from the
daemon. Exits non-zero if the daemon rejects the request or is busy
applying another command. With --json, results expose status, an optional
message and generation, plus ready when --wait-ready reaches the operation.
Flag-parsing errors remain plain text and exit 1.`,
		Example:       "  proc-compose restart backend    # restart single process\n  proc-compose restart -f myapp.yml backend",
		Args:          cobra.ExactArgs(1),
		SilenceUsage:  true,
		SilenceErrors: false,
		RunE: func(cmd *cobra.Command, args []string) error {
			restartResult.Process = args[0]
			if restartWait {
				restartResult.Ready = new(bool)
			}
			// Don't validate the process name against the local config —
			// the daemon may be running a different (older) config and is
			// the source of truth. The daemon's ack carries an
			// "unknown process" error if the name doesn't match.
			_, _, socketPath, _, err := resolveLiveDaemonPaths(configFile)
			if err != nil {
				return err
			}
			client, err := ipc.Dial(socketPath)
			if err != nil {
				return err
			}
			defer client.Close()

			if err := client.Send(ipc.Command{Action: "restart", Process: args[0]}); err != nil {
				return fmt.Errorf("failed to send restart command: %w", err)
			}

			// Read events until we get an ack (skip snapshot and any state/log events).
			for {
				ev, err := client.Recv()
				if err != nil {
					return fmt.Errorf("no response from daemon: %w", err)
				}
				if ev.Type == ipc.TypeAck {
					restartResult.acknowledge(ev)
					switch ev.Ack {
					case "ok":
						if restartWait {
							if ev.AckGeneration == 0 {
								return fmt.Errorf("daemon does not expose process generations; restart it with the current proc-compose")
							}
							if err := waitForProcessesReady(socketPath, map[string]struct{}{args[0]: {}}, time.Duration(restartTimeout)*time.Second, map[string]uint64{args[0]: ev.AckGeneration}); err != nil {
								return err
							}
							*restartResult.Ready = true
							if !restartJSON {
								fmt.Printf("restarted %q and verified generation %d is ready\n", args[0], ev.AckGeneration)
							}
							return nil
						}
						if !restartJSON {
							fmt.Printf("restart requested for %q\n", args[0])
						}
						return nil
					case "busy":
						return fmt.Errorf("daemon busy — try again")
					default:
						msg := ev.AckDetail
						if msg == "" {
							msg = ev.Ack
						}
						return fmt.Errorf("daemon rejected restart: %s", msg)
					}
				}
			}
		},
	}
	restartCmd.Flags().BoolVar(&restartWait, "wait-ready", false, "wait for the replacement incarnation's readiness")
	restartCmd.Flags().IntVar(&restartTimeout, "timeout", 60, "seconds to wait for replacement readiness")
	addMutationJSON(restartCmd, &restartJSON, &restartResult)

	// ── reload ───────────────────────────────────────────────────────────────
	var reloadJSON bool
	reloadResult := mutationResponse{Operation: "reload"}
	reloadCmd := &cobra.Command{
		Use:     "reload",
		Aliases: []string{"rl"},
		Short:   "Reload config and restart changed processes",
		Long: `Re-read the config and restart processes whose definition (cmd, env,
dir, etc.) changed. Unchanged processes keep running.

Hot-add / hot-remove are not supported: if processes were added or
removed the daemon returns "partial" naming the skipped entries —
restart the daemon to pick those up. Exits 0 on "ok" and "partial",
non-zero on rejection or daemon-busy. With --json, status distinguishes
"ok", "partial", "busy" and "error" without parsing human text. A successful
reload does not establish replacement readiness. Flag-parsing errors remain
plain text and exit 1.`,
		Example:       "  proc-compose reload            # reload and restart changed processes\n  proc-compose reload -f myapp.yml",
		SilenceUsage:  true,
		SilenceErrors: false,
		RunE: func(cmd *cobra.Command, args []string) error {
			_, _, socketPath, _, err := resolveLiveDaemonPaths(configFile)
			if err != nil {
				return err
			}
			client, err := ipc.Dial(socketPath)
			if err != nil {
				return err
			}
			defer client.Close()

			if err := client.Send(ipc.Command{Action: "reload"}); err != nil {
				return fmt.Errorf("failed to send reload command: %w", err)
			}

			for {
				ev, err := client.Recv()
				if err != nil {
					return fmt.Errorf("no response from daemon: %w", err)
				}
				if ev.Type == ipc.TypeAck {
					reloadResult.acknowledge(ev)
					switch ev.Ack {
					case "ok":
						if !reloadJSON {
							if ev.AckDetail != "" {
								fmt.Printf("reload: %s\n", ev.AckDetail)
							} else {
								fmt.Println("reload requested")
							}
						}
						return nil
					case "partial":
						if !reloadJSON {
							fmt.Fprintf(os.Stderr, "reload partial: %s\n", ev.AckDetail)
						}
						return nil
					case "busy":
						return fmt.Errorf("daemon busy — try again")
					default:
						msg := ev.AckDetail
						if msg == "" {
							msg = ev.Ack
						}
						return fmt.Errorf("daemon rejected reload: %s", msg)
					}
				}
			}
		},
	}
	addMutationJSON(reloadCmd, &reloadJSON, &reloadResult)

	// ── status ───────────────────────────────────────────────────────────────
	var statusJSON bool
	statusCmd := &cobra.Command{
		Use:           "status",
		Aliases:       []string{"st", "ps"},
		Short:         "Show running daemon's process states",
		Example:       "  proc-compose status            # text table\n  proc-compose status --json     # machine-readable for scripts",
		SilenceUsage:  true,
		SilenceErrors: false,
		RunE: func(cmd *cobra.Command, args []string) error {
			_, _, socketPath, _, err := resolveLiveDaemonPaths(configFile)
			if err != nil {
				return err
			}
			client, err := ipc.Dial(socketPath)
			if err != nil {
				return err
			}
			defer client.Close()

			// First event from the server is always a snapshot. Read it
			// then disconnect — status is a one-shot, not a stream.
			ev, err := client.Recv()
			if err != nil {
				return fmt.Errorf("no response from daemon: %w", err)
			}
			if ev.Type != ipc.TypeSnapshot {
				return fmt.Errorf("unexpected first event type %q", ev.Type)
			}

			if statusJSON {
				return printStatusJSON(ev.Processes, ev.TunnelURL, ev.Project)
			}
			printStatusTable(ev.Processes, ev.TunnelURL)
			return nil
		},
	}
	statusCmd.Flags().BoolVar(&statusJSON, "json", false, "emit machine-readable JSON")

	// ── validate ─────────────────────────────────────────────────────────────
	validateCmd := &cobra.Command{
		Use:     "validate",
		Aliases: []string{"check"},
		Short:   "Parse and validate the config without starting anything",
		Long: `Parse the config and run all validation rules without starting any
process. Exits non-zero on unknown fields, bad restart policies,
circular depends_on, invalid readiness regex, or malformed merge
sections. Suitable for CI and pre-commit checks.`,
		Example:       "  proc-compose validate          # parse + validate\n  proc-compose validate -f myapp.yml",
		SilenceUsage:  true,
		SilenceErrors: false,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfgPath := resolveConfigExtension(configFile)
			cfg, err := config.Load(cfgPath)
			if err != nil {
				return err
			}
			if err := checkOptionalBinaries(cfg); err != nil {
				return err
			}
			fmt.Printf("ok: %s parses cleanly (%d processes)\n", cfgPath, len(cfg.Processes))
			return nil
		},
	}

	// ── logs ─────────────────────────────────────────────────────────────────
	logsCmd := newLogsCommand(&configFile)

	// ── man ───────────────────────────────────────────────────────────────────
	var manDir string
	manCmd := &cobra.Command{
		Use:     "man",
		Short:   "Generate man page to stdout (or to a directory with --dir)",
		Example: "  proc-compose man                            # write man page to stdout\n  proc-compose man --dir /usr/local/share/man/man1  # install pages to a directory",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if manDir != "" {
				if err := os.MkdirAll(manDir, 0755); err != nil {
					return fmt.Errorf("cannot create directory: %w", err)
				}
				return doc.GenManTree(rootCmd, &doc.GenManHeader{Title: "PROC-COMPOSE", Section: "1"}, manDir)
			}
			return doc.GenMan(rootCmd, &doc.GenManHeader{Title: "PROC-COMPOSE", Section: "1"}, os.Stdout)
		},
	}
	manCmd.Flags().StringVar(&manDir, "dir", "", "write man pages to directory")

	// ── bootstrap ─────────────────────────────────────────────────────────────
	bootstrapCmd := &cobra.Command{
		Use:   "bootstrap",
		Short: "Generate setup and optionally validate its config",
		Long: `Scan the project, propose a config, and (optionally) validate the
generated config with proc-compose up --dry-run. Verification checks configuration
only; it does not start application processes or establish readiness. Safe and
read-only by default.

When -f is set, bootstrap scans and resolves paths relative to that
file's directory; otherwise it works in the current directory.
--write refuses to overwrite an existing config unless --force is
passed as well.

See: https://github.com/anivaryam/proc-compose/blob/main/docs/cli.md#bootstrap-a-working-setup`,
		Example: `  proc-compose bootstrap                  # show generated config without changing files
  proc-compose bootstrap --write          # create proc-compose.yml when missing
  proc-compose bootstrap --write --force  # overwrite existing config
  proc-compose bootstrap --verify         # validate generated config (dry run; no processes started)
  proc-compose bootstrap --write --verify # write and verify setup
  proc-compose bootstrap --json           # machine-readable report`,
		SilenceUsage:  true,
		SilenceErrors: false,
		RunE: func(cmd *cobra.Command, args []string) error {
			bootstrapRoot, bootstrapConfig, err := resolveConfigContext(configFile, cmd.Root().PersistentFlags().Changed("file"))
			if err != nil {
				return err
			}
			report, err := bootstrap.Run(bootstrap.Options{
				Root:       bootstrapRoot,
				ConfigFile: bootstrapConfig,
				Write:      bootstrapWrite,
				Overwrite:  bootstrapForce,
				Verify:     bootstrapVerify,
			})
			if bootstrapJSON {
				if jsonErr := bootstrap.WriteJSON(os.Stdout, report); jsonErr != nil {
					return jsonErr
				}
				return err
			}
			if report != nil {
				if textErr := bootstrap.WriteText(os.Stdout, report); textErr != nil {
					return textErr
				}
			}
			return err
		},
	}
	bootstrapCmd.Flags().BoolVar(&bootstrapWrite, "write", false, "create proc-compose.yml when no config exists")
	bootstrapCmd.Flags().BoolVar(&bootstrapForce, "force", false, "overwrite existing config when used with --write")
	bootstrapCmd.Flags().BoolVar(&bootstrapVerify, "verify", false, "validate generated config with a dry run; does not start processes")
	bootstrapCmd.Flags().BoolVar(&bootstrapJSON, "json", false, "emit machine-readable JSON")

	// ── doctor ────────────────────────────────────────────────────────────────
	doctorCmd := &cobra.Command{
		Use:     "doctor",
		Aliases: []string{"doc"},
		Short:   "Scan project and diagnose proc-compose setup",
		Long: `Diagnose a proc-compose project. Without an existing config, scans
common Node/Go/Python layouts (npm/yarn/pnpm/bun, Vite/Next/SvelteKit/
Astro, Go cmd/**/main.go, Django/Flask/Uvicorn/Hypercorn) and prints
suggested processes, ports, readiness probes, and YAML. With an
existing config, checks paths, env files, port conflicts, readiness
gaps, restart loops, merge-port setup, and missing binaries.

When -f is set, scans relative to that config file's directory.
--write only creates proc-compose.yml when no config exists.

See: https://github.com/anivaryam/proc-compose/blob/main/docs/cli.md#diagnose-or-generate-config`,
		Example: `  proc-compose doctor          # report detected services and config issues
  proc-compose doctor --write  # create proc-compose.yml when missing
  proc-compose doctor --json   # machine-readable report`,
		SilenceUsage:  true,
		SilenceErrors: false,
		RunE: func(cmd *cobra.Command, args []string) error {
			doctorRoot, doctorConfig, err := resolveConfigContext(configFile, cmd.Root().PersistentFlags().Changed("file"))
			if err != nil {
				return err
			}
			report, err := doctor.Run(doctor.Options{Root: doctorRoot, ConfigFile: doctorConfig, Write: doctorWrite})
			if doctorJSON {
				if jsonErr := doctor.WriteJSON(os.Stdout, report); jsonErr != nil {
					return jsonErr
				}
				return err
			}
			if report != nil {
				if textErr := doctor.WriteText(os.Stdout, report); textErr != nil {
					return textErr
				}
			}
			return err
		},
	}
	doctorCmd.Flags().BoolVar(&doctorWrite, "write", false, "create proc-compose.yml when no config exists")
	doctorCmd.Flags().BoolVar(&doctorJSON, "json", false, "emit machine-readable JSON")

	rootCmd.PersistentFlags().StringVarP(&configFile, "file", "f", "proc-compose.yml", "config file path (default searches current and parent dirs)")
	rootCmd.PersistentFlags().StringVar(&sessionName, "session", "", "named session with independent identity, ports, logs, and runtime directory")
	uninstallCmd.Flags().StringVar(&uninstallName, "name", "", "service name to uninstall")
	if err := uninstallCmd.MarkFlagRequired("name"); err != nil {
		fmt.Fprintf(os.Stderr, "warning: failed to mark --name as required: %v\n", err)
	}
	rootCmd.AddCommand(upCmd, monitorCmd, stopCmd, listCmd, initCmd, restartCmd, reloadCmd, uninstallCmd, logsCmd, manCmd, statusCmd, validateCmd, bootstrapCmd, doctorCmd, newWhyCommand(&configFile), newAgentGuideCommand(), newVerifyCommand(&configFile), newConfigCommand(&configFile), newAttemptsCommand(&configFile), newSessionsCommand(), newCompareCommand(&configFile))

	// The update notice runs before every command's own work, so it is
	// printed above the output it refers to and never after a TUI has taken
	// over the screen. Cobra returns for --help and --version before any run
	// hook, so those never reach it.
	rootCmd.PersistentPreRunE = func(cmd *cobra.Command, args []string) error {
		// Commands that promise a structured result report this refusal
		// themselves; they silence cobra's printing to keep stdout clean.
		if err := validateSessionFlag(); err != nil && cmd.Annotations[structuredUsageAnnotation] == "" {
			return err
		}
		printUpdateNotice(cmd, version, stderrIsTerminal())
		return nil
	}

	// The notice's release lookup runs in the background. Join it here, after
	// the command has done its work and before any exit below, so a short
	// command still records the result for the next invocation. The join is
	// bounded and returns at once when no lookup was scheduled.
	err := rootCmd.Execute()
	waitForUpdateRefresh()
	if err != nil {
		var coded exitCodeError
		if errors.As(err, &coded) {
			os.Exit(coded.code)
		}
		os.Exit(1)
	}
}

// resolveConfigPaths computes the absolute config path and derived paths for a daemon.
func resolveConfigPaths(configFile string) (absConfig, hash, socketPath, pidPath string, err error) {
	configFile = resolveConfigExtension(configFile)
	absConfig, err = filepath.Abs(configFile)
	if err != nil {
		return "", "", "", "", err
	}
	hash, err = paths.FromConfig(absConfig, sessionName)
	if err != nil {
		return "", "", "", "", err
	}
	return absConfig, hash, paths.Socket(hash), paths.PID(hash), nil
}

func resolveConfigContext(configFile string, explicit bool) (root, configPath string, err error) {
	if explicit {
		absConfig, absErr := filepath.Abs(configFile)
		if absErr != nil {
			return "", "", absErr
		}
		return filepath.Dir(absConfig), absConfig, nil
	}
	resolved := resolveConfigExtension(configFile)
	if resolved != configFile {
		absConfig, absErr := filepath.Abs(resolved)
		if absErr != nil {
			return "", "", absErr
		}
		return filepath.Dir(absConfig), absConfig, nil
	}
	return ".", "", nil
}

func validateSilentSelection(cfg *config.Config, args []string) error {
	for _, name := range config.SelectedNames(cfg, args) {
		if cfg.Processes[name].EffectiveMode() != config.ProcessModeTask {
			return nil
		}
	}
	return fmt.Errorf("up --silent requires at least one service; task-only stacks run in foreground")
}

// resolveConfigExtension makes proc-compose.yaml a transparent stand-in for
// proc-compose.yml when the user accepted the default --file value but
// keeps the .yaml form in the repo. Only triggers when:
//  1. The given path is the literal default ("proc-compose.yml").
//  2. proc-compose.yml does NOT exist in the current directory.
//  3. proc-compose.yaml DOES exist.
//
// Explicit -f flags are returned unchanged so user intent always wins.
func resolveConfigExtension(p string) string {
	if p != "proc-compose.yml" {
		return p
	}
	wd, err := os.Getwd()
	if err != nil {
		return p
	}
	for dir := wd; ; dir = filepath.Dir(dir) {
		yml := filepath.Join(dir, "proc-compose.yml")
		if _, err := os.Stat(yml); err == nil {
			if dir == wd {
				return p
			}
			return yml
		}
		yaml := filepath.Join(dir, "proc-compose.yaml")
		if _, err := os.Stat(yaml); err == nil {
			if dir == wd {
				return "proc-compose.yaml"
			}
			return yaml
		}
		if parent := filepath.Dir(dir); parent == dir {
			break
		}
	}
	return p
}
