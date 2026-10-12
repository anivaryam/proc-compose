package verify

import (
	"errors"
	"os"
	"sort"

	"github.com/anivaryam/proc-compose/internal/ipc"
	"github.com/anivaryam/proc-compose/internal/logs"
)

// Diagnosis bounds limits.
const (
	maxDiagnosisEvents   = 20000
	maxGroupsPerProcess  = 20
	maxTransitionsPerGen = 20
	maxDiagnosisLine     = 512
)

// Diagnosis explains one attempt from its receipt and the journal events that
// carry its attempt identity. It reports observations and temporal order only;
// it never labels a business-level cause.
type Diagnosis struct {
	SchemaVersion  int               `json:"schema_version"`
	Kind           string            `json:"kind"`
	AttemptID      string            `json:"attempt_id"`
	AttemptState   string            `json:"attempt_state"`
	Status         string            `json:"status"`
	RunID          *string           `json:"run_id"`
	Phases         map[string]string `json:"phases"`
	Errors         []ErrorRecord     `json:"errors"`
	Boundary       string            `json:"boundary"`
	Processes      []ProcessHistory  `json:"processes"`
	Check          *Check            `json:"check"`
	Journal        *string           `json:"journal"`
	EventsScanned  int               `json:"events_scanned"`
	Gap            bool              `json:"gap"`
	Truncated      bool              `json:"truncated"`
	HistoryMissing bool              `json:"history_missing"`
	Limitations    []string          `json:"limitations"`
}

type ProcessHistory struct {
	Name        string        `json:"name"`
	Generations []uint64      `json:"generations"`
	DependsOn   []string      `json:"depends_on"`
	Transitions []Transition  `json:"transitions"`
	Output      []OutputGroup `json:"output"`
	Truncated   bool          `json:"truncated"`
}

type Transition struct {
	EventID    string `json:"event_id"`
	Generation uint64 `json:"generation"`
	State      string `json:"state"`
	Ready      bool   `json:"ready"`
	LastError  string `json:"last_error,omitempty"`
	ExitCode   *int   `json:"exit_code"`
	At         string `json:"at"`
}

// OutputGroup collapses identical consecutive lines of one process
// generation and stream. FirstID/LastID locate the originals for
// `proc-compose logs --after`.
type OutputGroup struct {
	Generation uint64 `json:"generation"`
	Stream     string `json:"stream"`
	Level      string `json:"level"`
	Code       string `json:"code"`
	Message    string `json:"message"`
	Count      int    `json:"count"`
	FirstID    string `json:"first_id"`
	LastID     string `json:"last_id"`
}

// Diagnose builds a bounded diagnosis. processes, when non-empty, limits the
// result to those processes plus dependencies observed in the journal.
func Diagnose(receipt *Result, state string, processes []string) Diagnosis {
	d := Diagnosis{
		SchemaVersion: SchemaVersion,
		Kind:          "verify.diagnosis",
		AttemptState:  state,
		Status:        receipt.Status,
		RunID:         receipt.RunID,
		Phases:        map[string]string{"validation": receipt.Startup.Validation, "startup": receipt.Startup.Status, "check": receipt.Check.Status, "cleanup": receipt.Cleanup.Status},
		Errors:        receipt.Errors,
		Boundary:      "unavailable",
		Processes:     []ProcessHistory{},
		Check:         &receipt.Check,
		Journal:       receipt.Evidence.Journal,
		Limitations:   []string{"temporal_association_not_causation"},
	}
	if receipt.AttemptID != nil {
		d.AttemptID = *receipt.AttemptID
	}
	if receipt.Errors == nil {
		d.Errors = []ErrorRecord{}
	}
	if state != StateCompleted {
		d.Limitations = append(d.Limitations, "attempt_not_terminal")
	}
	if receipt.Journal() == "" || receipt.RunID == nil {
		d.HistoryMissing = true
		d.Limitations = append(d.Limitations, "journal_unavailable")
		return d
	}
	events, gap, truncated, err := scanAttempt(receipt.Journal(), logs.Options{AttemptID: d.AttemptID})
	d.Boundary = "attempt_id"
	if err == nil && len(events) == 0 {
		// Journals written before attempt identity existed: fall back to the
		// run, which this attempt owned exclusively.
		events, gap, truncated, err = scanAttempt(receipt.Journal(), logs.Options{RunID: *receipt.RunID})
		d.Boundary = "run_id"
		d.Limitations = append(d.Limitations, "attempt_boundary_inferred_from_run")
	}
	if err != nil || len(events) == 0 {
		d.Boundary = "unavailable"
		d.HistoryMissing = true
		d.Limitations = append(d.Limitations, "history_rotated_or_removed")
		return d
	}
	d.EventsScanned, d.Gap, d.Truncated = len(events), gap || receipt.Evidence.Gap, truncated
	if d.Gap {
		d.Limitations = append(d.Limitations, "journal_gap")
	}
	if first := receipt.Evidence.FirstCursor; first != nil && events[0].ID != *first {
		d.HistoryMissing = true
		d.Limitations = append(d.Limitations, "earliest_events_rotated")
	}
	d.Processes = histories(events, processes)
	for _, p := range d.Processes {
		d.Truncated = d.Truncated || p.Truncated
	}
	return d
}

// Journal returns the receipt's journal path, or "".
func (r *Result) Journal() string {
	if r.Evidence.Journal == nil {
		return ""
	}
	return *r.Evidence.Journal
}

func scanAttempt(path string, opts logs.Options) ([]ipc.LogEntry, bool, bool, error) {
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return nil, false, false, err
	}
	opts.Limit, opts.MaxBytes, opts.After = 1000, 1<<20, "start"
	var out []ipc.LogEntry
	gap := false
	for {
		page, err := logs.Query(path, opts)
		if err != nil {
			return out, gap, false, err
		}
		gap = gap || page.Gap
		out = append(out, page.Events...)
		if len(out) >= maxDiagnosisEvents {
			return out[:maxDiagnosisEvents], gap, true, nil
		}
		if !page.HasMore || page.NextCursor == "" || page.NextCursor == opts.After {
			return out, gap, false, nil
		}
		opts.After = page.NextCursor
	}
}

func histories(events []ipc.LogEntry, filter []string) []ProcessHistory {
	byName := map[string]*ProcessHistory{}
	var order []string
	get := func(name string) *ProcessHistory {
		h, ok := byName[name]
		if !ok {
			h = &ProcessHistory{Name: name, Generations: []uint64{}, DependsOn: []string{}, Transitions: []Transition{}, Output: []OutputGroup{}}
			byName[name] = h
			order = append(order, name)
		}
		return h
	}
	for _, event := range events {
		if event.Process == "" || event.Process == "proc-compose" {
			continue
		}
		h := get(event.Process)
		if n := len(h.Generations); event.Generation != 0 && (n == 0 || h.Generations[n-1] != event.Generation) {
			h.Generations = append(h.Generations, event.Generation)
		}
		if event.State != nil {
			if len(event.State.DependsOn) > 0 {
				h.DependsOn = event.State.DependsOn
			}
			h.Transitions = append(h.Transitions, Transition{EventID: event.ID, Generation: event.State.Generation, State: event.State.State, Ready: event.State.Ready, LastError: event.State.LastError, ExitCode: event.State.ExitCode, At: event.Time.UTC().Format("2006-01-02T15:04:05.000Z07:00")})
			if len(h.Transitions) > maxTransitionsPerGen*max(1, len(h.Generations)) {
				h.Transitions = h.Transitions[1:]
				h.Truncated = true
			}
			continue
		}
		if event.Level == "debug" {
			continue
		}
		message := truncateUTF8(event.Line, maxDiagnosisLine)
		if n := len(h.Output); n > 0 {
			last := &h.Output[n-1]
			if last.Generation == event.Generation && last.Stream == event.Stream && last.Message == message && last.Code == event.Code {
				last.Count++
				last.LastID = event.ID
				continue
			}
		}
		h.Output = append(h.Output, OutputGroup{Generation: event.Generation, Stream: event.Stream, Level: event.Level, Code: event.Code, Message: message, Count: 1, FirstID: event.ID, LastID: event.ID})
		if len(h.Output) > maxGroupsPerProcess {
			h.Output = h.Output[1:]
			h.Truncated = true
		}
	}
	include := map[string]bool{}
	if len(filter) > 0 {
		var visit func(string)
		visit = func(name string) {
			if include[name] {
				return
			}
			include[name] = true
			if h, ok := byName[name]; ok {
				for _, dep := range h.DependsOn {
					visit(dep)
				}
			}
		}
		for _, name := range filter {
			visit(name)
		}
	}
	out := []ProcessHistory{}
	for _, name := range order {
		if len(filter) == 0 || include[name] {
			out = append(out, *byName[name])
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
