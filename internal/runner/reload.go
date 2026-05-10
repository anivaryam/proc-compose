package runner

import (
	"fmt"
	"strings"

	"github.com/anivaryam/proc-compose/internal/config"
	"github.com/anivaryam/proc-compose/internal/ipc"
)

// Reload re-reads the config file and restarts any processes whose
// definition changed. The returned result tells the IPC client whether
// every requested change took effect:
//   "ok"      — config parsed, all changes applied (or none needed)
//   "partial" — applied what we could, but skipped processes added or
//               removed since the daemon started (hot-add/remove unsupported)
//   "error"   — config could not be parsed
func (r *Runner) Reload() ipc.CommandResult {
	if r.ConfigPath == "" {
		r.systemEvent("reload: no config path set", true)
		return ipc.CommandResult{Status: "error", Message: "no config path set"}
	}
	newCfg, err := config.Load(r.ConfigPath)
	if err != nil {
		r.systemEvent(fmt.Sprintf("reload: %v", err), true)
		return ipc.CommandResult{Status: "error", Message: err.Error()}
	}

	r.cfgMu.Lock()
	type change struct {
		name string
		proc config.Process
	}
	var (
		changes []change
		added   []string
		removed []string
	)
	for name, newProc := range newCfg.Processes {
		oldProc, exists := r.Config.Processes[name]
		if !exists {
			added = append(added, name)
			continue
		}
		if procChanged(oldProc, newProc) {
			r.Config.Processes[name] = newProc
			changes = append(changes, change{name: name, proc: newProc})
		}
	}
	for name := range r.Config.Processes {
		if _, exists := newCfg.Processes[name]; !exists {
			removed = append(removed, name)
		}
	}
	r.cfgMu.Unlock()

	for _, name := range added {
		r.systemEvent(fmt.Sprintf("reload: new process %q ignored (hot-add not supported)", name), false)
	}
	for _, name := range removed {
		r.systemEvent(fmt.Sprintf("reload: removed process %q ignored (hot-remove not supported)", name), false)
	}
	for _, c := range changes {
		if st := r.store.get(c.name); st != nil {
			st.requestRestart()
		}
		r.systemEvent(fmt.Sprintf("reload: restarting %q (config changed)", c.name), false)
	}
	if len(changes) == 0 && len(added) == 0 && len(removed) == 0 {
		r.systemEvent("reload: no changes detected", false)
	}

	if len(added) > 0 || len(removed) > 0 {
		var parts []string
		if len(added) > 0 {
			parts = append(parts, fmt.Sprintf("ignored added: %s", strings.Join(added, ", ")))
		}
		if len(removed) > 0 {
			parts = append(parts, fmt.Sprintf("ignored removed: %s", strings.Join(removed, ", ")))
		}
		if len(changes) > 0 {
			parts = append(parts, fmt.Sprintf("restarted %d", len(changes)))
		}
		return ipc.CommandResult{Status: "partial", Message: strings.Join(parts, "; ") + " (restart the daemon to apply add/remove)"}
	}
	if len(changes) == 0 {
		return ipc.CommandResult{Status: "ok", Message: "no changes detected"}
	}
	return ipc.CommandResult{Status: "ok", Message: fmt.Sprintf("restarted %d", len(changes))}
}

// procChanged returns true if any field of the process definition differs.
func procChanged(a, b config.Process) bool {
	if a.Cmd != b.Cmd || a.Dir != b.Dir || a.Restart != b.Restart ||
		a.MaxRestarts != b.MaxRestarts || a.ShutdownTimeout != b.ShutdownTimeout ||
		a.EnvFile != b.EnvFile {
		return true
	}
	if len(a.Env) != len(b.Env) {
		return true
	}
	for k, v := range a.Env {
		if b.Env[k] != v {
			return true
		}
	}
	if len(a.DependsOn) != len(b.DependsOn) {
		return true
	}
	for i := range a.DependsOn {
		if a.DependsOn[i] != b.DependsOn[i] {
			return true
		}
	}
	return false
}
