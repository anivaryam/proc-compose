package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
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
	"github.com/anivaryam/proc-compose/internal/systemd"
	"github.com/spf13/cobra"
	"github.com/spf13/cobra/doc"
)

var version = "dev"

func main() {
	var configFile string

	rootCmd := &cobra.Command{
		Use:     "proc-compose",
		Short:   "Run multiple processes from a single config file",
		Version: version,
		Long: `A lightweight process runner that starts, logs, and manages
multiple local services from a single YAML config.

Example:
  proc-compose up
  proc-compose up --silent              # daemonize
  proc-compose up --log-file ./run.log  # log to file
  proc-compose monitor                  # TUI for a running daemon
  proc-compose stop                     # stop a running daemon
  proc-compose list`,
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
		Example: `  proc-compose up                    # start all processes
  proc-compose up frontend backend   # start specific processes
  proc-compose up -s --log-file app.log   # daemonize with log file
  proc-compose up --log-format json       # JSON log output
  proc-compose up --verbose              # verbose debug output`,
		SilenceUsage:  true,
		SilenceErrors: false,
		RunE: func(cmd *cobra.Command, args []string) error {
			if survive {
				if err := validateUnitName(surviveName); err != nil {
					return err
				}
			}

			cfg, err := config.Load(resolveConfigExtension(configFile))
			if err != nil {
				return err
			}

			checkOptionalBinaries(cfg)

			if len(args) > 0 {
				for _, name := range args {
					if _, ok := cfg.Processes[name]; !ok {
						return fmt.Errorf("unknown process %q", name)
					}
				}
			}

			// Dry-run: validate config and print processes without starting
			if dryRun {
				r := &runner.Runner{Config: cfg}
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
			if err := preflightCheck(pidPath, socketPath, cfg); err != nil {
				return err
			}

			// ── Daemonize ────────────────────────────────────────────────────
			if silent {
				effectiveLog := logFile
				if effectiveLog == "" {
					effectiveLog = paths.Log(hash)
				}
				childArgs := buildChildArgs(os.Args[1:], absConfig, effectiveLog)
				pid, err := daemon.Reexec(childArgs, effectiveLog)
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
				if err := waitForDaemonReady(pid, socketPath, effectiveLog, 10*time.Second); err != nil {
					return err
				}
				if waitReady {
					expected := waitReadyProcessNames(cfg, args)
					if err := waitForProcessesReady(socketPath, expected, time.Duration(waitTimeout)*time.Second); err != nil {
						return err
					}
				}
				fmt.Printf("proc-compose daemon started\n")
				fmt.Printf("  PID:     %d\n", pid)
				fmt.Printf("  Logs:    %s\n", effectiveLog)
				fmt.Printf("  Monitor: proc-compose monitor\n")
				fmt.Printf("  Stop:    proc-compose stop\n")
				return nil
			}

			// ── Foreground ───────────────────────────────────────────────────
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
				fmt.Println()
				cancel()
			}()

			r := &runner.Runner{
				Config:     cfg,
				ConfigPath: absConfig,
				Filter:     args,
				LogFile:    logWriter,
				IPC:        ipcServer,
				NoColor:    noColor,
				Verbose:    verbose,
				LogFormat:  logFormat,
				HealthPort: healthPort,
				Silent:     noBanner,
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
	upCmd.Flags().BoolVar(&survive, "survive", false, "generate systemd unit for auto-restart on reboot (prints to stdout)")
	upCmd.Flags().StringVar(&surviveName, "name", "", "service name for --survive (required)")
	upCmd.Flags().BoolVar(&install, "install", false, "install and enable systemd unit (with --survive)")
	upCmd.Flags().BoolVar(&force, "force", false, "overwrite existing unit file (with --install)")
	upCmd.Flags().BoolVar(&dryRun, "dry-run", false, "validate config and list processes without starting")
	upCmd.Flags().BoolVarP(&verbose, "verbose", "v", false, "enable verbose debug output")
	upCmd.Flags().StringVar(&logFormat, "log-format", "text", "log output format: text or json")
	upCmd.Flags().IntVar(&healthPort, "health-port", 0, "HTTP port for /health endpoint (0 = disabled)")
	upCmd.Flags().BoolVar(&noBanner, "no-banner", false, "suppress startup banner (set automatically when daemonizing)")
	if err := upCmd.Flags().MarkHidden("no-banner"); err != nil {
		fmt.Fprintf(os.Stderr, "warning: failed to hide --no-banner: %v\n", err)
	}
	upCmd.Flags().BoolVar(&waitReady, "wait-ready", false, "with --silent, block until all started processes pass their readiness checks")
	upCmd.Flags().IntVar(&waitTimeout, "wait-timeout", 60, "seconds to wait for readiness when --wait-ready is set")

	// ── monitor ───────────────────────────────────────────────────────────────
	monitorCmd := &cobra.Command{
		Use:           "monitor",
		Aliases:       []string{"m"},
		Short:         "Open a TUI monitor for a running daemon",
		Example:       "  proc-compose monitor           # open TUI\n  proc-compose monitor -f myapp.yml   # monitor specific config",
		SilenceUsage:  true,
		SilenceErrors: false,
		RunE: func(cmd *cobra.Command, args []string) error {
			_, _, socketPath, _, err := resolveConfigPaths(configFile)
			if err != nil {
				return err
			}
			return monitor.Run(socketPath)
		},
	}

	// ── stop ──────────────────────────────────────────────────────────────────
	var (
		stopTimeout int
		stopForce   bool
	)
	stopCmd := &cobra.Command{
		Use:           "stop",
		Short:         "Stop a running proc-compose daemon",
		Example:       "  proc-compose stop              # graceful, 10s timeout\n  proc-compose stop --timeout 30 # graceful, 30s timeout\n  proc-compose stop --force      # SIGKILL immediately",
		SilenceUsage:  true,
		SilenceErrors: false,
		RunE: func(cmd *cobra.Command, args []string) error {
			_, _, socketPath, pidPath, err := resolveConfigPaths(configFile)
			if err != nil {
				return err
			}

			alive, pid := daemon.IsAliveFromPIDFile(pidPath)
			if pid == 0 {
				return fmt.Errorf("no running proc-compose found for this config (tried %s)", sanitizePath(pidPath))
			}
			if !alive {
				fmt.Printf("daemon (PID %d) is no longer running — cleaning up\n", pid)
				daemon.Cleanup(pidPath, socketPath)
				return nil
			}

			proc, err := os.FindProcess(pid)
			if err != nil {
				return err
			}

			if stopForce {
				if err := daemon.KillProcess(proc); err != nil {
					return fmt.Errorf("failed to SIGKILL %d: %w", pid, err)
				}
				fmt.Printf("killed proc-compose daemon (PID %d)\n", pid)
				return nil
			}

			if err := daemon.StopProcess(proc); err != nil {
				return fmt.Errorf("failed to stop process %d: %w", pid, err)
			}

			deadline := time.Now().Add(time.Duration(stopTimeout) * time.Second)
			for time.Now().Before(deadline) {
				if !daemon.IsAlive(pid) {
					fmt.Printf("stopped proc-compose daemon (PID %d)\n", pid)
					return nil
				}
				time.Sleep(200 * time.Millisecond)
			}

			// Graceful timeout exceeded — escalate.
			fmt.Fprintf(os.Stderr, "daemon did not exit within %ds; sending SIGKILL\n", stopTimeout)
			if err := daemon.KillProcess(proc); err != nil {
				return fmt.Errorf("failed to SIGKILL after timeout: %w", err)
			}
			fmt.Printf("killed proc-compose daemon (PID %d)\n", pid)
			return nil
		},
	}
	stopCmd.Flags().IntVar(&stopTimeout, "timeout", 10, "seconds to wait for graceful shutdown before SIGKILL")
	stopCmd.Flags().BoolVar(&stopForce, "force", false, "SIGKILL immediately without graceful shutdown")

	// ── list ──────────────────────────────────────────────────────────────────
	listCmd := &cobra.Command{
		Use:           "list",
		Aliases:       []string{"l"},
		Short:         "List processes defined in the config file",
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
			template, err := starterTemplate(initTemplate)
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
		Use:           "uninstall",
		Aliases:       []string{"un"},
		Short:         "Remove systemd unit installed via --survive --install",
		SilenceUsage:  true,
		SilenceErrors: false,
		RunE: func(cmd *cobra.Command, args []string) error {
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
	restartCmd := &cobra.Command{
		Use:           "restart <process>",
		Aliases:       []string{"r"},
		Short:         "Restart a single process in a running daemon",
		Example:       "  proc-compose restart backend    # restart single process\n  proc-compose restart -f myapp.yml backend",
		Args:          cobra.ExactArgs(1),
		SilenceUsage:  true,
		SilenceErrors: false,
		RunE: func(cmd *cobra.Command, args []string) error {
			// Don't validate the process name against the local config —
			// the daemon may be running a different (older) config and is
			// the source of truth. The daemon's ack carries an
			// "unknown process" error if the name doesn't match.
			_, _, socketPath, _, err := resolveConfigPaths(configFile)
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
					switch ev.Ack {
					case "ok":
						fmt.Printf("restart requested for %q\n", args[0])
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

	// ── reload ───────────────────────────────────────────────────────────────
	reloadCmd := &cobra.Command{
		Use:           "reload",
		Aliases:       []string{"rl"},
		Short:         "Reload config and restart changed processes",
		Example:       "  proc-compose reload            # reload and restart changed processes\n  proc-compose reload -f myapp.yml",
		SilenceUsage:  true,
		SilenceErrors: false,
		RunE: func(cmd *cobra.Command, args []string) error {
			_, _, socketPath, _, err := resolveConfigPaths(configFile)
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
					switch ev.Ack {
					case "ok":
						if ev.AckDetail != "" {
							fmt.Printf("reload: %s\n", ev.AckDetail)
						} else {
							fmt.Println("reload requested")
						}
						return nil
					case "partial":
						fmt.Fprintf(os.Stderr, "reload partial: %s\n", ev.AckDetail)
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
			_, _, socketPath, _, err := resolveConfigPaths(configFile)
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
				return printStatusJSON(ev.Processes, ev.TunnelURL)
			}
			printStatusTable(ev.Processes, ev.TunnelURL)
			return nil
		},
	}
	statusCmd.Flags().BoolVar(&statusJSON, "json", false, "emit machine-readable JSON")

	// ── validate ─────────────────────────────────────────────────────────────
	validateCmd := &cobra.Command{
		Use:           "validate",
		Aliases:       []string{"check"},
		Short:         "Parse and validate the config without starting anything",
		SilenceUsage:  true,
		SilenceErrors: false,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfgPath := resolveConfigExtension(configFile)
			cfg, err := config.Load(cfgPath)
			if err != nil {
				return err
			}
			fmt.Printf("ok: %s parses cleanly (%d processes)\n", cfgPath, len(cfg.Processes))
			return nil
		},
	}

	// ── logs ─────────────────────────────────────────────────────────────────
	var tailLines int
	logsCmd := &cobra.Command{
		Use:           "logs",
		Short:         "Show logs from a running or past daemon",
		SilenceUsage:  true,
		SilenceErrors: false,
		RunE: func(cmd *cobra.Command, args []string) error {
			_, hash, _, _, err := resolveConfigPaths(configFile)
			if err != nil {
				return err
			}
			logPath := paths.Log(hash)
			if _, err := os.Stat(logPath); os.IsNotExist(err) {
				return fmt.Errorf("no log file found at %s (daemon may not have run with --log-file)", sanitizePath(logPath))
			}
			data, err := os.ReadFile(logPath)
			if err != nil {
				return fmt.Errorf("cannot read log file %s: %w", sanitizePath(logPath), err)
			}
			lines := append([]string(nil), strings.Split(string(data), "\n")...)
			if tailLines > 0 && tailLines < len(lines) {
				lines = lines[len(lines)-tailLines:]
			}
			for _, l := range lines {
				fmt.Println(l)
			}
			return nil
		},
	}
	logsCmd.Flags().IntVarP(&tailLines, "tail", "n", 50, "show last N lines of log")

	// ── man ───────────────────────────────────────────────────────────────────
	var manDir string
	manCmd := &cobra.Command{
		Use:   "man",
		Short: "Generate man page to stdout (or to a directory with --dir)",
		Args:  cobra.NoArgs,
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
		Short: "Generate and optionally verify proc-compose setup",
		Example: `  proc-compose bootstrap                  # show generated config without changing files
  proc-compose bootstrap --write          # create proc-compose.yml when missing
  proc-compose bootstrap --write --force  # overwrite existing config
  proc-compose bootstrap --verify         # verify generated config through proc-compose
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
	bootstrapCmd.Flags().BoolVar(&bootstrapVerify, "verify", false, "verify generated config through proc-compose")
	bootstrapCmd.Flags().BoolVar(&bootstrapJSON, "json", false, "emit machine-readable JSON")

	// ── doctor ────────────────────────────────────────────────────────────────
	doctorCmd := &cobra.Command{
		Use:     "doctor",
		Aliases: []string{"doc"},
		Short:   "Scan project and diagnose proc-compose setup",
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
	uninstallCmd.Flags().StringVar(&uninstallName, "name", "", "service name to uninstall")
	if err := uninstallCmd.MarkFlagRequired("name"); err != nil {
		fmt.Fprintf(os.Stderr, "warning: failed to mark --name as required: %v\n", err)
	}
	rootCmd.AddCommand(upCmd, monitorCmd, stopCmd, listCmd, initCmd, restartCmd, reloadCmd, uninstallCmd, logsCmd, manCmd, statusCmd, validateCmd, bootstrapCmd, doctorCmd)

	if err := rootCmd.Execute(); err != nil {
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
	hash, err = paths.FromConfig(absConfig)
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
