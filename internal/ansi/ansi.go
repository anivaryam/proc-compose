// Package ansi centralizes the ANSI escape codes used by both the runner's
// log formatter and the monitor TUI so the two surfaces share a single
// per-process color table and a single colour-disable switch.
package ansi

import (
	"os"
	"sync/atomic"
)

const (
	Reset   = "\033[0m"
	Bold    = "\033[1m"
	Dim     = "\033[2m"
	Red     = "\033[31m"
	Green   = "\033[32m"
	Yellow  = "\033[33m"
	Cyan    = "\033[36m"
	Reverse = "\033[7m"
)

// disabled is a global toggle that suppresses every escape code returned by
// Wrap / ProcessColor. It is initialised from $NO_COLOR (per the no-color.org
// convention) and may be flipped at runtime via SetDisabled. Stored as an
// int32 so both the runner goroutine and the monitor's render loop can read
// it without locking.
var disabled atomic.Bool

func init() {
	if os.Getenv("NO_COLOR") != "" {
		disabled.Store(true)
	}
}

// SetDisabled toggles colour output globally. Pass true to suppress all
// escape codes returned by Wrap / ProcessColor (used by --no-color and by
// JSON log mode, which must emit clean machine-readable output).
func SetDisabled(d bool) { disabled.Store(d) }

// Disabled reports whether colour output is currently suppressed.
func Disabled() bool { return disabled.Load() }

// Wrap returns code unchanged when colour output is enabled, or "" when
// disabled. Callers can sprinkle Wrap around every escape code without
// branching at every callsite.
func Wrap(code string) string {
	if disabled.Load() {
		return ""
	}
	return code
}

// ProcessColors are the escape codes cycled per managed process.
// Order matters: callers index into this slice by ColorIndex so the same
// process keeps the same color across runner output and monitor TUI.
var ProcessColors = []string{
	"\033[36m", // cyan
	"\033[35m", // magenta
	"\033[33m", // yellow
	"\033[34m", // blue
	"\033[32m", // green
	"\033[91m", // bright red
	"\033[96m", // bright cyan
	"\033[95m", // bright magenta
}

// ProcessColor returns the ANSI code for the given process color index, or
// "" when colour is disabled. Negative indexes are normalised so callers
// can pass hashed values directly.
func ProcessColor(idx int) string {
	if disabled.Load() {
		return ""
	}
	n := len(ProcessColors)
	return ProcessColors[((idx%n)+n)%n]
}
