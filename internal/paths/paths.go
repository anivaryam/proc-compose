// Package paths manages derived paths for daemon files (sockets, PID files, logs).
package paths

import (
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"runtime"
)

// FromConfig returns the 8-char hash derived from the absolute config path.
// All file paths for a daemon instance are derived from this hash.
//
// FNV-64 is used over MD5: identifier is non-security (just a per-config
// directory namespace), and FNV avoids tripping security scanners that flag
// md5 sums regardless of context.
func FromConfig(configFile string) (string, error) {
	abs, err := filepath.Abs(configFile)
	if err != nil {
		return "", fmt.Errorf("cannot resolve config path: %w", err)
	}
	h := fnv.New64a()
	h.Write([]byte(abs))
	return fmt.Sprintf("%016x", h.Sum64())[:8], nil
}

// runtimeDir returns a per-user directory for daemon files. Preference:
//   1. $XDG_RUNTIME_DIR (Linux: /run/user/$UID, already mode 0700)
//   2. os.UserCacheDir() + "/proc-compose" (mkdir 0700)
//   3. os.TempDir() as last resort (shared; no privacy guarantees)
//
// Failures fall back silently to os.TempDir so the caller's open/listen call
// surfaces the eventual error with a meaningful path.
func runtimeDir() string {
	if d := os.Getenv("XDG_RUNTIME_DIR"); d != "" {
		return d
	}
	if cache, err := os.UserCacheDir(); err == nil {
		dir := filepath.Join(cache, "proc-compose")
		if err := os.MkdirAll(dir, 0700); err == nil {
			return dir
		}
	}
	return os.TempDir()
}

// Socket returns the socket path for the daemon.
// On Windows, named pipes are used (\\.\pipe\pc-<hash>).
// On Unix, a Unix socket is placed in the per-user runtime dir.
func Socket(hash string) string {
	if runtime.GOOS == "windows" {
		return `\\.\pipe\pc-` + hash
	}
	return filepath.Join(runtimeDir(), "pc-"+hash+".sock")
}

// PID returns the PID file path for the daemon.
func PID(hash string) string {
	return filepath.Join(runtimeDir(), "pc-"+hash+".pid")
}

// Log returns the default log file path when --log-file is not specified.
func Log(hash string) string {
	return filepath.Join(runtimeDir(), "pc-"+hash+".log")
}
