//go:build windows

package monitor

import (
	"os"
	"time"

	"golang.org/x/term"
)

func setupResizeSignal(resize chan<- struct{}) {
	go func() {
		var lastW, lastH int
		if w, h, err := term.GetSize(int(os.Stdout.Fd())); err == nil {
			lastW, lastH = w, h
		}
		for {
			time.Sleep(500 * time.Millisecond)
			if w, h, err := term.GetSize(int(os.Stdout.Fd())); err == nil {
				if w != lastW || h != lastH {
					lastW, lastH = w, h
					select {
					case resize <- struct{}{}:
					default:
					}
				}
			}
		}
	}()
}
