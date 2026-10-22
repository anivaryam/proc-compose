// Package logs reads bounded, resumable pages from the supervisor's event journal.
package logs

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/anivaryam/proc-compose/internal/ipc"
)

type Options struct {
	Limit    int
	MaxBytes int
	After    string
	// Tail selects the newest matching events instead of the oldest ones.
	// It applies whenever it is set, with or without a cursor: the window
	// is the newest Limit events after the cursor, or the newest Limit
	// events of the whole journal when there is no cursor.
	//
	// Under tail:
	//   - has_more is always false. A tail page is the end of the window,
	//     not a forward page that can be continued.
	//   - next_cursor is the last event scanned, not the last event
	//     returned. It advances over events that did not match the filters
	//     as well, so resuming from it never replays a scanned event.
	//   - truncated is true when older events were dropped to fit the
	//     event limit or the byte budget, and also when a single event had
	//     to be shortened to fit the budget.
	Tail       bool
	Processes  []string
	Level      string
	Kind       string
	Code       string
	Stream     string
	Search     string
	Since      time.Time
	RunID      string
	AttemptID  string
	Generation uint64
}

type Page struct {
	Version    int            `json:"schema_version"`
	Events     []ipc.LogEntry `json:"events"`
	NextCursor string         `json:"next_cursor,omitempty"`
	HasMore    bool           `json:"has_more"`
	Gap        bool           `json:"gap"`
	Truncated  bool           `json:"truncated"`
	EventBytes int            `json:"event_bytes"`
}

func Query(path string, opts Options) (Page, error) {
	page := Page{Version: 1, Events: []ipc.LogEntry{}, NextCursor: opts.After}
	if opts.Limit == 0 {
		opts.Limit = 50
	}
	if opts.MaxBytes == 0 {
		opts.MaxBytes = 64 << 10
	}
	if opts.Limit < 1 || opts.Limit > 1000 || opts.MaxBytes < 1024 || opts.MaxBytes > 1<<20 {
		return page, fmt.Errorf("log pages require 1..1000 events and 1024..1048576 event bytes")
	}
	if opts.After != "" && opts.After != "start" {
		parts := strings.Split(opts.After, ":")
		if len(parts) != 2 || parts[0] == "" {
			return page, fmt.Errorf("invalid log cursor")
		}
		if _, err := strconv.ParseUint(parts[1], 10, 64); err != nil {
			return page, fmt.Errorf("invalid log cursor: %w", err)
		}
	}
	found := opts.After == "" || opts.After == "start"
	if opts.After == "start" {
		page.NextCursor = ""
	}
	files, err := snapshotFiles(path)
	if err != nil {
		return page, err
	}
	defer func() {
		for _, file := range files {
			file.file.Close()
		}
	}()
	var tailSizes []int
	var previous ipc.LogEntry
	for _, retained := range files {
		file, info, filePath := retained.file, retained.info, retained.file.Name()
		// Only committed complete records in this snapshot are visible. A
		// concurrently appended partial line is read on the next query.
		scanner := bufio.NewScanner(io.LimitReader(file, info.Size()))
		scanner.Buffer(make([]byte, 64<<10), ipc.MaxEventBytes)
		scanner.Split(completeLines)
		for scanner.Scan() {
			var event ipc.LogEntry
			if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
				file.Close()
				return page, fmt.Errorf("invalid journal record in %s: %w", filePath, err)
			}
			if event.Version != 1 || event.ID == "" {
				file.Close()
				return page, fmt.Errorf("unsupported journal event schema in %s", filePath)
			}
			if previous.RunID == event.RunID && previous.Sequence != 0 && event.Sequence != previous.Sequence+1 {
				page.Gap = true
			}
			previous = event
			if !found {
				if event.ID == opts.After {
					found = true
					page.NextCursor = event.ID
				}
				continue
			}
			if !matches(event, opts) {
				page.NextCursor = event.ID
				continue
			}
			if opts.Tail {
				bounded, size, err := fit(event, opts.MaxBytes)
				if err != nil {
					file.Close()
					return page, err
				}
				page.Events = append(page.Events, bounded)
				tailSizes = append(tailSizes, size)
				page.EventBytes += size
				page.Truncated = page.Truncated || bounded.Truncated
				for len(page.Events) > opts.Limit || page.EventBytes > opts.MaxBytes {
					page.EventBytes -= tailSizes[0]
					page.Events[0] = ipc.LogEntry{}
					page.Events, tailSizes = page.Events[1:], tailSizes[1:]
					page.Truncated = true
				}
				page.NextCursor = event.ID
				continue
			}
			if len(page.Events) == opts.Limit {
				page.HasMore = true
				file.Close()
				return page, nil
			}
			remaining := opts.MaxBytes - page.EventBytes
			if remaining < 1024 && len(page.Events) > 0 {
				page.HasMore = true
				file.Close()
				return page, nil
			}
			bounded, size, err := fit(event, remaining)
			if err != nil {
				file.Close()
				return page, err
			}
			page.Events = append(page.Events, bounded)
			page.EventBytes += size
			page.Truncated = page.Truncated || bounded.Truncated
			page.NextCursor = event.ID
		}
		err = scanner.Err()
		file.Close()
		if err != nil {
			return page, err
		}
	}
	if !found {
		opts.After, opts.Tail = "", true
		fallback, err := Query(path, opts)
		fallback.Gap = true
		return fallback, err
	}
	return page, nil
}

type retainedFile struct {
	file *os.File
	info os.FileInfo
}

// snapshotAttempts bounds how many times the reader re-pins the journal family
// before it reports the journal as unavailable.
const snapshotAttempts = 5

// snapshotRetryDelay is the pause between two pinning attempts.
//
// It is a variable so a test can widen the window a reader waits out. A
// rotation replaces the live journal in microseconds, so the production value
// is far longer than the window it has to cover.
var snapshotRetryDelay = time.Millisecond

// Pin a coherent set of file handles before scanning. Opening each rotated
// pathname only after reading the previous one can replay or skip entire files
// if rotation occurs during the query. Validate names after opening and retry
// a bounded number of times; a busy journal returns an explicit retryable error.
//
// A rotation renames the live journal aside before it installs the replacement,
// so a reader that opens the journal in that window finds the live path
// missing. That absence is a rotation in flight, not a missing journal: the
// snapshot is taken again instead of reporting that the journal does not
// exist. The journal is reported as gone only when no file of its family is
// left.
func snapshotFiles(path string) ([]retainedFile, error) {
	for attempt := 0; attempt < snapshotAttempts; attempt++ {
		if attempt > 0 {
			time.Sleep(snapshotRetryDelay)
		}
		var files []retainedFile
		observed := make(map[string]os.FileInfo)
		var openErr error
		liveMissing := false
		for index := 3; index >= 0; index-- {
			name := path
			if index != 0 {
				name = fmt.Sprintf("%s.%d", path, index)
			}
			file, err := openJournal(name)
			// errors.Is, not os.IsNotExist: on Windows a name that rotation is
			// deleting at this moment also reports as not existing.
			if errors.Is(err, fs.ErrNotExist) {
				observed[name] = nil
				if index == 0 {
					liveMissing = true
				}
				continue
			}
			if err != nil {
				openErr = err
				break
			}
			info, err := file.Stat()
			if err != nil {
				file.Close()
				openErr = err
				break
			}
			observed[name] = info
			files = append(files, retainedFile{file: file, info: info})
		}
		// The live journal is always there: the writer creates it with the
		// session and a rotation replaces it within the same operation. A
		// missing live file therefore means a rotation is in flight, so the
		// snapshot is not coherent and has to be taken again.
		stable := openErr == nil && !liveMissing
		if stable {
			for name, info := range observed {
				current, err := os.Stat(name)
				if info == nil && os.IsNotExist(err) {
					continue
				}
				if info == nil || err != nil || !os.SameFile(info, current) {
					stable = false
					break
				}
			}
		}
		if stable {
			return files, nil
		}
		for _, file := range files {
			file.file.Close()
		}
		if openErr != nil {
			return nil, openErr
		}
	}
	// The live journal stayed unavailable for the whole budget. It is really
	// gone only when no file of its family is left; anything else means a
	// rotation is in flight and the query has to be retried.
	if !journalFamilyPresent(path) {
		return nil, os.ErrNotExist
	}
	return nil, fmt.Errorf("journal rotated repeatedly while opening history; retry this query with the same cursor")
}

// journalFamilyPresent reports whether any file of the journal family is still
// on disk.
//
// The live journal is renamed aside while it is rotated, so the rotated copies
// and the writer's staged file are the evidence that the journal exists and
// that the query has to be read again rather than reported as missing.
func journalFamilyPresent(path string) bool {
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		return false
	}
	prefix := filepath.Base(path) + "."
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), prefix) {
			return true
		}
	}
	return false
}

func completeLines(data []byte, atEOF bool) (int, []byte, error) {
	if index := bytes.IndexByte(data, '\n'); index >= 0 {
		return index + 1, data[:index], nil
	}
	return 0, nil, nil
}

func matches(event ipc.LogEntry, opts Options) bool {
	if len(opts.Processes) > 0 {
		found := false
		for _, name := range opts.Processes {
			if name == event.Process {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return (opts.Level == "" || opts.Level == event.Level) &&
		(opts.Kind == "" || opts.Kind == event.Kind) &&
		(opts.Code == "" || opts.Code == event.Code) &&
		(opts.Stream == "" || opts.Stream == event.Stream) &&
		(opts.RunID == "" || opts.RunID == event.RunID) &&
		(opts.AttemptID == "" || opts.AttemptID == event.AttemptID) &&
		(opts.Generation == 0 || opts.Generation == event.Generation) &&
		(opts.Since.IsZero() || !event.Time.Before(opts.Since)) &&
		(opts.Search == "" || strings.Contains(strings.ToLower(event.Line), strings.ToLower(opts.Search)))
}

// Truncation affects a query copy, never the persisted record or cursor. Retain
// identity even when an unusually large state payload must be omitted.
func fit(event ipc.LogEntry, budget int) (ipc.LogEntry, int, error) {
	encoded, err := json.Marshal(event)
	if err != nil {
		return event, 0, err
	}
	if len(encoded) <= budget {
		return event, len(encoded), nil
	}
	event.Truncated = true
	if event.OriginalBytes == 0 {
		event.OriginalBytes = len(event.Line)
	}
	event.State = nil
	message := event.Line
	low, high := 0, len(message)
	for low < high {
		middle := (low + high + 1) / 2
		prefix := message[:middle]
		for len(prefix) > 0 && !utf8.ValidString(prefix) {
			prefix = prefix[:len(prefix)-1]
		}
		event.Line = prefix
		data, _ := json.Marshal(event)
		if len(data) <= budget {
			low = middle
		} else {
			high = middle - 1
		}
	}
	event.Line = message[:low]
	for len(event.Line) > 0 && !utf8.ValidString(event.Line) {
		event.Line = event.Line[:len(event.Line)-1]
	}
	encoded, err = json.Marshal(event)
	if len(encoded) > budget {
		return event, 0, fmt.Errorf("log event metadata exceeds the byte budget")
	}
	return event, len(encoded), err
}

func Text(event ipc.LogEntry) string {
	return fmt.Sprintf("%s %-12s #%d %-5s [%s/%s] %s", event.Time.Local().Format("15:04:05.000"), event.Process, event.Generation, event.Level, event.Kind, event.Stream, event.Line)
}
