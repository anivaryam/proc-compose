package ipc

import (
	"reflect"
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
