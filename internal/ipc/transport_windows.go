//go:build windows

package ipc

import (
	"net"
	"time"

	winio "github.com/Microsoft/go-winio"
)

func removeStale(addr string) error {
	return nil
}

func platformListen(addr string) (net.Listener, error) {
	return winio.ListenPipe(addr, nil)
}

func platformDial(addr string, timeout time.Duration) (net.Conn, error) {
	return winio.DialPipe(addr, &timeout)
}
