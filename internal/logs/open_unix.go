//go:build !windows

package logs

import "os"

func openJournal(path string) (*os.File, error) { return os.Open(path) }
