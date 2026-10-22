//go:build !windows

package sharedfile

import "os"

// Open opens path for reading.
func Open(path string) (*os.File, error) { return os.Open(path) }

// ReadFile reads the whole file at path.
func ReadFile(path string) ([]byte, error) { return os.ReadFile(path) }

// OpenAppend opens path for appending, creating it with perm when it does not
// exist.
func OpenAppend(path string, perm os.FileMode) (*os.File, error) {
	return os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, perm)
}

// Replace renames from to to, replacing to when it exists.
func Replace(from, to string) error { return os.Rename(from, to) }
