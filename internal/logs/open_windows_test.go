//go:build windows

package logs

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unsafe"

	"github.com/anivaryam/proc-compose/internal/ipc"

	"golang.org/x/sys/windows"
)

// holdDeletePending puts name into the state rotation leaves it in while its
// delete is in progress: a handle marks it for deletion with the legacy
// disposition and stays open, so the name remains and every open of it fails
// with STATUS_DELETE_PENDING until release is called.
func holdDeletePending(t *testing.T, name string) (release func()) {
	t.Helper()
	p, err := windows.UTF16PtrFromString(name)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := windows.CreateFile(p, windows.DELETE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		t.Fatalf("open %s for delete: %v", name, err)
	}
	deleteFile := byte(1) // FILE_DISPOSITION_INFO.DeleteFile
	if err := windows.SetFileInformationByHandle(handle, windows.FileDispositionInfo, &deleteFile, uint32(unsafe.Sizeof(deleteFile))); err != nil {
		windows.CloseHandle(handle)
		t.Fatalf("mark %s for deletion: %v", name, err)
	}
	released := false
	release = func() {
		if !released {
			released = true
			windows.CloseHandle(handle)
		}
	}
	t.Cleanup(release)
	return release
}

func writeJournalEntry(t *testing.T, name string) {
	t.Helper()
	entry, _ := json.Marshal(ipc.LogEntry{Version: 1, ID: "run:1", RunID: "run", Sequence: 1, Line: "kept"})
	if err := os.WriteFile(name, append(entry, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestOpenJournal_DeletePendingCountsAsNotExist(t *testing.T) {
	name := filepath.Join(t.TempDir(), "events.jsonl.3")
	writeJournalEntry(t, name)
	release := holdDeletePending(t, name)
	_, err := openJournal(name)
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("openJournal() on a name being deleted = %v, want it to count as not existing", err)
	}
	if !strings.Contains(err.Error(), "being deleted") {
		t.Fatalf("error = %q, want it to say the file is being deleted", err)
	}
	release()
	if _, err := os.Stat(name); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Stat() after the delete completed = %v, want not existing", err)
	}
}

// TestOpenJournal_GenuineAccessDeniedIsNotMasked pins the other half: an access
// failure that is not a pending delete keeps its error.
func TestOpenJournal_GenuineAccessDeniedIsNotMasked(t *testing.T) {
	name := filepath.Join(t.TempDir(), "events.jsonl.3")
	// A directory cannot be opened as a file; Windows reports access denied.
	if err := os.Mkdir(name, 0o700); err != nil {
		t.Fatal(err)
	}
	_, err := openJournal(name)
	if !errors.Is(err, windows.ERROR_ACCESS_DENIED) || errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("openJournal() on a directory = %v, want access denied that does not count as not existing", err)
	}
}

// TestQuery_DeletePendingBackupIsRetryable is the query-level regression for the
// rotation race: a backup whose delete is in progress must give the documented
// retryable error, never a hard access-denied, and the query must succeed once
// the delete completes.
func TestQuery_DeletePendingBackupIsRetryable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	writeJournalEntry(t, path)
	writeJournalEntry(t, path+".3")
	release := holdDeletePending(t, path+".3")
	if _, err := Query(path, Options{After: "start", Limit: 5}); err == nil || !strings.Contains(err.Error(), "retry this query") {
		t.Fatalf("Query() with a backup being deleted = %v, want the retryable error", err)
	}
	release()
	page, err := Query(path, Options{After: "start", Limit: 5})
	if err != nil || len(page.Events) != 1 || page.Events[0].ID != "run:1" {
		t.Fatalf("Query() after the delete completed = %+v, %v; want the live entry", page, err)
	}
}

func TestQuery_PermissionFailureIsNotRetryable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	writeJournalEntry(t, path)
	if err := os.Mkdir(path+".3", 0o700); err != nil {
		t.Fatal(err)
	}
	_, err := Query(path, Options{After: "start", Limit: 5})
	if !errors.Is(err, windows.ERROR_ACCESS_DENIED) || strings.Contains(err.Error(), "retry this query") {
		t.Fatalf("Query() with an unreadable backup = %v, want a hard access-denied error", err)
	}
}
