//go:build !windows

package monitor

import (
	"os"
	"os/signal"
	"syscall"
)

// enableVTOutput is a no-op on Unix; ANSI escapes are honoured natively.
func enableVTOutput() (func(), error) { return func() {}, nil }

func setupResizeSignal(resize chan<- struct{}) {
	sigwinch := make(chan os.Signal, 1)
	signal.Notify(sigwinch, syscall.SIGWINCH)
	go func() {
		for range sigwinch {
			resize <- struct{}{}
		}
	}()
}
