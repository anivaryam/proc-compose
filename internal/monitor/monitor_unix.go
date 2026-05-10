//go:build !windows

package monitor

import (
	"os"
	"os/signal"
	"syscall"
)

func setupResizeSignal(resize chan<- struct{}) {
	sigwinch := make(chan os.Signal, 1)
	signal.Notify(sigwinch, syscall.SIGWINCH)
	go func() {
		for range sigwinch {
			resize <- struct{}{}
		}
	}()
}
