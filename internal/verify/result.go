// Package verify runs one owned verification attempt: start a fresh session,
// wait for readiness, run an existing check command, record a receipt, and
// verify cleanup. Humans and agents render the same Result.
package verify

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"time"
)

const SchemaVersion = 1

// Result kinds.
const (
	KindResponse     = "verify.response"
	KindReceipt      = "verify.receipt"
	KindCompactError = "verify.compact_error"
)

// Attempt (receipt) and response states.
const (
	StateActive        = "active"
	StateCompleted     = "completed"
	StateAbandoned     = "abandoned"
	StateIndeterminate = "indeterminate"
)

// Overall and phase statuses.
const (
	StatusPassed      = "passed"
	StatusFailed      = "failed"
	StatusInterrupted = "interrupted"
	StatusNotRun      = "not_run"
	StatusTimedOut    = "timed_out"
	StatusUnverified  = "unverified"
	StatusNotNeeded   = "not_needed"
	StatusNotConfig   = "not_configured"
)

// Phases.
const (
	PhaseValidation = "validation"
	PhaseAcquiring  = "acquiring"
	PhaseStarting   = "starting"
	PhaseWaiting    = "waiting_ready"
	PhaseChecking   = "checking"
	PhaseCleaningUp = "cleaning_up"
	PhaseCompleted  = "completed"
)

// Stable v1 error codes.
const (
	CodeUsage               = "usage_error"
	CodeInvalidConfig       = "invalid_configuration"
	CodeSessionOccupied     = "session_occupied"
	CodeResourceUnavailable = "resource_unavailable"
	CodeStartupFailed       = "startup_failed"
	CodeStartupTimeout      = "startup_timeout"
	CodeCheckStartFailed    = "check_start_failed"
	CodeCheckExitNonzero    = "check_exit_nonzero"
	CodeCheckTimeout        = "check_timeout"
	CodeRequiredFailed      = "required_process_failed"
	CodeInterrupted         = "interrupted"
	CodeCleanupUnverified   = "cleanup_unverified"
	CodeTeardownFailed      = "teardown_failed"
	CodeReceiptPersistence  = "receipt_persistence_failed"
	CodeResultSizeLimit     = "result_size_limit"
	CodeEvidenceUnavailable = "evidence_unavailable"
	// The check's environment report found a mismatch.
	CodeEnvironmentIneligible = "environment_ineligible"
	// The environment report was invalid, or required and missing or empty.
	CodeEnvironmentUnverified = "environment_unverified"
)

// Environment statuses. A check that never started leaves StatusNotRun.
const (
	EnvironmentEligible   = "eligible"
	EnvironmentIneligible = "ineligible"
	EnvironmentUnknown    = "unknown"
)

// Reasons an environment is unknown.
const (
	ReasonReportMissing  = "report_missing"
	ReasonReportInvalid  = "report_invalid"
	ReasonNoObservations = "no_observations"
)

// Observation statuses, as reported by the check.
const (
	ObservationMatched    = "matched"
	ObservationMismatched = "mismatched"
	ObservationUnobserved = "unobserved"
)

// Exit codes.
const (
	ExitPassed      = 0
	ExitFailed      = 1
	ExitUsage       = 2
	ExitInterrupted = 130
)

// Precedence bases.
const (
	PrecedencePassed         = "all_required_phases_passed"
	PrecedenceFailure        = "failure_without_interruption"
	PrecedenceInterrupted    = "interruption"
	PrecedencePreAcquisition = "pre_acquisition_failure"
)

// ErrorRecord is a stable machine code and its positive occurrence count.
type ErrorRecord struct {
	Code  string `json:"code"`
	Count int    `json:"count"`
}

// Errors aggregates codes into sorted {code,count} records.
type Errors map[string]int

func (e Errors) Add(code string) { e[code]++ }

func (e Errors) Records() []ErrorRecord {
	out := make([]ErrorRecord, 0, len(e))
	for code, count := range e {
		if count > 0 {
			out = append(out, ErrorRecord{Code: code, Count: count})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Code < out[j].Code })
	return out
}

type Source struct {
	Revision         *string  `json:"revision"`
	Dirty            *bool    `json:"dirty"`
	ObservedAt       *string  `json:"observed_at"`
	LoadedCodeProven bool     `json:"loaded_code_proven"`
	Limitations      []string `json:"limitations"`
}

type ProcessGeneration struct {
	Name       string `json:"name"`
	Generation uint64 `json:"generation"`
}

type Endpoint struct {
	Port int    `json:"port"`
	URL  string `json:"url"`
}

type ReadinessObservation struct {
	Process    string  `json:"process"`
	Generation uint64  `json:"generation"`
	Probe      *string `json:"probe"`
	Status     string  `json:"status"`
	ObservedAt *string `json:"observed_at"`
}

type Startup struct {
	Status                string                 `json:"status"`
	StartedAt             *string                `json:"started_at"`
	FinishedAt            *string                `json:"finished_at"`
	DeadlineAt            *string                `json:"deadline_at"`
	Validation            string                 `json:"validation"`
	ReadinessObservations []ReadinessObservation `json:"readiness_observations"`
	UnprobedProcesses     []string               `json:"unprobed_processes"`
	Errors                []ErrorRecord          `json:"errors"`
}

type Check struct {
	Status          string   `json:"status"`
	Generation      *uint64  `json:"generation"`
	Argv            []string `json:"argv"`
	ArgvSHA256      *string  `json:"argv_sha256"`
	ArgvBytes       int      `json:"argv_bytes"`
	Cwd             *string  `json:"cwd"`
	StartedAt       *string  `json:"started_at"`
	FinishedAt      *string  `json:"finished_at"`
	DeadlineAt      *string  `json:"deadline_at"`
	ExitCode        *int     `json:"exit_code"`
	Signal          *string  `json:"signal"`
	TimedOut        bool     `json:"timed_out"`
	Stdout          string   `json:"stdout"`
	Stderr          string   `json:"stderr"`
	OutputTruncated bool     `json:"output_truncated"`
}

// Environment is the check's own report of whether the attempt exercised the
// environment it intended: build, endpoints, data, fixtures. proc-compose
// records the report and enforces its verdict; the observations are the
// application's, not proof by this tool.
type Environment struct {
	Required     bool          `json:"required"`
	Status       string        `json:"status"`
	Reason       *string       `json:"reason"`
	Observations []Observation `json:"observations"`
}

// Observation is one named fact the check compared, such as the build an
// endpoint reports or the fixture a query reads back.
type Observation struct {
	Name     string  `json:"name"`
	Status   string  `json:"status"`
	Expected *string `json:"expected"`
	Observed *string `json:"observed"`
}

type UnresolvedProcess struct {
	Name       string  `json:"name"`
	Generation *uint64 `json:"generation"`
	Identity   string  `json:"identity"`
}

type Cleanup struct {
	Status              string              `json:"status"`
	Attempted           bool                `json:"attempted"`
	StartedAt           *string             `json:"started_at"`
	FinishedAt          *string             `json:"finished_at"`
	DeadlineAt          *string             `json:"deadline_at"`
	SignalingAuthority  string              `json:"signaling_authority"`
	TerminationVerified *bool               `json:"termination_verified"`
	UnresolvedProcesses []UnresolvedProcess `json:"unresolved_processes"`
	Errors              []ErrorRecord       `json:"errors"`
	// Teardown records each teardown process in run order; it is always a
	// list, empty when none is configured. Status and TerminationVerified
	// describe termination only.
	Teardown []TeardownRecord `json:"teardown"`
}

// TeardownRecord is one teardown process's outcome. Status is one of the
// runner's teardown statuses: passed, failed, timed_out, handed_off, not_run,
// or skipped.
type TeardownRecord struct {
	Name       string  `json:"name"`
	Status     string  `json:"status"`
	ExitCode   *int    `json:"exit_code"`
	StartedAt  *string `json:"started_at"`
	FinishedAt *string `json:"finished_at"`
	Error      string  `json:"error,omitempty"`
}

// EvidenceEvent is a bounded excerpt of one journal event.
type EvidenceEvent struct {
	ID         string `json:"id"`
	Process    string `json:"process"`
	Generation uint64 `json:"generation"`
	Level      string `json:"level"`
	Code       string `json:"code"`
	Message    string `json:"message"`
}

type Evidence struct {
	Journal       *string         `json:"journal"`
	FirstCursor   *string         `json:"first_cursor"`
	LastCursor    *string         `json:"last_cursor"`
	Gap           bool            `json:"gap"`
	Truncated     bool            `json:"truncated"`
	ExcerptStatus string          `json:"excerpt_status"`
	Events        []EvidenceEvent `json:"events"`
}

// Owner identifies the CLI process holding an attempt, for abandonment
// classification. It is not proof of liveness on its own.
type Owner struct {
	PID      int    `json:"pid"`
	Hostname string `json:"hostname"`
}

// Result is the full v1 response and receipt schema. Every field is always
// present; nullable fields encode as JSON null.
type Result struct {
	SchemaVersion     int                 `json:"schema_version"`
	Kind              string              `json:"kind"`
	State             string              `json:"state"`
	AttemptID         *string             `json:"attempt_id"`
	Status            string              `json:"status"`
	Phase             string              `json:"phase"`
	Session           string              `json:"session"`
	RunID             *string             `json:"run_id"`
	ConfigPath        string              `json:"config_path"`
	ConfigSHA256      *string             `json:"config_sha256"`
	Workdir           string              `json:"workdir"`
	Source            Source              `json:"source"`
	ToolVersion       string              `json:"tool_version"`
	Owner             *Owner              `json:"owner"`
	SelectedProcesses []ProcessGeneration `json:"selected_processes"`
	Endpoints         map[string]Endpoint `json:"endpoints"`
	EndpointsNote     *string             `json:"endpoints_not_available"`
	Startup           Startup             `json:"startup"`
	Check             Check               `json:"check"`
	Environment       Environment         `json:"environment"`
	Cleanup           Cleanup             `json:"cleanup"`
	Evidence          Evidence            `json:"evidence"`
	Errors            []ErrorRecord       `json:"errors"`
	ExitCode          int                 `json:"exit_code"`
	Precedence        string              `json:"precedence"`
	ReceiptPath       *string             `json:"receipt_path"`
	ReceiptPersisted  bool                `json:"receipt_persisted"`
	Retention         string              `json:"retention"`
	StartedAt         string              `json:"started_at"`
	FinishedAt        *string             `json:"finished_at"`
}

// CompactError is the response-only form used when the full result cannot
// fit the size limit. It never represents success and is never persisted.
type CompactError struct {
	SchemaVersion            int               `json:"schema_version"`
	Kind                     string            `json:"kind"`
	Status                   string            `json:"status"`
	Phase                    string            `json:"phase"`
	ExitCode                 int               `json:"exit_code"`
	AttemptState             *string           `json:"attempt_state"`
	PhaseStatuses            map[string]string `json:"phase_statuses"`
	Errors                   []ErrorRecord     `json:"errors"`
	Identity                 CompactIdentity   `json:"identity"`
	TerminalReceiptPersisted bool              `json:"terminal_receipt_persisted"`
}

type CompactIdentity struct {
	ConfigSHA256                      *string  `json:"config_sha256"`
	SessionSHA256                     *string  `json:"session_sha256"`
	RunIDSHA256                       *string  `json:"run_id_sha256"`
	AttemptIDSHA256                   *string  `json:"attempt_id_sha256"`
	ArgvSHA256                        *string  `json:"argv_sha256"`
	SelectionEndpointGenerationSHA256 *string  `json:"selection_endpoint_generation_sha256"`
	Complete                          bool     `json:"complete"`
	OmittedFields                     []string `json:"omitted_fields"`
}

// MaxResultBytes bounds the exact UTF-8 bytes of every serialized result.
const MaxResultBytes = 64 << 10

// Per-field encoded contribution caps.
const (
	MaxArgvBytes      = 16 << 10
	MaxSelectionBytes = 8 << 10
	MaxEndpointBytes  = 8 << 10
	MaxOutputBytes    = 4 << 10
	// MaxEnvironmentBytes bounds the environment report file and its
	// observations as encoded in the result.
	MaxEnvironmentBytes = 8 << 10
	MaxObservations     = 32
)

func digest(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func digestPtr(data []byte) *string { d := digest(data); return &d }

func stringDigest(value *string) *string {
	if value == nil {
		return nil
	}
	return digestPtr([]byte(*value))
}

// ArgvDigest is SHA-256 over compact UTF-8 JSON of the exact argv, plus its
// encoded byte count.
func ArgvDigest(argv []string) (string, int) {
	data, _ := json.Marshal(argv)
	return digest(data), len(data)
}

func timestamp(t time.Time) *string {
	if t.IsZero() {
		return nil
	}
	s := t.UTC().Format(time.RFC3339Nano)
	return &s
}

func ptr[T any](v T) *T { return &v }

// Encode serializes a full result, dropping optional evidence excerpts first and
// falling back to the compact form when required content cannot fit.
func Encode(result *Result) ([]byte, bool, error) {
	data, err := json.Marshal(result)
	if err != nil {
		return nil, false, err
	}
	if len(data) <= MaxResultBytes {
		return data, false, nil
	}
	trimmed := *result
	trimmed.Evidence.Events = []EvidenceEvent{}
	trimmed.Evidence.Truncated = true
	trimmed.Check.Stdout, trimmed.Check.Stderr, trimmed.Check.OutputTruncated = "", "", true
	if data, err = json.Marshal(&trimmed); err == nil && len(data) <= MaxResultBytes {
		*result = trimmed
		return data, false, nil
	}
	compact := Compact(result)
	data, err = json.Marshal(compact)
	if err != nil {
		return nil, true, err
	}
	return data, true, nil
}

// Compact derives the compact error form from a result that cannot be emitted.
func Compact(result *Result) CompactError {
	errs := Errors{}
	for _, record := range result.Errors {
		errs[record.Code] += record.Count
	}
	errs.Add(CodeResultSizeLimit)
	status := result.Status
	if status == StatusPassed {
		status = StatusFailed
	}
	var exit int
	acquired := result.RunID != nil
	switch {
	case status == StatusInterrupted:
		exit = ExitInterrupted
	case acquired:
		exit = ExitFailed
	default:
		exit = ExitUsage
	}
	var attemptState *string
	if acquired {
		attemptState = ptr(StateIndeterminate)
	}
	validation := StatusPassed
	if result.Startup.Validation == StatusFailed {
		validation = StatusFailed
	}
	var argvDigest *string
	if result.Check.ArgvSHA256 != nil {
		argvDigest = result.Check.ArgvSHA256
	}
	session := result.Session
	return CompactError{
		SchemaVersion: SchemaVersion,
		Kind:          KindCompactError,
		Status:        status,
		Phase:         result.Phase,
		ExitCode:      exit,
		AttemptState:  attemptState,
		PhaseStatuses: map[string]string{"validation": validation, "startup": result.Startup.Status, "check": result.Check.Status, "environment": result.Environment.Status, "cleanup": result.Cleanup.Status},
		Errors:        errs.Records(),
		Identity: CompactIdentity{
			ConfigSHA256:                      result.ConfigSHA256,
			SessionSHA256:                     stringDigest(&session),
			RunIDSHA256:                       stringDigest(result.RunID),
			AttemptIDSHA256:                   stringDigest(result.AttemptID),
			ArgvSHA256:                        argvDigest,
			SelectionEndpointGenerationSHA256: selectionDigest(result),
			Complete:                          false,
			OmittedFields:                     []string{"config_path", "workdir", "session", "run_id", "attempt_id", "argv", "selected_processes", "process_generations", "endpoints", "source", "tool_version", "startup", "check", "environment", "cleanup", "evidence", "receipt_path", "retention", "started_at", "finished_at"},
		},
	}
}

// selectionDigest covers canonical JSON of ordered selection, endpoints, and
// generations. encoding/json sorts map keys, which makes the map canonical.
func selectionDigest(result *Result) *string {
	data, err := json.Marshal(struct {
		Selection []ProcessGeneration    `json:"selection"`
		Endpoints map[string]Endpoint    `json:"endpoints"`
		Readiness []ReadinessObservation `json:"readiness"`
	}{result.SelectedProcesses, result.Endpoints, result.Startup.ReadinessObservations})
	if err != nil {
		return nil
	}
	return digestPtr(data)
}
