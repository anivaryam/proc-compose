//go:build windows

package sharedfile

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// shareAll lets other handles read, write, rename and delete the file while
// this handle is open. os.Open and os.OpenFile omit FILE_SHARE_DELETE, so a
// handle they open makes a rotation's rename or delete fail.
const shareAll = windows.FILE_SHARE_READ | windows.FILE_SHARE_WRITE | windows.FILE_SHARE_DELETE

// appendAccess is what os.OpenFile requests for O_WRONLY|O_APPEND: every right
// GENERIC_WRITE grants except FILE_WRITE_DATA, so every write lands at the end
// of the file.
const appendAccess = windows.FILE_APPEND_DATA | windows.FILE_WRITE_ATTRIBUTES |
	windows.FILE_WRITE_EA | windows.STANDARD_RIGHTS_WRITE | windows.SYNCHRONIZE

var procRtlGetLastNtStatus = windows.NewLazySystemDLL("ntdll.dll").NewProc("RtlGetLastNtStatus")

// Open opens path for reading. The handle lets other processes rename and
// delete the file while it is open.
func Open(path string) (*os.File, error) {
	return open(path, windows.GENERIC_READ, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL)
}

// ReadFile reads the whole file at path through a handle that lets other
// processes rename, delete or replace the file while it is read.
func ReadFile(path string) ([]byte, error) {
	file, err := Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return io.ReadAll(file)
}

// OpenAppend opens path for appending, creating it with perm when it does not
// exist. The handle lets the file be renamed and deleted while it is open, so
// a writer can rotate the file it is still writing to.
func OpenAppend(path string, perm os.FileMode) (*os.File, error) {
	attrs := uint32(windows.FILE_ATTRIBUTE_NORMAL)
	if perm&0o200 == 0 {
		attrs = windows.FILE_ATTRIBUTE_READONLY
	}
	return open(path, appendAccess, windows.OPEN_ALWAYS, attrs)
}

func open(path string, access, disposition, attrs uint32) (*os.File, error) {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	handle, err := createFile(name, access, disposition, attrs)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	return os.NewFile(uintptr(handle), path), nil
}

// createFile opens a file with every sharing mode.
//
// Rotation deletes the oldest backup, and while that delete is in progress an
// open of the same name fails with STATUS_DELETE_PENDING. Win32 reports that as
// ERROR_ACCESS_DENIED, the same code as a genuine permission failure, so on that
// code the NTSTATUS behind it is read to tell the two apart. The status is kept
// per thread, so the thread is locked across both calls, and ntdll is resolved
// first so that loading it cannot overwrite the status.
func createFile(name *uint16, access, disposition, attrs uint32) (windows.Handle, error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	canQueryStatus := procRtlGetLastNtStatus.Find() == nil
	handle, err := windows.CreateFile(name, access, shareAll, nil, disposition, attrs, 0)
	if canQueryStatus && errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		if status, _, _ := procRtlGetLastNtStatus.Call(); windows.NTStatus(status) == windows.STATUS_DELETE_PENDING {
			return handle, errDeletePending
		}
	}
	return handle, err
}

// errDeletePending reports a file whose deletion is in progress. It counts as
// not existing: the name is being removed.
var errDeletePending error = deletePendingError{}

type deletePendingError struct{}

func (deletePendingError) Error() string { return "file is being deleted" }

func (deletePendingError) Is(target error) bool { return target == fs.ErrNotExist }

// replaceAttempts bounds how often Replace tries a rename that another handle
// blocks. The waits between the attempts double from one millisecond, so the
// last attempt starts about 127ms after the first.
const replaceAttempts = 8

// Replace renames from to to, replacing to when it exists.
//
// The rename uses POSIX semantics, so it succeeds while readers that opened
// to through Open or ReadFile still have it open, and those readers keep
// reading the file they opened. A filesystem without POSIX renames (FAT, some
// network shares, Windows before 10 1709) gets the plain rename instead, which
// fails while to is open.
//
// A handle that does not allow delete sharing — a reader outside this package,
// a virus scanner, an editor — still blocks the rename. Such a reader holds the
// file for the length of one read, so a blocked rename is tried again for a
// bounded time before its error is returned.
func Replace(from, to string) error {
	delay := time.Millisecond
	for attempt := 1; ; attempt++ {
		err := replace(from, to)
		if err == nil || attempt == replaceAttempts || !blocked(err) {
			return err
		}
		time.Sleep(delay)
		delay *= 2
	}
}

// blocked reports a rename that another handle on from or to prevented.
// ERROR_ACCESS_DENIED is what the plain rename reports for a target that is
// open, so it is retried as well: a genuine permission failure only costs the
// bounded wait before it is returned.
func blocked(err error) bool {
	return errors.Is(err, windows.ERROR_SHARING_VIOLATION) || errors.Is(err, windows.ERROR_ACCESS_DENIED)
}

// posixRename is a variable so a test can take the path of a filesystem that
// has no POSIX renames.
var posixRename = renamePOSIX

func replace(from, to string) error {
	if err := posixRename(from, to); err == nil {
		return nil
	}
	// The filesystem may not support POSIX renames; the plain rename reports
	// its own error, which is the one that matters when it fails too.
	return os.Rename(from, to)
}

// renamePOSIX renames from over to with FILE_RENAME_POSIX_SEMANTICS.
func renamePOSIX(from, to string) error {
	target, err := filepath.Abs(to)
	if err != nil {
		return err
	}
	source, err := windows.UTF16PtrFromString(from)
	if err != nil {
		return err
	}
	name, err := windows.UTF16FromString(target)
	if err != nil {
		return err
	}
	handle, err := windows.CreateFile(source, windows.DELETE|windows.SYNCHRONIZE, shareAll, nil,
		windows.OPEN_EXISTING, windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(handle)

	// FILE_RENAME_INFO, whose file name runs past the end of the struct. The
	// buffer is made of uint64s so the struct is aligned.
	type renameInfo struct {
		Flags          uint32
		RootDirectory  windows.Handle
		FileNameLength uint32
		FileName       [1]uint16
	}
	size := unsafe.Offsetof(renameInfo{}.FileName) + uintptr(len(name))*2
	buffer := make([]uint64, (size+7)/8)
	info := (*renameInfo)(unsafe.Pointer(&buffer[0]))
	info.Flags = windows.FILE_RENAME_REPLACE_IF_EXISTS | windows.FILE_RENAME_POSIX_SEMANTICS
	info.FileNameLength = uint32(len(name)-1) * 2 // in bytes, without the terminator
	copy(unsafe.Slice(&info.FileName[0], len(name)), name)
	return windows.SetFileInformationByHandle(handle, windows.FileRenameInfoEx,
		(*byte)(unsafe.Pointer(&buffer[0])), uint32(size))
}
