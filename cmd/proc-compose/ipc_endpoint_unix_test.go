//go:build !windows

package main

import (
	"net"
	"os"
	"path/filepath"
	"testing"
)

// testListenEndpoint creates a listening transport at addr using the same transport
// the product uses on this platform: a Unix domain socket.
func testListenEndpoint(addr string) (net.Listener, error) {
	return net.Listen("unix", addr)
}

// testTempDir returns a short-lived directory for test scratch files.
//
// A short directory is deliberate. A Unix socket path is limited to about 104
// bytes on macOS, and a directory named after the test would push long test names
// past that limit, so the name here is kept short rather than descriptive.
func testTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "pc-t-*")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// testEndpointForTest returns an unused endpoint address for this platform.
func testEndpointForTest(t *testing.T) string {
	t.Helper()
	return filepath.Join(testTempDir(t), "d.sock")
}
