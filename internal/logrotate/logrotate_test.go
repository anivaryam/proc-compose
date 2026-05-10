package logrotate

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriter_RotatesAtMaxSize(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.log")

	w, err := New(path, 100, 3)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Write enough to trigger rotation.
	data := strings.Repeat("x", 60)
	w.Write([]byte(data + "\n")) // 61 bytes
	w.Write([]byte(data + "\n")) // 122 bytes total -> rotate

	// After rotation, .1 should exist.
	if _, err := os.Stat(path + ".1"); err != nil {
		t.Fatalf("expected rotated file .1 to exist: %v", err)
	}

	w.Close()
}

func TestWriter_MaxFilesRespected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.log")

	maxFiles := 2
	w, err := New(path, 50, maxFiles)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Write enough to trigger 4 rotations.
	data := strings.Repeat("a", 60) + "\n"
	for i := 0; i < 5; i++ {
		w.Write([]byte(data))
	}
	w.Close()

	// Only .1 and .2 should exist, not .3 or beyond.
	for i := 1; i <= maxFiles; i++ {
		f := fmt.Sprintf("%s.%d", path, i)
		if _, err := os.Stat(f); err != nil {
			t.Errorf("expected %s to exist: %v", f, err)
		}
	}
	for i := maxFiles + 1; i <= maxFiles+2; i++ {
		f := fmt.Sprintf("%s.%d", path, i)
		if _, err := os.Stat(f); !os.IsNotExist(err) {
			t.Errorf("expected %s to NOT exist", f)
		}
	}
}

func TestWriter_SmallWrites(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.log")

	w, err := New(path, 100, 2)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Many small writes that collectively exceed the limit.
	for i := 0; i < 200; i++ {
		w.Write([]byte("line\n"))
	}
	w.Close()

	// Current file should be small (less than maxSize).
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Size() > 100 {
		t.Errorf("current file too large after rotation: %d bytes", info.Size())
	}

	// Rotated files should exist.
	if _, err := os.Stat(path + ".1"); err != nil {
		t.Errorf("expected .1 to exist: %v", err)
	}
}
