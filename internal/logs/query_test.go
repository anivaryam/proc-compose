package logs

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/anivaryam/proc-compose/internal/ipc"
	"github.com/anivaryam/proc-compose/internal/logrotate"
)

func TestQuery_RotationPaginationAndCursorGap(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	writer, err := logrotate.New(path, 500, 3)
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 30; i++ {
		event := ipc.LogEntry{Version: 1, ID: fmt.Sprintf("run:%d", i), RunID: "run", Sequence: uint64(i), Time: time.Now(), Process: "api", Line: strings.Repeat("x", 100), Kind: "output", Level: "info", Stream: "stdout"}
		data, _ := json.Marshal(event)
		if _, err := writer.Write(append(data, '\n')); err != nil {
			t.Fatal(err)
		}
	}
	writer.Close()
	first, err := Query(path, Options{After: "start", Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Events) != 2 || !first.HasMore {
		t.Fatalf("first page: %+v", first)
	}
	second, err := Query(path, Options{After: first.NextCursor, Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Events) == 0 || second.Events[0].Sequence <= first.Events[1].Sequence {
		t.Fatal("cursor replayed or skipped ordered records")
	}
	gap, err := Query(path, Options{After: "run:1", Limit: 2})
	if err != nil || !gap.Gap {
		t.Fatalf("lost history not disclosed: %+v %v", gap, err)
	}
}

func TestQuery_OpenReadersPermitRotationAndKeepPinnedData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	writer, err := logrotate.New(path, 256, 3)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	first, _ := json.Marshal(ipc.LogEntry{Version: 1, ID: "run:1", RunID: "run", Sequence: 1, Line: "first"})
	if _, err := writer.Write(append(first, '\n')); err != nil {
		t.Fatal(err)
	}
	reader, err := openJournal(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	for i := 2; i <= 40; i++ {
		data, _ := json.Marshal(ipc.LogEntry{Version: 1, ID: fmt.Sprintf("run:%d", i), RunID: "run", Sequence: uint64(i), Line: strings.Repeat("x", 256)})
		if _, err := writer.Write(append(data, '\n')); err != nil {
			t.Fatal(err)
		}
	}
	data, err := io.ReadAll(reader)
	if err != nil || !strings.Contains(string(data), `"id":"run:1"`) {
		t.Fatalf("pinned reader lost original content: %s %v", data, err)
	}
	page, err := Query(path, Options{Tail: true, Limit: 1})
	if err != nil || len(page.Events) != 1 || page.Events[0].ID != "run:40" {
		t.Fatalf("rotation lost latest output: %+v %v", page, err)
	}
}

func TestQuery_ConcurrentRotationNeverSilentlySkipsOrReplays(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	writer, err := logrotate.New(path, 1024, 3)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	var workers sync.WaitGroup
	writeErrors := make(chan error, 1)
	workers.Add(1)
	go func() {
		defer workers.Done()
		for i := 1; i <= 200; i++ {
			data, err := json.Marshal(ipc.LogEntry{Version: 1, ID: fmt.Sprintf("run:%d", i), RunID: "run", Sequence: uint64(i), Process: "api", Line: strings.Repeat("x", 128)})
			if err == nil {
				_, err = writer.Write(append(data, '\n'))
			}
			if err != nil {
				writeErrors <- err
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()
	defer workers.Wait()
	cursor := "start"
	var last uint64
	for i := 0; i < 100; i++ {
		page, err := Query(path, Options{After: cursor, Limit: 5})
		if err != nil {
			if !strings.Contains(err.Error(), "retry this query") {
				t.Fatal(err)
			}
			continue
		}
		for _, event := range page.Events {
			if !page.Gap && last != 0 && event.Sequence != last+1 {
				t.Fatalf("undisclosed discontinuity: %d -> %d", last, event.Sequence)
			}
			last = event.Sequence
		}
		if page.NextCursor != "" {
			cursor = page.NextCursor
		}
		time.Sleep(2 * time.Millisecond)
	}
	workers.Wait()
	select {
	case err := <-writeErrors:
		t.Fatal(err)
	default:
	}
}

// A rotation renames the live journal aside before it installs the
// replacement, so a reader that opens the journal in that window finds the live
// path missing. The newest events are in the staged file then — also after a
// failed rotation that could not put the live journal back — so the reader
// reads it in place of the live journal: the journal is neither missing nor
// a reason to retry, and no event is out of the query's reach.
func TestQuery_MidRotationReadsTheStagedJournal(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	rotated, err := os.Create(path + ".1")
	if err != nil {
		t.Fatal(err)
	}
	writeEvents(t, rotated, 1, 2)
	rotated.Close()
	writer, err := logrotate.New(path, 1<<20, 3)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	writeEvents(t, writer, 3, 3)
	// Reproduce the rotation window: the live journal has been renamed aside
	// while the writer has it open, and no replacement has been installed.
	if err := os.Rename(path, path+logrotate.StagingSuffix); err != nil {
		t.Fatal(err)
	}
	// What the writer writes now lands in the staged file.
	writeEvents(t, writer, 4, 4)
	page, err := Query(path, Options{After: "start"})
	if err != nil {
		t.Fatalf("query while the live journal is staged: %v", err)
	}
	if got := eventIDs(page); got != "run:1,run:2,run:3,run:4" {
		t.Fatalf("page = %s, want the rotated copy followed by the staged journal", got)
	}
	// Once the live journal is back the journal is read normally again.
	if err := os.Rename(path+logrotate.StagingSuffix, path); err != nil {
		t.Fatal(err)
	}
	page, err = Query(path, Options{After: "start"})
	if err != nil {
		t.Fatalf("query after the live journal came back: %v", err)
	}
	if got := eventIDs(page); got != "run:1,run:2,run:3,run:4" {
		t.Fatalf("page = %s, want every recorded event", got)
	}
}

// A live journal that another process removed leaves the rotated copies, and
// a rotation between installing the newest copy and the new live file leaves
// the same state: the copies are the whole journal then. The query reads them
// instead of reporting a rotation in progress until the writer next rotates,
// and a journal with no file left is still reported as missing.
func TestQuery_ReadsTheRotatedCopiesWhenTheLiveJournalIsGone(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	writer, err := logrotate.New(path, 1<<20, 3)
	if err != nil {
		t.Fatal(err)
	}
	writeEvents(t, writer, 1, 2)
	writer.Close()
	if err := os.Rename(path, path+".1"); err != nil {
		t.Fatal(err)
	}
	page, err := Query(path, Options{After: "start"})
	if err != nil {
		t.Fatalf("query without a live journal: %v", err)
	}
	if got := eventIDs(page); got != "run:1,run:2" {
		t.Fatalf("page = %s, want the rotated copy's events", got)
	}

	if err := os.Remove(path + ".1"); err != nil {
		t.Fatal(err)
	}
	if _, err := Query(path, Options{After: "start"}); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("query of a journal with no file left = %v, want a missing journal", err)
	}
}

// writeEvents writes the events from to last of run "run" to writer.
func writeEvents(t *testing.T, writer io.Writer, from, last uint64) {
	t.Helper()
	for i := from; i <= last; i++ {
		data, _ := json.Marshal(ipc.LogEntry{Version: 1, ID: fmt.Sprintf("run:%d", i), RunID: "run", Sequence: i, Line: "ready"})
		if _, err := writer.Write(append(data, '\n')); err != nil {
			t.Fatal(err)
		}
	}
}

// eventIDs lists the page's event ids, comma separated.
func eventIDs(page Page) string {
	ids := make([]string, 0, len(page.Events))
	for _, event := range page.Events {
		ids = append(ids, event.ID)
	}
	return strings.Join(ids, ",")
}

// A reader that opens the journal while the rotation is in flight reads it
// whether it pins the staged file or the file installed at the live path; a
// rename between its opens makes it pin the journal again.
func TestQuery_RecoversWhenTheRotationInstallsItsReplacement(t *testing.T) {
	restore := snapshotRetryDelay
	snapshotRetryDelay = 20 * time.Millisecond
	defer func() { snapshotRetryDelay = restore }()

	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	writer, err := logrotate.New(path, 1<<20, 3)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	data, _ := json.Marshal(ipc.LogEntry{Version: 1, ID: "run:1", RunID: "run", Sequence: 1, Line: "ready"})
	if _, err := writer.Write(append(data, '\n')); err != nil {
		t.Fatal(err)
	}
	staged := path + logrotate.StagingSuffix
	if err := os.Rename(path, staged); err != nil {
		t.Fatal(err)
	}
	// The rotation completes while the reader pins the journal.
	go func() {
		time.Sleep(5 * time.Millisecond)
		_ = os.Rename(staged, path)
	}()
	page, err := Query(path, Options{After: "start"})
	if err != nil {
		t.Fatalf("query during a rotation: %v", err)
	}
	if len(page.Events) != 1 || page.Events[0].ID != "run:1" {
		t.Fatalf("page = %+v, want the single recorded event", page.Events)
	}
}

func TestQuery_CursorCrossesDaemonRuns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, run := range []string{"old", "new"} {
		if err := json.NewEncoder(file).Encode(ipc.LogEntry{Version: 1, ID: run + ":1", RunID: run, Sequence: 1, Process: "api", Line: "ready"}); err != nil {
			t.Fatal(err)
		}
	}
	file.Close()
	page, err := Query(path, Options{After: "old:1"})
	if err != nil || page.Gap || len(page.Events) != 1 || page.Events[0].RunID != "new" {
		t.Fatalf("run transition lost: %+v %v", page, err)
	}
}

func TestQuery_ByteBudgetUnicodeAndFilterWatermark(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	encoder := json.NewEncoder(file)
	large := strings.Repeat("α\"\n", 3000)
	for i, message := range []string{large, "other process", "next failure"} {
		event := ipc.LogEntry{Version: 1, ID: fmt.Sprintf("run:%d", i+1), RunID: "run", Sequence: uint64(i + 1), Time: time.Now(), Process: "api", Line: message, Level: "error", Kind: "output", Stream: "stderr"}
		if i == 1 {
			event.Process = "other"
		}
		if err := encoder.Encode(event); err != nil {
			t.Fatal(err)
		}
	}
	file.Close()
	page, err := Query(path, Options{After: "start", Limit: 1, MaxBytes: 1024, Processes: []string{"api"}, Stream: "stderr"})
	if err != nil {
		t.Fatal(err)
	}
	if !page.Truncated || page.EventBytes > 1024 || !utf8.ValidString(page.Events[0].Line) || page.Events[0].OriginalBytes != len(large) {
		t.Fatalf("byte budget lost: %+v", page)
	}
	next, err := Query(path, Options{After: page.NextCursor, Processes: []string{"api"}})
	if err != nil || len(next.Events) != 1 || next.Events[0].Line != "next failure" {
		t.Fatalf("filtered cursor lost records: %+v %v", next, err)
	}
}

func TestQuery_DoesNotExposeUncommittedPartialRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	data, _ := json.Marshal(ipc.LogEntry{Version: 1, ID: "run:1", Line: "complete"})
	if err := os.WriteFile(path, append(append(data, '\n'), []byte(`{"schema_version":1`)...), 0600); err != nil {
		t.Fatal(err)
	}
	page, err := Query(path, Options{After: "start"})
	if err != nil || len(page.Events) != 1 {
		t.Fatalf("partial record leaked: %+v %v", page, err)
	}
}

func TestQuery_AttemptFilter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	var lines []string
	for i, attempt := range []string{"A", "B", "A", ""} {
		event := ipc.LogEntry{Version: 1, ID: fmt.Sprintf("run:%d", i+1), RunID: "run", AttemptID: attempt, Sequence: uint64(i + 1), Process: "api", Line: "x"}
		data, _ := json.Marshal(event)
		lines = append(lines, string(data))
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	page, err := Query(path, Options{AttemptID: "A"})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Events) != 2 || page.Events[0].ID != "run:1" || page.Events[1].ID != "run:3" {
		t.Fatalf("attempt page = %+v", page.Events)
	}
}

// writeJournal writes count sequential events and returns the journal path.
func writeJournal(t *testing.T, count int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "events.jsonl")
	var lines []string
	for i := 1; i <= count; i++ {
		event := ipc.LogEntry{Version: 1, ID: fmt.Sprintf("run:%d", i), RunID: "run", Sequence: uint64(i), Process: "api", Line: fmt.Sprintf("line %d", i)}
		data, _ := json.Marshal(event)
		lines = append(lines, string(data))
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

// A tail query with a cursor returns the newest events after that cursor,
// not the oldest ones (issue #42).
func TestQuery_TailWithCursorReturnsNewestEvents(t *testing.T) {
	path := writeJournal(t, 10)
	page, err := Query(path, Options{After: "run:1", Tail: true, Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Events) != 2 || page.Events[0].ID != "run:9" || page.Events[1].ID != "run:10" {
		t.Fatalf("tail page = %+v, want run:9 and run:10", page.Events)
	}
}

// A tail query from the start of the journal returns the newest events
// overall (issue #42).
func TestQuery_TailFromStartReturnsNewestEvents(t *testing.T) {
	path := writeJournal(t, 10)
	page, err := Query(path, Options{After: "start", Tail: true, Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Events) != 2 || page.Events[0].ID != "run:9" || page.Events[1].ID != "run:10" {
		t.Fatalf("tail page = %+v, want run:9 and run:10", page.Events)
	}
}

// The tail contract: has_more is always false, next_cursor is the last event
// scanned and truncated reports dropped events (issue #42).
func TestQuery_TailPageMetadata(t *testing.T) {
	path := writeJournal(t, 10)
	page, err := Query(path, Options{After: "run:1", Tail: true, Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if page.HasMore {
		t.Errorf("has_more = true under tail, want false")
	}
	if page.NextCursor != "run:10" {
		t.Errorf("next_cursor = %q, want run:10 (the last event scanned)", page.NextCursor)
	}
	if !page.Truncated {
		t.Errorf("truncated = false under tail, want true (older events were dropped)")
	}
}

// The cursor advances over events that did not match the filters, so it is
// the last event scanned and not the last event returned (issue #42).
func TestQuery_TailCursorAdvancesOverNonMatchingEvents(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	var lines []string
	for i := 1; i <= 10; i++ {
		process := "api"
		if i == 10 {
			process = "db"
		}
		event := ipc.LogEntry{Version: 1, ID: fmt.Sprintf("run:%d", i), RunID: "run", Sequence: uint64(i), Process: process, Line: fmt.Sprintf("line %d", i)}
		data, _ := json.Marshal(event)
		lines = append(lines, string(data))
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	page, err := Query(path, Options{After: "run:1", Tail: true, Limit: 2, Processes: []string{"api"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Events) != 2 || page.Events[0].ID != "run:8" || page.Events[1].ID != "run:9" {
		t.Fatalf("tail page = %+v, want run:8 and run:9", page.Events)
	}
	if page.NextCursor != "run:10" {
		t.Errorf("next_cursor = %q, want run:10 (the last event scanned)", page.NextCursor)
	}
}
