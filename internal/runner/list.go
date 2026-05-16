package runner

import (
	"fmt"
	"sort"

	"github.com/anivaryam/proc-compose/internal/ansi"
	"github.com/anivaryam/proc-compose/internal/config"
)

// ListProcesses prints the configured processes for the `list` command.
func (r *Runner) ListProcesses() {
	if r.NoColor {
		ansi.SetDisabled(true)
	}

	names := make([]string, 0, len(r.Config.Processes))
	for name := range r.Config.Processes {
		names = append(names, name)
	}
	sort.Strings(names)

	maxName := 0
	for _, name := range names {
		if len(name) > maxName {
			maxName = len(name)
		}
	}

	fmt.Println()
	fmt.Printf("%s%sproc-compose%s %sprocesses%s\n",
		wrap(colorBold), wrap("\033[36m"), wrap(colorReset),
		wrap(colorDim), wrap(colorReset))
	fmt.Println()

	for i, name := range names {
		p := r.Config.Processes[name]
		c := colorFor(i)
		restart := p.Restart
		if restart == "" {
			restart = "never"
		}
		if p.EffectiveMode() == config.ProcessModeTask {
			restart = "task"
		}
		fmt.Printf("  %s%s%-*s%s  %s  %s(%s)%s\n",
			wrap(colorBold), c, maxName, name, wrap(colorReset),
			p.Cmd,
			wrap(colorDim), restart, wrap(colorReset))
	}
	fmt.Println()
}
