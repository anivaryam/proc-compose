//go:build windows

package logs

import (
	"errors"
	"io/fs"
	"os"
	"runtime"

	"golang.org/x/sys/windows"
)

var procRtlGetLastNtStatus = windows.NewLazySystemDLL("ntdll.dll").NewProc("RtlGetLastNtStatus")

// Readers must allow rename/delete while their handles pin the old file.
// os.Open omits FILE_SHARE_DELETE and otherwise prevents live log rotation.
func openJournal(path string) (*os.File, error) {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	handle, err := createJournalHandle(name)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	return os.NewFile(uintptr(handle), path), nil
}

// createJournalHandle opens a journal file for reading.
//
// Rotation deletes the oldest backup, and while that delete is in progress an
// open of the same name fails with STATUS_DELETE_PENDING. Win32 reports that as
// ERROR_ACCESS_DENIED, the same code as a genuine permission failure, so on that
// code the NTSTATUS behind it is read to tell the two apart. The status is kept
// per thread, so the thread is locked across both calls, and ntdll is resolved
// first so that loading it cannot overwrite the status.
func createJournalHandle(name *uint16) (windows.Handle, error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	canQueryStatus := procRtlGetLastNtStatus.Find() == nil
	handle, err := windows.CreateFile(name, windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if canQueryStatus && errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		if status, _, _ := procRtlGetLastNtStatus.Call(); windows.NTStatus(status) == windows.STATUS_DELETE_PENDING {
			return handle, errDeletePending
		}
	}
	return handle, err
}

// errDeletePending reports a journal file whose deletion is in progress. It
// counts as not existing: the name is being removed, and the query's identity
// check decides whether the set of files it pinned is still coherent.
var errDeletePending error = deletePendingError{}

type deletePendingError struct{}

func (deletePendingError) Error() string { return "file is being deleted" }

func (deletePendingError) Is(target error) bool { return target == fs.ErrNotExist }
