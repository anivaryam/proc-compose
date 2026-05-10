package ipc

import (
	"net"
	"time"
)

// newServerListener creates a listening transport on the given address.
// On Unix: Unix domain socket.
// On Windows: named pipe via go-winio.
func newServerListener(addr string) (net.Listener, error) {
	if err := removeStale(addr); err != nil {
		return nil, err
	}
	return platformListen(addr)
}

// dialAddr dials the transport at addr with the given timeout.
func dialAddr(addr string, timeout time.Duration) (net.Conn, error) {
	return platformDial(addr, timeout)
}
