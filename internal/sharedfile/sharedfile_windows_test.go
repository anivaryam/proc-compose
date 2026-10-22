//go:build windows

package sharedfile

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// replaceFixture writes the target with "old" and the replacement with "new"
// and returns both paths.
func replaceFixture(t *testing.T) (tmp, target string) {
	t.Helper()
	dir := t.TempDir()
	tmp, target = filepath.Join(dir, ".doc.tmp"), filepath.Join(dir, "doc.json")
	if err := os.WriteFile(target, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tmp, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	return tmp, target
}

// TestReplace_ReaderKeepsTheFileItOpened pins the POSIX rename: the replace
// succeeds while a reader has the target open, and the reader keeps reading
// the version it opened. The plain rename fails with "Access is denied" here.
func TestReplace_ReaderKeepsTheFileItOpened(t *testing.T) {
	tmp, target := replaceFixture(t)
	reader, err := Open(target)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	if err := Replace(tmp, target); err != nil {
		t.Fatalf("Replace while a reader has the target open: %v", err)
	}
	if data, err := io.ReadAll(reader); err != nil || string(data) != "old" {
		t.Fatalf("open reader = %q, %v; want the version it opened", data, err)
	}
	if data, err := ReadFile(target); err != nil || string(data) != "new" {
		t.Fatalf("target = %q, %v; want the replacement", data, err)
	}
}

// TestReplace_WaitsOutAReaderThatBlocksDeletes pins the bounded retry: a
// reader that does not allow delete sharing (os.Open) blocks the rename only
// while it has the file open.
func TestReplace_WaitsOutAReaderThatBlocksDeletes(t *testing.T) {
	tmp, target := replaceFixture(t)
	reader, err := os.Open(target)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(20 * time.Millisecond)
		reader.Close()
	}()
	if err := Replace(tmp, target); err != nil {
		t.Fatalf("Replace once the blocking reader closed: %v", err)
	}
	if data, err := os.ReadFile(target); err != nil || string(data) != "new" {
		t.Fatalf("target = %q, %v; want the replacement", data, err)
	}
}

// TestReplace_ReportsARenameThatStaysBlocked pins the bound: a reader that
// keeps blocking the rename makes Replace fail with the sharing error, and the
// replacement is left in place.
func TestReplace_ReportsARenameThatStaysBlocked(t *testing.T) {
	tmp, target := replaceFixture(t)
	reader, err := os.Open(target)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	err = Replace(tmp, target)
	if !errors.Is(err, windows.ERROR_ACCESS_DENIED) && !errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
		t.Fatalf("Replace while a reader blocks it = %v, want a sharing failure", err)
	}
	if _, err := os.Stat(tmp); err != nil {
		t.Fatalf("the replacement is gone after a failed replace: %v", err)
	}
}

// TestReplace_FallsBackToThePlainRename pins the path of a filesystem without
// POSIX renames: the plain rename still replaces the target.
func TestReplace_FallsBackToThePlainRename(t *testing.T) {
	original := posixRename
	posixRename = func(string, string) error { return windows.ERROR_NOT_SUPPORTED }
	defer func() { posixRename = original }()
	tmp, target := replaceFixture(t)
	if err := Replace(tmp, target); err != nil {
		t.Fatalf("Replace without POSIX renames: %v", err)
	}
	if data, err := os.ReadFile(target); err != nil || string(data) != "new" {
		t.Fatalf("target = %q, %v; want the replacement", data, err)
	}
}
