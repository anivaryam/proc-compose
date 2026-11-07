package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/anivaryam/proc-compose/internal/ipc"
	"github.com/anivaryam/proc-compose/internal/verify"
	"github.com/spf13/cobra"
)

// exitCodeError carries a command's documented numeric exit status to main
// after the command has already rendered its own result.
type exitCodeError struct{ code int }

func (e exitCodeError) Error() string { return fmt.Sprintf("exit status %d", e.code) }

// structuredUsageAnnotation marks a command that renders its own structured
// refusal for semantic usage errors the root pre-run hook would otherwise
// return, such as an invalid --session.
const structuredUsageAnnotation = "proc-compose/structured-usage"

// validateSessionFlag checks the global --session value.
func validateSessionFlag() error {
	if sessionName == "" {
		return nil
	}
	if err := validateUnitName(sessionName); err != nil {
		return fmt.Errorf("invalid --session: %w", err)
	}
	return nil
}

func newVerifyCommand(configFile *string) *cobra.Command {
	var (
		machine        bool
		streamLogs     bool
		requireEnv     bool
		allowCross     bool
		dir            string
		startupTimeout string
		checkTimeout   string
		cleanupTimeout string
	)
	command := &cobra.Command{
		Use:   "verify [processes...] -- PROGRAM [ARG...]",
		Short: "Start a fresh owned stack, run one check against it, and verify cleanup",
		Long: `Run one owned verification attempt: validate the config, acquire the
session, start the selected processes, wait for readiness, run the check
command, stop everything this attempt started, and record a receipt.

The check after -- is executed directly (no shell). Use an explicit shell to
get shell syntax, for example: -- sh -c 'npm test && npm run lint'.
It inherits the caller's environment plus PROC_COMPOSE_SESSION,
PROC_COMPOSE_SESSION_DIR, PROC_COMPOSE_RUN_ID and, for each declared named
port, PROC_COMPOSE_PORT_<PROCESS>_<PORT> and PROC_COMPOSE_URL_<PROCESS>_<PORT>.
It runs in the config directory unless --dir is given (relative to it).

The check may write an environment report, a JSON object
{"observations": [{"name", "status", "expected", "observed"}]} with status
matched, mismatched, or unobserved, to the path in
PROC_COMPOSE_ENVIRONMENT_REPORT. It is recorded under environment in the
result. Any observation that did not match, or an invalid report, fails the
attempt; --require-environment also fails it when the report is missing or
empty. The observations are the check's own; proc-compose does not verify them.

An http or tcp readiness probe or health check answered by another live
session (its process, or a port it leases) does not count: readiness waits
for the process's own server, and the error names that session. On macOS
answers are never attributed, so nothing is refused. --allow-cross-session
counts such answers again; the owner is still recorded.

The target session must not already be running; verify refuses rather than
reusing or stopping it. Receipts live under the session runtime directory
(attempts/<attempt_id>.json) and may disappear at logout or reboot.

Exit status: 0 passed; 1 failed (startup, check, environment, required
process, cleanup, or receipt persistence); 2 usage, configuration, or occupied session before
anything started; 130 interrupted. With --json, stdout is exactly one JSON
object (kind verify.response, or verify.compact_error if the full result
cannot fit 64 KiB). Process output streams to stderr in human mode; with
--json it is kept only in the journal (see why --attempt) unless
--stream-logs is given. Flag parsing errors before verify --json is
recognised are plain text.

Default budgets (unmeasured policy): startup 120s (1s-10m), check 10m
(1s-1h), cleanup 30s (30s-2m).`,
		Example: `  proc-compose --session checkout-fix verify --json -- npm run test:smoke
  proc-compose verify api -- go test ./e2e/...
  proc-compose verify --check-timeout 2m --dir web -- npx playwright test`,
		SilenceUsage:  true,
		SilenceErrors: true,
		Annotations:   map[string]string{structuredUsageAnnotation: "true"},
		RunE: func(cmd *cobra.Command, args []string) error {
			selectors, argv := args, []string(nil)
			if dash := cmd.ArgsLenAtDash(); dash >= 0 {
				selectors, argv = args[:dash], args[dash:]
			}
			stdout, stderr := cmd.OutOrStdout(), cmd.ErrOrStderr()
			opts := verify.Options{
				Session:            sessionName,
				Selectors:          selectors,
				Argv:               argv,
				Dir:                dir,
				ToolVersion:        version,
				Log:                stderr,
				RequireEnvironment: requireEnv,
				RefuseCrossSession: !allowCross,
			}
			if !machine || streamLogs {
				opts.ProcessOutput = stderr
			}
			// The config path does not depend on the session, so even a
			// refusal for an invalid --session names it.
			if abs, err := filepath.Abs(resolveConfigExtension(*configFile)); err == nil {
				opts.ConfigPath = abs
			}
			parseErr := validateSessionFlag()
			if parseErr == nil {
				opts.ConfigPath, opts.Hash, _, _, parseErr = resolveConfigPaths(*configFile)
			}
			if parseErr == nil {
				parseErr = parseBudgets(startupTimeout, checkTimeout, cleanupTimeout, &opts)
			}
			if parseErr == nil && argv == nil {
				parseErr = fmt.Errorf("verify requires a check command after --")
			}
			var result *verify.Result
			if parseErr != nil {
				fmt.Fprintf(stderr, "Error: %v\n", parseErr)
				result = verify.Usage(opts, verify.CodeUsage)
			} else {
				ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
				result = verify.Run(ctx, opts)
				stop()
			}
			return renderVerify(stdout, stderr, result, machine)
		},
	}
	usageFlagErrors(command)
	command.Flags().BoolVar(&machine, "json", false, "emit exactly one JSON result object on stdout")
	command.Flags().BoolVar(&streamLogs, "stream-logs", false, "with --json, also stream process output to stderr (default: journal only)")
	addRequireEnvironmentFlag(command, &requireEnv)
	command.Flags().BoolVar(&allowCross, "allow-cross-session", false, "count readiness and health answers from another live session instead of refusing them")
	addBudgetFlags(command, &dir, &startupTimeout, &checkTimeout, &cleanupTimeout)
	return command
}

func renderVerify(stdout, stderr io.Writer, result *verify.Result, machine bool) error {
	if machine {
		data, compact, err := verify.Encode(result)
		if err != nil {
			fmt.Fprintf(stderr, "Error: cannot encode verify result: %v\n", err)
			return exitCodeError{verify.ExitFailed}
		}
		if _, err := fmt.Fprintln(stdout, string(data)); err != nil {
			return exitCodeError{verify.ExitFailed}
		}
		if compact {
			return exitCodeError{verify.Compact(result).ExitCode}
		}
	} else {
		printVerifyHuman(stdout, result)
	}
	if result.ExitCode != 0 {
		return exitCodeError{result.ExitCode}
	}
	return nil
}

func printVerifyHuman(w io.Writer, r *verify.Result) {
	fmt.Fprintf(w, "verify %s (exit %d)\n", strings.ToUpper(r.Status), r.ExitCode)
	fmt.Fprintf(w, "  session:  %s", r.Session)
	if r.RunID != nil {
		fmt.Fprintf(w, " (run %s)", *r.RunID)
	}
	fmt.Fprintln(w)
	fmt.Fprintf(w, "  config:   %s (validation %s)\n", r.ConfigPath, r.Startup.Validation)
	fmt.Fprintf(w, "  startup:  %s\n", r.Startup.Status)
	for _, obs := range r.Startup.ReadinessObservations {
		probe := "none"
		if obs.Probe != nil {
			probe = *obs.Probe
		}
		fmt.Fprintf(w, "    %s gen %d: %s (probe %s)\n", obs.Process, obs.Generation, obs.Status, probe)
		if o := obs.Owner; o != nil && o.Result == ipc.OwnerForeign {
			fmt.Fprintf(w, "      warning: answered at %s by PID %d (%s), outside %s's process tree\n", o.Address, o.PID, o.Command, obs.Process)
			if o.Scope == ipc.ScopeCrossSession && o.Session != nil {
				fmt.Fprintf(w, "      another live session's answer: PID %d %s\n", o.PID, o.Session.Describe())
			}
		}
	}
	if len(r.Startup.UnprobedProcesses) > 0 {
		fmt.Fprintf(w, "    started without a readiness probe: %s\n", strings.Join(r.Startup.UnprobedProcesses, ", "))
	}
	// The endpoint names are sorted so the rendering is reproducible; the
	// JSON path is already sorted by encoding/json.
	endpoints := make([]string, 0, len(r.Endpoints))
	for name := range r.Endpoints {
		endpoints = append(endpoints, name)
	}
	sort.Strings(endpoints)
	for _, name := range endpoints {
		fmt.Fprintf(w, "  endpoint: %s %s\n", name, r.Endpoints[name].URL)
	}
	fmt.Fprintf(w, "  check:    %s", r.Check.Status)
	if r.Check.ExitCode != nil {
		fmt.Fprintf(w, " (exit %d)", *r.Check.ExitCode)
	}
	if r.Check.Signal != nil {
		fmt.Fprintf(w, " (signal %s)", *r.Check.Signal)
	}
	if r.Check.TimedOut {
		fmt.Fprint(w, " (timed out)")
	}
	fmt.Fprintf(w, "  %s\n", strings.Join(r.Check.Argv, " "))
	printEnvironmentHuman(w, r.Environment)
	fmt.Fprintf(w, "  cleanup:  %s (signalling authority %s)\n", r.Cleanup.Status, r.Cleanup.SignalingAuthority)
	for _, u := range r.Cleanup.UnresolvedProcesses {
		fmt.Fprintf(w, "    unresolved: %s (%s)\n", u.Name, u.Identity)
	}
	if len(r.Errors) > 0 {
		codes := make([]string, 0, len(r.Errors))
		for _, e := range r.Errors {
			codes = append(codes, fmt.Sprintf("%s×%d", e.Code, e.Count))
		}
		fmt.Fprintf(w, "  errors:   %s\n", strings.Join(codes, ", "))
	}
	if r.ReceiptPath != nil {
		state := "persisted"
		if !r.ReceiptPersisted {
			state = "NOT persisted"
		}
		fmt.Fprintf(w, "  receipt:  %s (%s; %s)\n", *r.ReceiptPath, state, r.Retention)
	}
	if r.Startup.Status == verify.StatusPassed && r.Source.Revision != nil {
		dirty := ""
		if r.Source.Dirty != nil && *r.Source.Dirty {
			dirty = " (dirty)"
		}
		fmt.Fprintf(w, "  source:   %s%s — provenance only; loaded code is not proven\n", *r.Source.Revision, dirty)
	}
}

// printEnvironmentHuman prints the environment verdict when the check reported
// one or the attempt required it. Receipts from before environment reports
// have no status and print nothing.
func printEnvironmentHuman(w io.Writer, env verify.Environment) {
	reported := env.Status == verify.EnvironmentEligible || env.Status == verify.EnvironmentIneligible ||
		(env.Reason != nil && *env.Reason != verify.ReasonReportMissing)
	if env.Status == "" || !(env.Required || reported) {
		return
	}
	fmt.Fprintf(w, "  environment: %s", env.Status)
	if env.Reason != nil {
		fmt.Fprintf(w, " (%s)", *env.Reason)
	}
	if env.Required {
		fmt.Fprint(w, " [required]")
	}
	fmt.Fprintln(w)
	for _, o := range env.Observations {
		if o.Status == verify.ObservationMatched {
			continue
		}
		fmt.Fprintf(w, "    %s %s: expected %s, observed %s\n", o.Name, o.Status, orNone(o.Expected), orNone(o.Observed))
	}
}

func orNone(value *string) string {
	if value == nil {
		return "(none)"
	}
	return strconv.Quote(*value)
}

// addRequireEnvironmentFlag registers --require-environment for verify and compare.
func addRequireEnvironmentFlag(command *cobra.Command, target *bool) {
	command.Flags().BoolVar(target, "require-environment", false, "fail unless the check reports an eligible environment to PROC_COMPOSE_ENVIRONMENT_REPORT")
}

// parseBudgets parses phase budget flags. Range checks belong to the
// operation, so humans and agents receive the same structured refusal.
func parseBudgets(startup, check, cleanup string, opts *verify.Options) error {
	for _, budget := range []struct {
		name, raw string
		target    *time.Duration
	}{
		{"startup-timeout", startup, &opts.StartupTimeout},
		{"check-timeout", check, &opts.CheckTimeout},
		{"cleanup-timeout", cleanup, &opts.CleanupTimeout},
	} {
		value, err := time.ParseDuration(budget.raw)
		if err != nil {
			return fmt.Errorf("--%s: %v", budget.name, err)
		}
		*budget.target = value
	}
	return nil
}

// addBudgetFlags registers the shared phase budget flags.
func addBudgetFlags(command *cobra.Command, dir, startup, check, cleanup *string) {
	command.Flags().StringVar(dir, "dir", "", "check working directory (relative paths resolve against the config directory)")
	command.Flags().StringVar(startup, "startup-timeout", verify.DefaultStartupTimeout.String(), "startup and readiness budget (1s-10m)")
	command.Flags().StringVar(check, "check-timeout", verify.DefaultCheckTimeout.String(), "check command budget (1s-1h)")
	command.Flags().StringVar(cleanup, "cleanup-timeout", verify.DefaultCleanupTimeout.String(), "independent cleanup budget (30s-2m)")
}

// usageFlagErrors keeps parser errors visible while the command silences
// cobra's own error printing to protect JSON stdout.
func usageFlagErrors(command *cobra.Command) {
	command.SetFlagErrorFunc(func(cmd *cobra.Command, err error) error {
		fmt.Fprintf(cmd.ErrOrStderr(), "Error: %v\n", err)
		return exitCodeError{verify.ExitUsage}
	})
}
