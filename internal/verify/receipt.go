package verify

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
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

// ReadReceipt loads one receipt.
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
