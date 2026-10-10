package runner

import (
	"fmt"

	"github.com/anivaryam/proc-compose/internal/ansi"
)

// wrap is a thin alias for ansi.Wrap so existing callsites stay short.
// Colour state lives in the ansi package now (initialised from NO_COLOR,
// updated via ansi.SetDisabled when the user passes --no-color).
func wrap(code string) string { return ansi.Wrap(code) }

const (
	colorReset  = ansi.Reset
	colorBold   = ansi.Bold
	colorDim    = ansi.Dim
	colorRed    = ansi.Red
	colorGreen  = ansi.Green
	colorYellow = ansi.Yellow
)

func colorFor(index int) string {
	return wrap(ansi.ProcessColor(index))
}

// BannerOptions controls PrintBanner behaviour.
type BannerOptions struct {
	Silent bool // suppress banner entirely (e.g. daemon child)
}

// PrintBanner emits the startup header listing configured processes. The
// per-process line says "configured" — the actual "ready" event is logged
// once the process has either started or passed its readiness probe.
//
// Suppressed entirely when the runner is in JSON log mode (mixed text and
// JSON output breaks log aggregator parsers) or when opts.Silent is set.
func PrintBanner(names []string, maxName int, opts BannerOptions) {
	if opts.Silent {
		return
	}
	fmt.Println()
	fmt.Printf("%s%sproc-compose%s %sstarting %d processes%s\n",
		wrap(colorBold), wrap("\033[36m"), wrap(colorReset),
		wrap(colorGreen), len(names), wrap(colorReset))
	fmt.Println()
	for i, name := range names {
		c := colorFor(i)
		fmt.Printf("  %s%s%-*s%s  configured\n", wrap(colorBold), c, maxName, name, wrap(colorReset))
	}
	fmt.Println()
	fmt.Printf("  %sPress Ctrl+C to stop all%s\n", wrap(colorDim), wrap(colorReset))
	fmt.Println()
}

func FormatLine(name string, maxName int, colorIndex int, line string) string {
	c := colorFor(colorIndex)
	return fmt.Sprintf("%s%s%-*s%s %s│%s %s",
		wrap(colorBold), c, maxName, name, wrap(colorReset),
		wrap(colorDim), wrap(colorReset),
		line)
}
