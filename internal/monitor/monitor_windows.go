//go:build windows

package monitor

import (
	"fmt"
	"os"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/term"
)

// enableVTOutput turns on ENABLE_VIRTUAL_TERMINAL_PROCESSING (and
// DISABLE_NEWLINE_AUTO_RETURN) on stdout so the conhost host honours the
// cursor-positioning and SGR escapes the TUI emits. Returns a restorer that
// reinstates the prior mode.
func enableVTOutput() (func(), error) {
	h := windows.Handle(os.Stdout.Fd())
	var mode uint32
	if err := windows.GetConsoleMode(h, &mode); err != nil {
		return func() {}, fmt.Errorf("GetConsoleMode: %w", err)
	}
	newMode := mode | windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING | windows.DISABLE_NEWLINE_AUTO_RETURN
	if newMode == mode {
		return func() {}, nil
	}
	if err := windows.SetConsoleMode(h, newMode); err != nil {
		return func() {}, fmt.Errorf("SetConsoleMode: %w", err)
	}
	return func() { _ = windows.SetConsoleMode(h, mode) }, nil
}

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
