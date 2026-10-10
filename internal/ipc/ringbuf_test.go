package ipc

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestLogRing_PushUnderCapacity(t *testing.T) {
	r := newLogRing(4)
	r.push(LogEntry{Line: "a"})
	r.push(LogEntry{Line: "b"})

	got := r.snapshot()
	want := []LogEntry{{Line: "a"}, {Line: "b"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestLogRing_BoundsEncodedHistory(t *testing.T) {
	r := newLogRing(500)
	for i := 0; i < 100; i++ {
		r.push(LogEntry{Line: strings.Repeat("x", 10<<10)})
	}
	data, err := json.Marshal(r.snapshot())
	if err != nil {
		t.Fatal(err)
	}
	if len(data) > 270<<10 {
		t.Fatalf("history exceeded byte budget: %d", len(data))
	}
	var legacy LogEntry
	if err := json.Unmarshal([]byte(`{"process":"old","line":"legacy output"}`), &legacy); err != nil || legacy.Line != "legacy output" {
		t.Fatalf("legacy event not decoded: %+v %v", legacy, err)
	}
}

func TestEvent_LegacyMonitorAndNewJournalCompatibility(t *testing.T) {
	entry := LogEntry{Version: 1, ID: "run:1", Line: "hello", Process: "worker"}
	for _, event := range []Event{{Type: TypeLog, Log: &entry}, {Type: TypeSnapshot, RecentLogs: []LogEntry{entry}}} {
		data, err := json.Marshal(event)
		if err != nil {
			t.Fatal(err)
		}
		var old struct {
			Log *struct {
				Line string `json:"line"`
			} `json:"log"`
			RecentLogs []struct {
				Line string `json:"line"`
			} `json:"recent_logs"`
		}
		if err := json.Unmarshal(data, &old); err != nil {
			t.Fatal(err)
		}
		if event.Type == TypeLog && (old.Log == nil || old.Log.Line != "hello") {
			t.Fatalf("older monitor lost output: %s", data)
		}
		if event.Type == TypeSnapshot && (len(old.RecentLogs) != 1 || old.RecentLogs[0].Line != "hello") {
			t.Fatalf("older monitor lost history: %s", data)
		}
		var current Event
		if err := json.Unmarshal(data, &current); err != nil {
			t.Fatal(err)
		}
		if current.Log != nil && current.Log.Line != "hello" {
			t.Fatal("current decoder lost message")
		}
	}
	data, err := json.Marshal(entry)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), `"line":`) {
		t.Fatal("IPC compatibility duplicated journal text")
	}
}

func TestLogRing_PushOverCapacityKeepsLatest(t *testing.T) {
	r := newLogRing(3)
	for _, l := range []string{"a", "b", "c", "d", "e"} {
		r.push(LogEntry{Line: l})
	}

	got := r.snapshot()
	want := []LogEntry{{Line: "c"}, {Line: "d"}, {Line: "e"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestLogRing_SnapshotEmpty(t *testing.T) {
	r := newLogRing(3)
	if got := r.snapshot(); len(got) != 0 {
		t.Errorf("expected empty snapshot, got %v", got)
	}
}

func TestLogRing_PushExactlyCapacity(t *testing.T) {
	r := newLogRing(3)
	for _, l := range []string{"a", "b", "c"} {
		r.push(LogEntry{Line: l})
	}
	got := r.snapshot()
	want := []LogEntry{{Line: "a"}, {Line: "b"}, {Line: "c"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}
