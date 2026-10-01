//go:build windows

package main

import (
	"net"
	"os"
	"strconv"
	"testing"
	"time"

	winio "github.com/Microsoft/go-winio"
)

// testListenEndpoint creates a listening transport at addr using the same transport
// the product uses on this platform: a named pipe, not a Unix socket. Running these
// tests over a real named pipe keeps the CLI's connection handling — address form,
// dial, ack, disconnect — under test on Windows rather than skipping it.
func testListenEndpoint(addr string) (net.Listener, error) {
	return winio.ListenPipe(addr, nil)
}

// testTempDir returns a scratch directory. Windows has no socket path length limit,
// so the standard test directory is used.
func testTempDir(t *testing.T) string {
	t.Helper()
	return t.TempDir()
}

// testEndpointForTest returns an unused named-pipe address for this platform.
//
// A pipe name is not a path: it has no directory component and no extension, and
// backslashes are reserved for the \\.\pipe\ prefix that the transport adds.
func testEndpointForTest(t *testing.T) string {
	t.Helper()
	return "pc-test-" + strconv.FormatInt(time.Now().UnixNano(), 36) + "-" + strconv.Itoa(os.Getpid())
}
