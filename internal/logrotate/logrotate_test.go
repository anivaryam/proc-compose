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

// TestWriter_RecreatesARemovedLiveFile pins the recovery from a live file
// that another process removed. The rename that stages it fails with ENOENT;
// the old code swallowed that and truncated whatever it found at the path,
// the first fix aborted every later rotation on it, so the writer never wrote
// to a visible file again. The writer installs a new live file instead,
// reports once that the bytes written to the removed file are not in the log,
// and the writes after that land in the new file and succeed.
func TestWriter_RecreatesARemovedLiveFile(t *testing.T) {
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
	if err == nil || !strings.Contains(err.Error(), "removed") {
		t.Fatalf("the write that found the live file removed = %v, want an error that says so", err)
	}
	if n != len(payload) {
		t.Fatalf("write = %d bytes, want %d: the bytes must still be written", n, len(payload))
	}

	// Nothing was rotated: there was nothing left to rotate.
	for _, name := range []string{path + ".1", path + StagingSuffix} {
		if _, err := os.Stat(name); !os.IsNotExist(err) {
			t.Errorf("%s exists after the live file was recreated: %v", name, err)
		}
	}

	// The writer carries on in a new live file.
	n, err = w.Write([]byte("more"))
	if err != nil {
		t.Fatalf("the write after the live file was recreated: %v", err)
	}
	if n != len("more") {
		t.Fatalf("write = %d bytes, want %d", n, len("more"))
	}
	if got := readFile(t, path); got != "more" {
		t.Fatalf("live file = %q, want the write after the recreation", got)
	}
}

// TestWriter_ReopenFailureKeepsWriterUsable pins the last step of the
// rotation: the renames succeeded but the new live file could not be opened.
// The writer must keep a usable handle, must not reset its size counter, and
// must keep reporting the failure — and the bytes must stay in the live file,
// which is back at the live path.
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
	openFile = func(string, os.FileMode) (*os.File, error) {
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

	// The pre-rotation bytes are not lost: the rotation was rolled back, so
	// they are in the live file.
	if got := readFile(t, path); got != payload {
		t.Fatalf("live file holds %d bytes, want the %d pre-rotation bytes", len(got), len(payload))
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

	// The retry must not consume a rotated copy.
	for _, name := range []string{path + ".1", path + ".2"} {
		if _, err := os.Stat(name); !os.IsNotExist(err) {
			t.Fatalf("the retried rotation left %s behind: %v", name, err)
		}
	}
	if got := readFile(t, path); got != payload+"more" {
		t.Fatalf("live file holds %d bytes, want %d", len(got), len(payload)+len("more"))
	}
}

// readFile returns the content of name, failing the test when it cannot be
// read.
func readFile(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(data)
}

// failRename makes the rename seam fail while fail(from, to) says so, and
// restores it when the test ends.
func failRename(t *testing.T, fail func(from, to string) bool) {
	t.Helper()
	original := rename
	rename = func(from, to string) error {
		if fail(from, to) {
			return &os.LinkError{Op: "rename", Old: from, New: to, Err: os.ErrPermission}
		}
		return original(from, to)
	}
	t.Cleanup(func() { rename = original })
}

// rotations writes lines of 60 bytes to w until it has rotated count times,
// with each line naming its sequence number, and returns the next number.
func rotations(t *testing.T, w *Writer, path string, count, next int) int {
	t.Helper()
	for done := 0; done < count; next++ {
		if _, err := fmt.Fprintf(w, "%059d\n", next); err != nil {
			t.Fatalf("write %d: %v", next, err)
		}
		// A successful rotation is the only thing that resets the counter.
		if w.written == 0 {
			done++
		}
	}
	if _, err := os.Stat(path + ".1"); err != nil {
		t.Fatalf("no rotated copy after %d rotations: %v", count, err)
	}
	return next
}

// lines returns the sequence numbers the log family holds, oldest first.
func lines(t *testing.T, path string, maxFiles int) []string {
	t.Helper()
	var out []string
	for i := maxFiles; i >= 0; i-- {
		name := path
		if i > 0 {
			name = fmt.Sprintf("%s.%d", path, i)
		}
		data, err := os.ReadFile(name)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, strings.Fields(string(data))...)
	}
	return out
}

// TestWriter_FailedRotationRecoversOnALaterWrite is the regression for #33: a
// rotation that failed after the live file was staged used to leave the live
// path absent for the rest of the run, so every later rotation failed, the
// rotated copy grew without limit and every query reported a rotation in
// progress. A transient failure (here the reopen, as with EMFILE) must leave
// the live file in place, must not drop a rotated copy on every retry, and a
// later rotation must succeed with every byte still in the log in order.
func TestWriter_FailedRotationRecoversOnALaterWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.log")
	w, err := New(path, 100, 3)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer w.Close()

	// Fill every rotated slot, so a rotation has to drop the oldest copy, and
	// write one line more, so the next write reaches the rotation size.
	next := rotations(t, w, path, 4, 1)
	if _, err := fmt.Fprintf(w, "%059d\n", next); err != nil {
		t.Fatal(err)
	}
	next++
	want := lines(t, path, 3)
	oldest := len(strings.Fields(readFile(t, path+".3")))

	failures := 3
	original := openFile
	openFile = func(name string, perm os.FileMode) (*os.File, error) {
		if failures > 0 {
			failures--
			return nil, os.ErrPermission
		}
		return original(name, perm)
	}
	t.Cleanup(func() { openFile = original })

	for failures > 0 {
		line := fmt.Sprintf("%059d\n", next)
		next++
		if _, err := w.Write([]byte(line)); err == nil {
			t.Fatal("a write whose rotation failed reported success")
		}
		want = append(want, strings.TrimSpace(line))
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("the live path is absent after a failed rotation: %v", err)
		}
		if _, err := os.Stat(path + StagingSuffix); !os.IsNotExist(err) {
			t.Fatalf("a staged file is left after a failed rotation: %v", err)
		}
	}
	// The failed attempts together dropped only the one copy a single
	// rotation drops, and every byte is still in the log in order.
	if got := lines(t, path, 3); strings.Join(got, ",") != strings.Join(want[oldest:], ",") {
		t.Fatalf("log after the failed rotations = %v, want %v: everything but the oldest copy", got, want[oldest:])
	}

	// The next write rotates successfully.
	if _, err := fmt.Fprintf(w, "%059d\n", next); err != nil {
		t.Fatalf("the write after the failure cleared: %v", err)
	}
	if _, err := os.Stat(path + ".1"); err != nil {
		t.Fatalf("the recovered rotation produced no copy: %v", err)
	}
	if info, err := os.Stat(path); err != nil || info.Size() != 0 {
		t.Fatalf("live file after the recovered rotation = %v, %v; want a new empty file", info, err)
	}
	if w.written != 0 {
		t.Fatalf("written = %d after the recovered rotation, want 0", w.written)
	}
	if _, err := w.Write([]byte("after\n")); err != nil {
		t.Fatalf("a write after the recovered rotation: %v", err)
	}
}

// TestWriter_FailedCopyRenameRollsBack pins the step between staging and
// reopening: the staged file could not become the newest copy. The live file
// goes back to the live path with its bytes, and the next rotation succeeds.
func TestWriter_FailedCopyRenameRollsBack(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.log")
	w, err := New(path, 100, 3)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer w.Close()

	fail := true
	failRename(t, func(from, to string) bool { return fail && from == path+StagingSuffix && to == path+".1" })
	payload := strings.Repeat("x", 150)
	if _, err := w.Write([]byte(payload)); err == nil {
		t.Fatal("the write whose rotation failed reported success")
	}
	if got := readFile(t, path); got != payload {
		t.Fatalf("live file holds %d bytes after the rollback, want %d", len(got), len(payload))
	}
	if _, err := os.Stat(path + StagingSuffix); !os.IsNotExist(err) {
		t.Fatalf("a staged file is left after the rollback: %v", err)
	}

	fail = false
	if _, err := w.Write([]byte("more")); err != nil {
		t.Fatalf("the retried rotation: %v", err)
	}
	if got := readFile(t, path+".1"); got != payload+"more" {
		t.Fatalf("the rotated copy holds %d bytes, want %d", len(got), len(payload)+len("more"))
	}
}

// TestWriter_FailedRollbackIsCompletedByTheNextRotation pins the double
// failure: the rotation failed and so did the rename that rolls it back. The
// file stays staged, where a reader still finds it, and the next rotation
// first puts it back at the live path and then rotates it.
func TestWriter_FailedRollbackIsCompletedByTheNextRotation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.log")
	w, err := New(path, 100, 3)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer w.Close()

	staging := path + StagingSuffix
	fail := true
	failRename(t, func(from, to string) bool { return fail && from == staging })
	payload := strings.Repeat("x", 150)
	if _, err := w.Write([]byte(payload)); err == nil {
		t.Fatal("the write whose rotation failed reported success")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("live path after a failed rollback: %v, want it absent", err)
	}
	if got := readFile(t, staging); got != payload {
		t.Fatalf("staged file holds %d bytes, want %d", len(got), len(payload))
	}
	if w.displaced != staging {
		t.Fatalf("displaced = %q, want %q", w.displaced, staging)
	}

	fail = false
	if _, err := w.Write([]byte("more")); err != nil {
		t.Fatalf("the rotation after the failed rollback: %v", err)
	}
	if got := readFile(t, path+".1"); got != payload+"more" {
		t.Fatalf("the rotated copy holds %d bytes, want %d", len(got), len(payload)+len("more"))
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the live file is missing after the recovered rotation: %v", err)
	}
	if w.displaced != "" {
		t.Fatalf("displaced = %q after the recovered rotation, want empty", w.displaced)
	}
}

// TestWriter_ShiftFailureNeverOverwritesACopy pins that a rotated copy that
// cannot be moved stops the rotation: the old code warned and went on, so the
// staged file was renamed over the copy that was still there.
func TestWriter_ShiftFailureNeverOverwritesACopy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.log")
	w, err := New(path, 100, 3)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer w.Close()
	next := rotations(t, w, path, 1, 1)
	// One line more, so the next write reaches the rotation size.
	if _, err := fmt.Fprintf(w, "%059d\n", next); err != nil {
		t.Fatal(err)
	}
	next++
	copyBefore := readFile(t, path+".1")

	failRename(t, func(from, to string) bool { return from == path+".1" && to == path+".2" })
	for i := 0; i < 3; i++ {
		if _, err := fmt.Fprintf(w, "%059d\n", next); err == nil {
			t.Fatal("a write whose rotation failed reported success")
		}
		next++
	}
	if got := readFile(t, path+".1"); got != copyBefore {
		t.Fatalf("the rotated copy changed: %q, want %q", got, copyBefore)
	}
	if got := lines(t, path, 3); len(got) != next-1 {
		t.Fatalf("log holds %d lines, want all %d written: %v", len(got), next-1, got)
	}
}

// TestNew_RestoresAStagedLiveFile pins the recovery from a rotation that was
// interrupted, by a crash for example, after the live file was staged: New
// puts it back, so its bytes stay in the log instead of being overwritten by
// the next rotation's staging rename. A staged file that cannot be put back
// fails New instead of being lost silently.
func TestNew_RestoresAStagedLiveFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.log")
	if err := os.WriteFile(path+StagingSuffix, []byte("staged\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	w, err := New(path, 100, 3)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := w.Write([]byte("live\n")); err != nil {
		t.Fatal(err)
	}
	w.Close()
	if got := readFile(t, path); got != "staged\nlive\n" {
		t.Fatalf("live file = %q, want the staged bytes followed by the new ones", got)
	}
	if _, err := os.Stat(path + StagingSuffix); !os.IsNotExist(err) {
		t.Fatalf("the staged file is still there: %v", err)
	}

	// A staged file next to a live file is not touched.
	other := filepath.Join(t.TempDir(), "other.log")
	for _, name := range []string{other, other + StagingSuffix} {
		if err := os.WriteFile(name, []byte(filepath.Base(name)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	w, err = New(other, 100, 3)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	w.Close()
	if got := readFile(t, other+StagingSuffix); got != filepath.Base(other+StagingSuffix) {
		t.Fatalf("staged file next to a live file = %q, want it untouched", got)
	}

	// A staged file that cannot be put back fails New.
	blocked := filepath.Join(t.TempDir(), "blocked.log")
	if err := os.WriteFile(blocked+StagingSuffix, []byte("staged\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	failRename(t, func(from, _ string) bool { return from == blocked+StagingSuffix })
	if w, err := New(blocked, 100, 3); err == nil {
		w.Close()
		t.Fatal("New succeeded although the staged file could not be put back")
	}
}
