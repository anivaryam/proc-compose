package logs

import (
	"encoding/json"
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
