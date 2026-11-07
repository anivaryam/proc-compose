package sharedfile

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
)

// TestReadFile_NeverFailsWhileTheFileIsReplaced pins the contract the session
// metadata and the PID file rely on (#14): readers that read through ReadFile
// while a writer replaces the file through Replace always read one whole
// version, and the writer's replace never fails. On Windows a plain reader
// makes the replace fail with "Access is denied".
func TestReadFile_NeverFailsWhileTheFileIsReplaced(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "doc.json")
	if err := os.WriteFile(path, []byte("version-0"), 0o600); err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	var readers sync.WaitGroup
	var reads, failures atomic.Int64
	var failure atomic.Value
	for i := 0; i < 4; i++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				data, err := ReadFile(path)
				if err == nil && len(data) < len("version-") {
					err = errors.New("partial document " + strconv.Quote(string(data)))
				}
				if err != nil {
					failures.Add(1)
					failure.Store(err.Error())
				}
				reads.Add(1)
			}
		}()
	}
	for i := 1; i <= 200; i++ {
		tmp := filepath.Join(dir, ".doc.json.tmp")
		if err := os.WriteFile(tmp, []byte("version-"+strconv.Itoa(i)), 0o600); err != nil {
			close(stop)
			readers.Wait()
			t.Fatal(err)
		}
		if err := Replace(tmp, path); err != nil {
			close(stop)
			readers.Wait()
			t.Fatalf("replace %d while readers read: %v", i, err)
		}
	}
	close(stop)
	readers.Wait()
	if failures.Load() > 0 {
		t.Fatalf("%d of %d reads failed while the file was replaced, last: %v", failures.Load(), reads.Load(), failure.Load())
	}
	if data, err := ReadFile(path); err != nil || string(data) != "version-200" {
		t.Fatalf("ReadFile after the replaces = %q, %v; want the last version", data, err)
	}
}

// TestOpenAppend_CreatesAndAppends pins the writer's open: a missing file is
// created with the requested mode, and an existing one is appended to.
func TestOpenAppend_CreatesAndAppends(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.log")
	for _, chunk := range []string{"first\n", "second\n"} {
		f, err := OpenAppend(path, 0o600)
		if err != nil {
			t.Fatalf("OpenAppend: %v", err)
		}
		if _, err := f.WriteString(chunk); err != nil {
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "first\nsecond\n" {
		t.Fatalf("file = %q, %v; want both chunks in order", data, err)
	}
	if runtime.GOOS != "windows" {
		if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("mode = %v, %v; want 0600", info.Mode().Perm(), err)
		}
	}
}

// TestOpenAppend_FileCanBeRenamedAndRemovedWhileOpen pins what a rotating
// writer needs: the file it is writing to can be renamed aside, the writes
// follow it, and it can be removed while it is open.
func TestOpenAppend_FileCanBeRenamedAndRemovedWhileOpen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")
	f, err := OpenAppend(path, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString("before\n"); err != nil {
		t.Fatal(err)
	}
	moved := path + ".1"
	if err := os.Rename(path, moved); err != nil {
		t.Fatalf("rename while open: %v", err)
	}
	if _, err := f.WriteString("after\n"); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(moved); err != nil || string(data) != "before\nafter\n" {
		t.Fatalf("renamed file = %q, %v; want the writes before and after the rename", data, err)
	}
	if err := os.Remove(moved); err != nil {
		t.Fatalf("remove while open: %v", err)
	}
}

// TestOpen_ReaderPermitsRenameAndRemove pins the reader's side: a file a
// reader has open can still be renamed and removed, and the reader keeps
// reading the file it opened.
func TestOpen_ReaderPermitsRenameAndRemove(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	if err := os.WriteFile(path, []byte("pinned"), 0o600); err != nil {
		t.Fatal(err)
	}
	reader, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	if err := os.Rename(path, path+".1"); err != nil {
		t.Fatalf("rename while a reader has the file open: %v", err)
	}
	if err := os.Remove(path + ".1"); err != nil {
		t.Fatalf("remove while a reader has the file open: %v", err)
	}
	data := make([]byte, 16)
	n, err := reader.Read(data)
	if err != nil || string(data[:n]) != "pinned" {
		t.Fatalf("read = %q, %v; want the content the reader opened", data[:n], err)
	}
}

// TestOpen_MissingFileIsNotExist pins the error a missing file gives.
func TestOpen_MissingFileIsNotExist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing")
	if _, err := Open(path); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Open = %v, want not exist", err)
	}
	if _, err := ReadFile(path); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("ReadFile = %v, want not exist", err)
	}
}
