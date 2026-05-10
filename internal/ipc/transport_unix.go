//go:build !windows

package ipc

import (
	"net"
	"os"
	"time"
)

func removeStale(addr string) error {
	os.Remove(addr)
	return nil
}

func platformListen(addr string) (net.Listener, error) {
	ln, err := net.Listen("unix", addr)
	if err != nil {
		return nil, err
	}
	// Restrict the socket file to the owner. Linux ignores file perms for
	// AF_UNIX connect, but BSD/macOS honour them — either way, 0600 is
	// defence-in-depth against another local user issuing restart/reload.
	if err := os.Chmod(addr, 0600); err != nil {
		ln.Close()
		return nil, err
	}
	return ln, nil
}

func platformDial(addr string, timeout time.Duration) (net.Conn, error) {
	return net.DialTimeout("unix", addr, timeout)
}
