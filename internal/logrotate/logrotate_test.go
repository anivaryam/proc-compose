package logrotate

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
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

// TestWriter_RenameFailureKeepsLogContent pins the rotation failure that
// used to destroy the log it was supposed to preserve.
//
// The rename needs write permission on the directory while the write needs
// only write permission on the file, so the write succeeds and only the
// rotation fails. The bytes must survive, no rotated copy may appear, and
// the write must report the failure instead of claiming success.
func TestWriter_RenameFailureKeepsLogContent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("directory permissions do not stop a rename on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions, so the rename cannot be made to fail this way")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "test.log")
	w, err := New(path, 100, 3)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer w.Close()

	if err := os.Chmod(dir, 0500); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0700) })

	payload := strings.Repeat("x", 150)
	n, err := w.Write([]byte(payload))
	if err == nil {
		t.Fatal("the write that triggered the failed rotation reported success")
	}
	if n != len(payload) {
		t.Fatalf("write = %d bytes, want %d: the bytes must still be written", n, len(payload))
	}

	// Every byte written before the failed rotation is still in the live log.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Size() != int64(len(payload)) {
		t.Fatalf("live log = %d bytes, want %d: a failed rotation must not truncate the log", info.Size(), len(payload))
	}

	// No rotated copy was created.
	if _, err := os.Stat(path + ".1"); !os.IsNotExist(err) {
		t.Fatalf("a rotated copy exists after the failed rotation: %v", err)
	}

	// A later write retries the rotation and reports it again.
	more := "more"
	n, err = w.Write([]byte(more))
	if err == nil {
		t.Fatal("the write after the failed rotation reported success")
	}
	if n != len(more) {
		t.Fatalf("write = %d bytes, want %d", n, len(more))
	}
}

// TestWriter_MissingLiveFileAbortsRotation pins the other live-rename
// failure: the file is gone. The old code swallowed ENOENT and went on to
// truncate whatever it found at the path, so the rotation has to abort on
// any failure, ENOENT included.
func TestWriter_MissingLiveFileAbortsRotation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.log")
	w, err := New(path, 100, 3)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer w.Close()

	// The live file disappears underneath the writer.
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove: %v", err)
	}

	payload := strings.Repeat("x", 150)
	n, err := w.Write([]byte(payload))
	if err == nil {
		t.Fatal("the write that triggered the failed rotation reported success")
	}
	if n != len(payload) {
		t.Fatalf("write = %d bytes, want %d: the bytes must still be written", n, len(payload))
	}

	// The rotation aborted: no copy and no staging file were produced.
	for _, name := range []string{path + ".1", path + ".staging"} {
		if _, err := os.Stat(name); !os.IsNotExist(err) {
			t.Errorf("%s exists after the failed rotation: %v", name, err)
		}
	}

	// The writer is still usable and still reports the failure.
	n, err = w.Write([]byte("more"))
	if err == nil {
		t.Fatal("the write after the failed rotation reported success")
	}
	if n != len("more") {
		t.Fatalf("write = %d bytes, want %d", n, len("more"))
	}
}

// TestWriter_ReopenFailureKeepsWriterUsable pins the second half of the
// rotation: the rename succeeded but the new live file could not be opened.
// The writer must keep a usable handle, must not reset its size counter, and
// must keep reporting the failure — and the bytes must not be lost.
//
// The reopen is forced to fail through the openFile seam, so the branch is
// reached deterministically instead of depending on a filesystem quirk.
func TestWriter_ReopenFailureKeepsWriterUsable(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.log")
	w, err := New(path, 100, 3)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer w.Close()

	original := openFile
	openFile = func(string, int, os.FileMode) (*os.File, error) {
		return nil, os.ErrPermission
	}
	t.Cleanup(func() { openFile = original })

	payload := strings.Repeat("x", 150)
	n, err := w.Write([]byte(payload))
	if err == nil {
		t.Fatal("the write that triggered the failed rotation reported success")
	}
	if n != len(payload) {
		t.Fatalf("write = %d bytes, want %d: the bytes must still be written", n, len(payload))
	}
	if w.file == nil {
		t.Fatal("the writer holds a nil file after the failed rotation")
	}
	if w.written != int64(len(payload)) {
		t.Fatalf("written = %d, want %d: a failed rotation must not reset the size counter", w.written, len(payload))
	}

	// The pre-rotation bytes are not lost: they are in the rotated copy.
	data, err := os.ReadFile(path + ".1")
	if err != nil {
		t.Fatalf("the rotated copy is missing: %v", err)
	}
	if !strings.Contains(string(data), payload) {
		t.Fatalf("the rotated copy does not hold the pre-rotation bytes: %d bytes", len(data))
	}

	// A later write retries the rotation and reports it again.
	n, err = w.Write([]byte("more"))
	if err == nil {
		t.Fatal("the write after the failed rotation reported success")
	}
	if n != len("more") {
		t.Fatalf("write = %d bytes, want %d", n, len("more"))
	}
	if w.file == nil {
		t.Fatal("the writer holds a nil file after the failed rotation")
	}

	// The retry must not consume another rotated copy.
	if _, err := os.Stat(path + ".2"); !os.IsNotExist(err) {
		t.Fatalf("the retried rotation consumed a rotated copy: %v", err)
	}
}
