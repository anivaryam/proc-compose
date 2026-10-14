package verify

import (
	"context"
	"sort"
	"strconv"
	"time"
)

// MaxCompareRepeat bounds repeated measurements per side.
const MaxCompareRepeat = 5

// Comparison is the result of running one scenario against two caller-supplied
// environments. Differences are observations about the two environments; they
// are never attributed to a code change by this tool.
type Comparison struct {
	SchemaVersion int                    `json:"schema_version"`
	Kind          string                 `json:"kind"`
	Status        string                 `json:"status"` // comparable | not_comparable | interrupted
	ExitCode      int                    `json:"exit_code"`
	OutcomesEqual *bool                  `json:"outcomes_equal"`
	Differences   []Difference           `json:"differences"`
	Sides         map[string]SideSummary `json:"sides"`
	Order         []string               `json:"order"`
	Limitations   []string               `json:"limitations"`
	Errors        []ErrorRecord          `json:"errors"`
}

type Difference struct {
	Field     string `json:"field"`
	Baseline  any    `json:"baseline"`
	Candidate any    `json:"candidate"`
}

type SideSummary struct {
	ConfigPath    string           `json:"config_path"`
	Session       string           `json:"session"`
	Attempts      []AttemptOutcome `json:"attempts"`
	MedianCheckMs *int64           `json:"median_check_ms"`
	Consistent    bool             `json:"consistent"`
}

type AttemptOutcome struct {
	AttemptID       *string `json:"attempt_id"`
	Status          string  `json:"status"`
	ExitCode        int     `json:"exit_code"`
	StartupStatus   string  `json:"startup_status"`
	CheckStatus     string  `json:"check_status"`
	CheckExitCode   *int    `json:"check_exit_code"`
	StdoutSHA256    string  `json:"stdout_sha256"`
	StderrSHA256    string  `json:"stderr_sha256"`
	StdoutExcerpt   string  `json:"stdout_excerpt"`
	OutputTruncated bool    `json:"output_truncated"`
	CheckMs         *int64  `json:"check_ms"`
	CleanupStatus   string  `json:"cleanup_status"`
	// EnvironmentStatus is the check's environment verdict (environment.status).
	EnvironmentStatus string        `json:"environment_status"`
	ReceiptPath       *string       `json:"receipt_path"`
	ReceiptPersisted  bool          `json:"receipt_persisted"`
	Errors            []ErrorRecord `json:"errors"`
}

// outcomeKey is the observable outcome compared across sides.
type outcomeKey struct {
	Status, CheckStatus, Stdout, Stderr string
	CheckExit                           *int
}

func outcomeOf(r *Result) AttemptOutcome {
	o := AttemptOutcome{AttemptID: r.AttemptID, Status: r.Status, ExitCode: r.ExitCode, StartupStatus: r.Startup.Status, CheckStatus: r.Check.Status, CheckExitCode: r.Check.ExitCode,
		StdoutSHA256: digest([]byte(r.Check.Stdout)), StderrSHA256: digest([]byte(r.Check.Stderr)), StdoutExcerpt: truncateUTF8(r.Check.Stdout, 1024), OutputTruncated: r.Check.OutputTruncated,
		CleanupStatus: r.Cleanup.Status, EnvironmentStatus: r.Environment.Status, ReceiptPath: r.ReceiptPath, ReceiptPersisted: r.ReceiptPersisted, Errors: r.Errors}
	if r.Check.StartedAt != nil && r.Check.FinishedAt != nil {
		start, err1 := time.Parse(time.RFC3339Nano, *r.Check.StartedAt)
		end, err2 := time.Parse(time.RFC3339Nano, *r.Check.FinishedAt)
		if err1 == nil && err2 == nil {
			o.CheckMs = ptr(end.Sub(start).Milliseconds())
		}
	}
	if o.Errors == nil {
		o.Errors = []ErrorRecord{}
	}
	return o
}

func (o AttemptOutcome) key() outcomeKey {
	return outcomeKey{Status: o.Status, CheckStatus: o.CheckStatus, Stdout: o.StdoutSHA256, Stderr: o.StderrSHA256, CheckExit: o.CheckExitCode}
}

func (k outcomeKey) equal(other outcomeKey) bool {
	sameExit := (k.CheckExit == nil) == (other.CheckExit == nil) && (k.CheckExit == nil || *k.CheckExit == *other.CheckExit)
	return k.Status == other.Status && k.CheckStatus == other.CheckStatus && k.Stdout == other.Stdout && k.Stderr == other.Stderr && sameExit
}

// Compare runs the scenario repeat times per side, alternating which side
// runs first to counterbalance order effects. Sides run sequentially so they
// never contend for the same external data at the same time.
func Compare(ctx context.Context, baseline, candidate Options, repeat int) *Comparison {
	c := &Comparison{SchemaVersion: SchemaVersion, Kind: "verify.comparison", Differences: []Difference{}, Order: []string{}, Errors: []ErrorRecord{},
		Limitations: []string{"data_isolation_caller_asserted", "differences_not_attributed_to_code_change", "dependencies_and_build_inputs_not_controlled"}}
	if repeat < 1 || repeat > MaxCompareRepeat {
		c.Status, c.ExitCode = "not_comparable", ExitUsage
		c.Errors = []ErrorRecord{{Code: CodeUsage, Count: 1}}
		return c
	}
	if repeat == 1 {
		c.Limitations = append(c.Limitations, "single_run_per_side")
	}
	sides := map[string]Options{"baseline": baseline, "candidate": candidate}
	results := map[string][]*Result{}
	errs := Errors{}
	interrupted := false
	for i := 0; i < repeat && !interrupted; i++ {
		order := []string{"baseline", "candidate"}
		if i%2 == 1 {
			order = []string{"candidate", "baseline"}
		}
		for _, side := range order {
			opts := sides[side]
			opts.ExtraEnv = map[string]string{"PROC_COMPOSE_COMPARE_SIDE": side, "PROC_COMPOSE_COMPARE_REPETITION": strconv.Itoa(i + 1)}
			result := Run(ctx, opts)
			results[side] = append(results[side], result)
			c.Order = append(c.Order, side)
			if result.Status == StatusInterrupted || ctx.Err() != nil {
				interrupted = true
				break
			}
			// A usage/pre-acquisition failure is the same for every repetition.
			if result.ExitCode == ExitUsage {
				for _, record := range result.Errors {
					errs[record.Code] += record.Count
				}
				c.Status, c.ExitCode, c.Errors = "not_comparable", ExitUsage, errs.Records()
				c.Sides = summaries(sides, results)
				return c
			}
		}
	}
	c.Sides = summaries(sides, results)
	comparable := true
	for _, side := range []string{"baseline", "candidate"} {
		for _, r := range results[side] {
			// Comparison requires the check to have run and every owned process
			// to have been cleaned up with a persisted receipt.
			if r.Startup.Status != StatusPassed || r.Check.Status == StatusNotRun || r.Cleanup.Status != StatusPassed || !r.ReceiptPersisted {
				comparable = false
			}
			for _, record := range r.Errors {
				switch record.Code {
				case CodeEnvironmentIneligible, CodeEnvironmentUnverified:
					// The check did not establish that it exercised the
					// intended environment, so its outcome is not comparable.
					comparable = false
					errs[record.Code] += record.Count
				case CodeCleanupUnverified, CodeReceiptPersistence, CodeStartupFailed, CodeStartupTimeout, CodeCheckStartFailed:
					errs[record.Code] += record.Count
				}
			}
		}
	}
	c.Errors = errs.Records()
	switch {
	case interrupted:
		c.Status, c.ExitCode = StatusInterrupted, ExitInterrupted
		errs.Add(CodeInterrupted)
		c.Errors = errs.Records()
	case !comparable:
		c.Status, c.ExitCode = "not_comparable", ExitFailed
	default:
		c.Status, c.ExitCode = "comparable", ExitPassed
		b, k := c.Sides["baseline"].Attempts[0], c.Sides["candidate"].Attempts[0]
		add := func(field string, x, y any, same bool) {
			if !same {
				c.Differences = append(c.Differences, Difference{Field: field, Baseline: x, Candidate: y})
			}
		}
		add("status", b.Status, k.Status, b.Status == k.Status)
		add("check.status", b.CheckStatus, k.CheckStatus, b.CheckStatus == k.CheckStatus)
		bk, kk := b.key(), k.key()
		add("check.exit_code", b.CheckExitCode, k.CheckExitCode, outcomeKey{CheckExit: bk.CheckExit}.equal(outcomeKey{CheckExit: kk.CheckExit}))
		add("check.stdout_sha256", b.StdoutSHA256, k.StdoutSHA256, b.StdoutSHA256 == k.StdoutSHA256)
		add("check.stderr_sha256", b.StderrSHA256, k.StderrSHA256, b.StderrSHA256 == k.StderrSHA256)
		equal := len(c.Differences) == 0
		c.OutcomesEqual = &equal
		if !c.Sides["baseline"].Consistent || !c.Sides["candidate"].Consistent {
			c.Limitations = append(c.Limitations, "nondeterministic_within_side")
		}
	}
	return c
}

func summaries(sides map[string]Options, results map[string][]*Result) map[string]SideSummary {
	out := map[string]SideSummary{}
	for side, opts := range sides {
		name := opts.Session
		if name == "" {
			name = "default"
		}
		summary := SideSummary{ConfigPath: opts.ConfigPath, Session: name, Attempts: []AttemptOutcome{}, Consistent: true}
		var durations []int64
		for _, r := range results[side] {
			o := outcomeOf(r)
			if len(summary.Attempts) > 0 && !summary.Attempts[0].key().equal(o.key()) {
				summary.Consistent = false
			}
			summary.Attempts = append(summary.Attempts, o)
			if o.CheckMs != nil {
				durations = append(durations, *o.CheckMs)
			}
		}
		if len(durations) > 0 {
			sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
			summary.MedianCheckMs = ptr(durations[len(durations)/2])
		}
		out[side] = summary
	}
	return out
}
