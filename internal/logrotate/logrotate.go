// Package logrotate provides log file rotation when size thresholds are exceeded.
package logrotate

import (
	"fmt"
	"os"
	"sync"
)

// stagingSuffix names the temporary name the live log is renamed to before
// the rotated copies are shifted.
//
// Staging the live file first is what makes a rotation transactional: until
// that rename has succeeded, the live file, the open handle and the rotated
// copy set are all untouched, so a failure costs nothing.
const stagingSuffix = ".staging"

// openFile opens the live log file after a rotation. It is a package-level
// variable so a test can force the reopen to fail once the rename has
// succeeded; the rotation itself never goes through it.
var openFile = os.OpenFile

// Writer wraps an os.File and rotates it when it exceeds maxSize bytes.
// It keeps up to maxFiles rotated copies (file.1, file.2, etc.).
type Writer struct {
	path     string
	maxSize  int64
	maxFiles int

	mu      sync.Mutex
	file    *os.File
	written int64
}

// New creates a rotating Writer that writes to path and rotates when the
// file exceeds maxSize bytes. Up to maxFiles rotated copies are kept.
func New(path string, maxSize int64, maxFiles int) (*Writer, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return nil, err
	}
	// Account for existing file size.
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	return &Writer{
		path:     path,
		maxSize:  maxSize,
		maxFiles: maxFiles,
		file:     f,
		written:  info.Size(),
	}, nil
}

// Write writes p to the live log, rotating it when it has grown past the
// configured size.
//
// The bytes of a write that triggers a rotation are written either way. If
// the rotation then fails, the write reports that failure as well: the
// journal's owner has to learn that the record is degraded. A later write
// retries the rotation against the retained handle and reports the failure
// again until the rotation succeeds.
func (w *Writer) Write(p []byte) (n int, err error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	n, err = w.file.Write(p)
	w.written += int64(n)

	if w.written >= w.maxSize {
		// A failed rotation leaves the handle and the bytes in place, so the
		// next write retries it.
		if rerr := w.rotate(); rerr != nil && err == nil {
			err = rerr
		}
	}
	return n, err
}

// Close closes the underlying file.
func (w *Writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.file.Close()
}

// rotate rotates the current file: the live file is staged under a temporary
// name, the rotated copies are shifted, the staged file becomes the newest
// copy and a fresh empty live file takes its place.
//
// It is transactional. A failure at any step leaves the open handle, the
// bytes already written and the rotated copy set as they were, and returns
// the error. It never assigns a nil handle and never resets the size counter
// unless a rotation actually happened.
//
// Must be called with w.mu held.
func (w *Writer) rotate() error {
	// Stage the live file first. Nothing else has happened yet, so a failure
	// here — including ENOENT, which used to be swallowed — costs nothing:
	// the live file, the open handle and the copy set are as they were.
	staging := w.path + stagingSuffix
	if err := os.Rename(w.path, staging); err != nil {
		fmt.Fprintf(os.Stderr, "WARN: failed to stage current log %s: %v\n", w.path, err)
		return fmt.Errorf("rotate current log %s: %w", w.path, err)
	}

	// The copies shift only once the live file is safely staged, so a failed
	// rotation cannot consume a rotated copy.
	for i := w.maxFiles; i >= 1; i-- {
		old := fmt.Sprintf("%s.%d", w.path, i)
		if i == w.maxFiles {
			if err := os.Remove(old); err != nil && !os.IsNotExist(err) {
				fmt.Fprintf(os.Stderr, "WARN: failed to remove old log %s: %v\n", old, err)
			}
		} else {
			next := fmt.Sprintf("%s.%d", w.path, i+1)
			if err := os.Rename(old, next); err != nil && !os.IsNotExist(err) {
				fmt.Fprintf(os.Stderr, "WARN: failed to rotate log %s -> %s: %v\n", old, next, err)
			}
		}
	}

	// The staged file becomes the newest rotated copy.
	rotated := fmt.Sprintf("%s.%d", w.path, 1)
	if err := os.Rename(staging, rotated); err != nil {
		fmt.Fprintf(os.Stderr, "WARN: failed to rotate current log to %s: %v\n", rotated, err)
		return fmt.Errorf("rotate current log to %s: %w", rotated, err)
	}

	// The new live file is installed only now. The live path is empty
	// because the old content was renamed away, so O_TRUNC cannot destroy
	// anything that is still being written.
	f, err := openFile(w.path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		fmt.Fprintf(os.Stderr, "WARN: failed to reopen log file %s: %v\n", w.path, err)
		return fmt.Errorf("reopen log file %s: %w", w.path, err)
	}

	// The old handle is closed last: until the replacement exists the
	// writer keeps a usable handle.
	old := w.file
	w.file = f
	w.written = 0
	if old != nil {
		old.Close()
	}
	return nil
}
