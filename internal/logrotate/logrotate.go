// Package logrotate provides log file rotation when size thresholds are exceeded.
package logrotate

import (
	"fmt"
	"os"
	"sync"
)

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

func (w *Writer) Write(p []byte) (n int, err error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	n, err = w.file.Write(p)
	w.written += int64(n)

	if w.written >= w.maxSize {
		w.rotate()
	}
	return n, err
}

// Close closes the underlying file.
func (w *Writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.file.Close()
}

// rotate renames the current file and opens a new one.
// Must be called with w.mu held.
func (w *Writer) rotate() {
	w.file.Close()

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
	if err := os.Rename(w.path, fmt.Sprintf("%s.%d", w.path, 1)); err != nil && !os.IsNotExist(err) {
		fmt.Fprintf(os.Stderr, "WARN: failed to rotate current log to %s.1: %v\n", w.path, err)
	}

	f, err := os.OpenFile(w.path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		f, err = os.OpenFile(w.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
		if err != nil {
			fmt.Fprintf(os.Stderr, "WARN: failed to reopen log file %s: %v\n", w.path, err)
		}
	}
	w.file = f
	w.written = 0
}
