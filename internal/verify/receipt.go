package verify

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/anivaryam/proc-compose/internal/daemon"
)

// RetainedTerminalReceipts is how many terminal receipts a session keeps.
// Active and indeterminate receipts are never pruned.
const RetainedTerminalReceipts = 20

// Retention describes receipt durability; runtime directories can vanish at
// logout, reboot, or temporary-directory cleanup.
const Retention = "runtime-directory; may disappear at logout/reboot"

// AttemptsDir is where a session's receipts live.
func AttemptsDir(sessionDir string) string { return filepath.Join(sessionDir, "attempts") }

// environmentReportPath is where one attempt's check may write its
// environment report. It is not a .json file, so ListReceipts skips it.
func environmentReportPath(sessionDir, attemptID string) string {
	return filepath.Join(AttemptsDir(sessionDir), attemptID+".environment")
}

// environmentReport is the file a check writes to
// PROC_COMPOSE_ENVIRONMENT_REPORT.
type environmentReport struct {
	Observations []Observation `json:"observations"`
}

// readEnvironmentReport loads and validates a check's environment report. A
// missing file is os.ErrNotExist. Anything but a regular file holding one JSON
// object, repeated or unknown fields, non-string values, unknown statuses, and
// empty or duplicate names make the report invalid, so a typo cannot pass as
// evidence.
func readEnvironmentReport(path string) ([]Observation, error) {
	// Opening a FIFO or device the check left there could block or never end.
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("report is not a regular file (%s)", info.Mode().Type())
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, MaxEnvironmentBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > MaxEnvironmentBytes {
		return nil, fmt.Errorf("report exceeds %d bytes", MaxEnvironmentBytes)
	}
	// Decoding null into a struct is a silent no-op.
	if trimmed := bytes.TrimLeft(data, " \t\r\n"); len(trimmed) == 0 || trimmed[0] != '{' {
		return nil, errors.New("report must be a JSON object")
	}
	if err := uniqueMembers(json.NewDecoder(bytes.NewReader(data))); err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var report environmentReport
	if err := dec.Decode(&report); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("report must be exactly one JSON object")
	}
	if len(report.Observations) > MaxObservations {
		return nil, fmt.Errorf("report has %d observations, above the limit of %d", len(report.Observations), MaxObservations)
	}
	seen := map[string]bool{}
	for _, o := range report.Observations {
		if o.Name == "" || seen[o.Name] {
			return nil, fmt.Errorf("observation name %q is empty or repeated", o.Name)
		}
		seen[o.Name] = true
		switch o.Status {
		case ObservationMatched, ObservationMismatched, ObservationUnobserved:
		default:
			return nil, fmt.Errorf("observation %q has status %q, want matched, mismatched, or unobserved", o.Name, o.Status)
		}
	}
	// Escaping can grow values when they are re-encoded into the result.
	if encoded, _ := json.Marshal(report.Observations); len(encoded) > MaxEnvironmentBytes {
		return nil, fmt.Errorf("observations exceed %d bytes when encoded", MaxEnvironmentBytes)
	}
	return report.Observations, nil
}

// uniqueMembers reads one JSON value and fails if any object in it repeats a
// member. encoding/json keeps the last value and matches names
// case-insensitively, so a later "status" or "Status" could turn a mismatch
// into a match.
func uniqueMembers(dec *json.Decoder) error {
	token, err := dec.Token()
	if err != nil {
		return err
	}
	switch token {
	case json.Delim('{'):
		var names []string
		for dec.More() {
			token, err := dec.Token()
			if err != nil {
				return err
			}
			name, _ := token.(string)
			for _, seen := range names {
				if strings.EqualFold(seen, name) {
					return fmt.Errorf("member %q is repeated", name)
				}
			}
			names = append(names, name)
			if err := uniqueMembers(dec); err != nil {
				return err
			}
		}
	case json.Delim('['):
		for dec.More() {
			if err := uniqueMembers(dec); err != nil {
				return err
			}
		}
	default:
		return nil
	}
	_, err = dec.Token() // the closing delimiter
	return err
}

const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// NewAttemptID returns a 26-character ULID: 48-bit millisecond time followed by
// 80 random bits, Crockford base32. Lexical order follows creation time.
func NewAttemptID(now time.Time) (string, error) {
	var raw [16]byte
	ms := uint64(now.UnixMilli())
	for i := 5; i >= 0; i-- {
		raw[i] = byte(ms)
		ms >>= 8
	}
	if _, err := rand.Read(raw[6:]); err != nil {
		return "", err
	}
	var out [26]byte
	// 128 bits -> 26 base32 digits (130 bits, top two bits zero).
	var hi, lo uint64
	for i := 0; i < 8; i++ {
		hi = hi<<8 | uint64(raw[i])
		lo = lo<<8 | uint64(raw[8+i])
	}
	for i := 25; i >= 0; i-- {
		out[i] = crockford[lo&31]
		lo = lo>>5 | hi<<59
		hi >>= 5
	}
	return string(out[:]), nil
}

// WriteReceipt atomically persists a receipt (temporary file plus rename), so a
// reader never observes a partial record.
func WriteReceipt(path string, result *Result) error {
	receipt := *result
	receipt.Kind = KindReceipt
	data, err := json.Marshal(&receipt)
	if err != nil {
		return err
	}
	if len(data) > MaxResultBytes {
		return fmt.Errorf("receipt is %d bytes, above the %d-byte limit", len(data), MaxResultBytes)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	return daemon.WriteFileAtomic(path, append(data, '\n'), 0600)
}

// ReadReceipt loads one receipt. A receipt that does not satisfy the receipt
// contract — a known attempt state and a known overall status — is refused
// like an unreadable file: the attempts listing skips it and Prune preserves
// it, so a corrupt or hand-made file is never consumed as an attempt.
func ReadReceipt(path string) (*Result, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var result Result
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, fmt.Errorf("receipt %s: %w", path, err)
	}
	if result.SchemaVersion != SchemaVersion || result.Kind != KindReceipt {
		return nil, fmt.Errorf("receipt %s: unsupported schema %d/%q", path, result.SchemaVersion, result.Kind)
	}
	if !knownState(result.State) {
		return nil, fmt.Errorf("receipt %s: state %q is not a known attempt state", path, result.State)
	}
	if !knownOverallStatus(result.Status) {
		return nil, fmt.Errorf("receipt %s: status %q is not a known overall status", path, result.Status)
	}
	return &result, nil
}

// ListReceipts returns receipt paths in a session's attempts directory, newest
// first. A missing directory is an empty list.
func ListReceipts(sessionDir string) ([]string, error) {
	entries, err := os.ReadDir(AttemptsDir(sessionDir))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		out = append(out, filepath.Join(AttemptsDir(sessionDir), entry.Name()))
	}
	sort.Sort(sort.Reverse(sort.StringSlice(out)))
	return out, nil
}

// Prune removes the oldest terminal receipts beyond the retention count. keep
// is the receipt just written; it is never removed. Unreadable, active, and
// indeterminate receipts are preserved.
func Prune(sessionDir, keep string) error {
	paths, err := ListReceipts(sessionDir)
	if err != nil {
		return err
	}
	terminal := 0
	var errs []error
	for _, path := range paths {
		receipt, readErr := ReadReceipt(path)
		if readErr != nil || receipt.State != StateCompleted {
			continue
		}
		terminal++
		if terminal <= RetainedTerminalReceipts || path == keep {
			continue
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// OwnerEvidence answers the observations classification requires.
// OwnerRunning is read-only evidence that the session runtime still names this
// owner as its live runner (for example, a start-time-checked PID file).
// LeaseHeld may acquire a free lease momentarily to test it, so it is consulted
// only after the owner is positively gone; nil means the caller must stay
// read-only, and abandonment then cannot be established.
type OwnerEvidence struct {
	OwnerAlive   func(Owner) (alive bool, known bool)
	OwnerRunning func(Owner) bool
	LeaseHeld    func() (held bool, err error)
}

// Classify reports a receipt's attempt state from positive evidence. A
// nonterminal receipt is active only while its recorded owner is alive and the
// session runtime still names it as the runner, and abandoned only when the
// owner is positively absent and the session's instance lease is no longer
// held; anything less certain is indeterminate. A live owner process alone is
// not enough: it can outlive its attempt. A timestamp, missing heartbeat, or
// missing metadata alone proves nothing.
func Classify(receipt *Result, evidence OwnerEvidence) string {
	if receipt.State != StateActive {
		return receipt.State
	}
	if receipt.Owner == nil || evidence.OwnerAlive == nil {
		return StateIndeterminate
	}
	alive, known := evidence.OwnerAlive(*receipt.Owner)
	switch {
	case !known:
		return StateIndeterminate
	case alive && evidence.OwnerRunning != nil && evidence.OwnerRunning(*receipt.Owner):
		return StateActive
	case alive || evidence.LeaseHeld == nil:
		return StateIndeterminate
	}
	if held, err := evidence.LeaseHeld(); err == nil && !held {
		return StateAbandoned
	}
	return StateIndeterminate
}
