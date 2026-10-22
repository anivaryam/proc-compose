package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/anivaryam/proc-compose/internal/paths"
	"github.com/anivaryam/proc-compose/internal/verify"
	"github.com/spf13/cobra"
)

func newCompareCommand(configFile *string) *cobra.Command {
	var (
		machine, streamLogs, requireEnv  bool
		baseline, candidate              string
		repeat                           int
		dir, startup, check, cleanupFlag string
	)
	command := &cobra.Command{
		Use:   "compare --baseline DIR --candidate DIR [processes...] -- PROGRAM [ARG...]",
		Short: "Run one check against a baseline and a candidate worktree and compare outcomes",
		Long: `Run the same check against two caller-supplied worktrees, each through a
full verify attempt with its own fresh session (<session>-baseline and
<session>-candidate), and compare the observed outcomes: overall status, check
status, check exit code, and stdout/stderr digests.

Each worktree must contain the config named by -f (its base name). Sides run
one after another, alternating which goes first on each repetition; the check
receives PROC_COMPOSE_COMPARE_SIDE (baseline|candidate) and
PROC_COMPOSE_COMPARE_REPETITION so it can select explicitly isolated data.
An attempt whose check reports a mismatched environment (see verify --help),
or, with --require-environment, does not report an eligible one, makes the
comparison not comparable.
proc-compose does not isolate databases or external services itself, and it
never attributes a difference to the code change; both are reported as
limitations.

Exit status: 0 the comparison is valid (every attempt ran its check and
verified cleanup) — read outcomes_equal for the verdict (true or false when
it could be determined, null when it could not; the reason is named in
limitations); 1 not comparable; 2 usage; 130 interrupted.`,
		Example:       "  proc-compose compare --baseline ../main --candidate . --json -- npm run test:smoke\n  proc-compose compare --baseline /tmp/base --candidate . --repeat 3 -- go test ./e2e",
		SilenceUsage:  true,
		SilenceErrors: true,
		Annotations:   map[string]string{structuredUsageAnnotation: "true"},
		RunE: func(cmd *cobra.Command, args []string) error {
			selectors, argv := args, []string(nil)
			if dash := cmd.ArgsLenAtDash(); dash >= 0 {
				selectors, argv = args[:dash], args[dash:]
			}
			stderr := cmd.ErrOrStderr()
			name := sessionName
			if name == "" {
				name = "compare"
			}
			build := func(side, root string) (verify.Options, error) {
				if root == "" {
					return verify.Options{}, fmt.Errorf("--%s is required", side)
				}
				absRoot, err := filepath.Abs(root)
				if err != nil {
					return verify.Options{}, err
				}
				config := filepath.Join(absRoot, filepath.Base(resolveConfigExtension(*configFile)))
				sideSession := name + "-" + side
				hash, err := paths.FromConfig(config, sideSession)
				if err != nil {
					return verify.Options{}, err
				}
				opts := verify.Options{ConfigPath: config, Session: sideSession, Hash: hash, Selectors: selectors, Argv: argv, Dir: dir, ToolVersion: version, Log: stderr, RequireEnvironment: requireEnv}
				if !machine || streamLogs {
					opts.ProcessOutput = stderr
				}
				return opts, parseBudgets(startup, check, cleanupFlag, &opts)
			}
			err := validateSessionFlag()
			var base verify.Options
			if err == nil {
				base, err = build("baseline", baseline)
			}
			if err == nil {
				var cand verify.Options
				cand, err = build("candidate", candidate)
				if err == nil && argv == nil {
					err = fmt.Errorf("compare requires a check command after --")
				}
				if err == nil {
					ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
					result := verify.Compare(ctx, base, cand, repeat)
					stop()
					return renderComparison(cmd.OutOrStdout(), result, machine)
				}
			}
			fmt.Fprintf(stderr, "Error: %v\n", err)
			result := &verify.Comparison{SchemaVersion: verify.SchemaVersion, Kind: "verify.comparison", Status: "not_comparable", ExitCode: verify.ExitUsage,
				Differences: []verify.Difference{}, Sides: map[string]verify.SideSummary{}, Order: []string{}, Limitations: []string{}, Errors: []verify.ErrorRecord{{Code: verify.CodeUsage, Count: 1}}}
			return renderComparison(cmd.OutOrStdout(), result, machine)
		},
	}
	usageFlagErrors(command)
	command.Flags().BoolVar(&machine, "json", false, "emit exactly one JSON comparison object on stdout")
	command.Flags().BoolVar(&streamLogs, "stream-logs", false, "with --json, also stream process output to stderr (default: journal only)")
	command.Flags().StringVar(&baseline, "baseline", "", "baseline worktree directory")
	command.Flags().StringVar(&candidate, "candidate", "", "candidate worktree directory")
	command.Flags().IntVar(&repeat, "repeat", 1, fmt.Sprintf("attempts per side (1-%d); alternates order each repetition", verify.MaxCompareRepeat))
	addRequireEnvironmentFlag(command, &requireEnv)
	addBudgetFlags(command, &dir, &startup, &check, &cleanupFlag)
	return command
}

func renderComparison(w io.Writer, c *verify.Comparison, machine bool) error {
	if machine {
		if err := json.NewEncoder(w).Encode(c); err != nil {
			return exitCodeError{verify.ExitFailed}
		}
	} else {
		fmt.Fprintf(w, "compare %s (exit %d)\n", strings.ToUpper(c.Status), c.ExitCode)
		switch {
		case c.OutcomesEqual != nil:
			fmt.Fprintf(w, "  outcomes equal: %t\n", *c.OutcomesEqual)
		case c.Status == "comparable":
			fmt.Fprintf(w, "  outcomes equal: undetermined (see limitations)\n")
		}
		for _, side := range []string{"baseline", "candidate"} {
			summary, ok := c.Sides[side]
			if !ok {
				continue
			}
			fmt.Fprintf(w, "  %s (%s):\n", side, summary.ConfigPath)
			for _, a := range summary.Attempts {
				exit := "-"
				if a.CheckExitCode != nil {
					exit = fmt.Sprint(*a.CheckExitCode)
				}
				fmt.Fprintf(w, "    %s check=%s exit=%s environment=%s cleanup=%s\n", a.Status, a.CheckStatus, exit, a.EnvironmentStatus, a.CleanupStatus)
			}
		}
		for _, d := range c.Differences {
			fmt.Fprintf(w, "  differs: %s baseline=%s candidate=%s\n", d.Field, formatDifference(d.Baseline), formatDifference(d.Candidate))
		}
		fmt.Fprintf(w, "  limitations: %s\n", strings.Join(c.Limitations, ", "))
	}
	if c.ExitCode != 0 {
		return exitCodeError{c.ExitCode}
	}
	return nil
}

// formatDifference renders one side of a difference for humans. A pointer is
// dereferenced, so an exit code reads as a number instead of a heap address,
// and a missing value reads as "(none)". The JSON path is untouched: it
// encodes the pointer itself, keeping the null/number distinction.
func formatDifference(value any) string {
	switch v := value.(type) {
	case nil:
		return "(none)"
	case *int:
		if v == nil {
			return "(none)"
		}
		return strconv.Itoa(*v)
	case *string:
		if v == nil {
			return "(none)"
		}
		return *v
	case string:
		return v
	default:
		return fmt.Sprint(value)
	}
}
