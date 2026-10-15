package runner

import (
	"fmt"
	"slices"
	"strings"

	"github.com/anivaryam/proc-compose/internal/config"
	"github.com/anivaryam/proc-compose/internal/ipc"
)

// Reload re-reads the config file and restarts any processes whose
// definition changed. The returned result tells the IPC client whether
// every requested change took effect:
//
//	"ok"      — config parsed, all changes applied (or none needed)
//	"partial" — applied what we could, but skipped processes added or
//	            removed since the daemon started (hot-add/remove unsupported)
//	"error"   — config could not be parsed
func (r *Runner) Reload() ipc.CommandResult {
	if r.ConfigPath == "" {
		r.reloadEvent("reload: no config path set", true)
		return ipc.CommandResult{Status: "error", Message: "no config path set"}
	}
	newCfg, err := config.Load(r.ConfigPath)
	if err == nil && r.ResolveConfig != nil {
		newCfg, err = r.ResolveConfig(newCfg)
	}
	if err != nil {
		r.reloadEvent(fmt.Sprintf("reload: %v", err), true)
		return ipc.CommandResult{Status: "error", Message: err.Error()}
	}

	r.cfgMu.Lock()
	// Validate immutable runtime semantics before mutating any live definition.
	for name, newProc := range newCfg.Processes {
		if oldProc, exists := r.Config.Processes[name]; exists &&
			(oldProc.EffectiveMode() != newProc.EffectiveMode() || !slices.Equal(oldProc.DependsOn, newProc.DependsOn)) {
			r.cfgMu.Unlock()
			return ipc.CommandResult{Status: "error", Message: fmt.Sprintf("process %q changes mode or dependencies; restart the stack to apply this change", name)}
		}
	}
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
		r.reloadEvent(fmt.Sprintf("reload: new process %q ignored (hot-add not supported)", name), false)
	}
	for _, name := range removed {
		r.reloadEvent(fmt.Sprintf("reload: removed process %q ignored (hot-remove not supported)", name), false)
	}
	var skipped []string
	updated := 0
	store := r.store.Load()
	for _, c := range changes {
		var st *procState
		if store != nil {
			st = store.get(c.name)
		}
		if c.proc.EffectiveMode() == config.ProcessModeTeardown {
			// Nothing is running for it: the teardown phase reads the
			// definition when the stack stops. Admission is checked so the
			// phase never sees a definition change under it.
			r.lifecycleMu.Lock()
			if r.launchAdmissionClosedLocked() {
				r.lifecycleMu.Unlock()
				return ipc.CommandResult{Status: "error", Message: "daemon is shutting down"}
			}
			r.cfgMu.Lock()
			r.Config.Processes[c.name] = c.proc
			r.cfgMu.Unlock()
			r.lifecycleMu.Unlock()
			updated++
			r.reloadEvent(fmt.Sprintf("reload: teardown %q updated; it runs when the stack stops", c.name), false)
			continue
		}
		if c.proc.EffectiveMode() == config.ProcessModeTask {
			skipped = append(skipped, c.name)
			r.reloadEvent(fmt.Sprintf("reload: %q is completed and cannot be restarted; restart the daemon to apply changes", c.name), false)
			continue
		}
		if st != nil {
			r.lifecycleMu.Lock()
			if r.launchAdmissionClosedLocked() {
				r.lifecycleMu.Unlock()
				return ipc.CommandResult{Status: "error", Message: "daemon is shutting down"}
			}
			r.cfgMu.Lock()
			r.Config.Processes[c.name] = c.proc
			r.cfgMu.Unlock()
			result := r.requestRestartLocked(st)
			r.lifecycleMu.Unlock()
			if result.Status != "ok" {
				return result
			}
			r.reloadEvent(fmt.Sprintf("reload: restart requested for %q (config changed)", c.name), false)
		} else {
			skipped = append(skipped, c.name)
		}
	}
	if len(changes) == 0 && len(added) == 0 && len(removed) == 0 {
		r.reloadEvent("reload: no changes detected", false)
	}

	// applied describes the changes that took effect: restarted services and
	// updated teardown definitions.
	restarted := len(changes) - len(skipped) - updated
	applied := func(parts []string) []string {
		if restarted > 0 {
			parts = append(parts, fmt.Sprintf("restarted %d", restarted))
		}
		if updated > 0 {
			parts = append(parts, fmt.Sprintf("updated %d teardown", updated))
		}
		return parts
	}
	if len(added) > 0 || len(removed) > 0 {
		var parts []string
		if len(added) > 0 {
			parts = append(parts, fmt.Sprintf("ignored added: %s", strings.Join(added, ", ")))
		}
		if len(removed) > 0 {
			parts = append(parts, fmt.Sprintf("ignored removed: %s", strings.Join(removed, ", ")))
		}
		return ipc.CommandResult{Status: "partial", Message: strings.Join(applied(parts), "; ") + " (restart the daemon to apply add/remove)"}
	}
	if len(skipped) > 0 {
		parts := append(applied(nil), fmt.Sprintf("skipped %d completed", len(skipped)))
		return ipc.CommandResult{Status: "partial", Message: strings.Join(parts, "; ") + " (restart the daemon to apply changes to completed tasks)"}
	}
	if len(changes) == 0 {
		return ipc.CommandResult{Status: "ok", Message: "no changes detected"}
	}
	return ipc.CommandResult{Status: "ok", Message: strings.Join(applied(nil), "; ")}
}

// reloadEvent serializes reload diagnostics with admission closure. A reload
// that finishes reading config after shutdown cannot emit runner output after
// the bounded Run operation has returned.
func (r *Runner) reloadEvent(message string, isError bool) {
	r.lifecycleMu.Lock()
	defer r.lifecycleMu.Unlock()
	if r.launchAdmissionClosedLocked() {
		return
	}
	r.systemEvent(message, isError)
}

// procChanged returns true if any field of the process definition differs.
func procChanged(a, b config.Process) bool {
	if a.Cmd != b.Cmd || a.Dir != b.Dir || a.Restart != b.Restart ||
		a.MaxRestarts != b.MaxRestarts || a.ShutdownTimeout != b.ShutdownTimeout ||
		a.EnvFile != b.EnvFile || a.Mode != b.Mode || a.ReadyTimeout != b.ReadyTimeout {
		return true
	}
	if !readyWhenEqual(a.ReadyWhen, b.ReadyWhen) || !readyWhenEqual(a.HealthWhen, b.HealthWhen) ||
		a.HealthInterval != b.HealthInterval || a.HealthAction != b.HealthAction || a.ForeignListener != b.ForeignListener {
		return true
	}
	if len(a.Env) != len(b.Env) {
		return true
	}
	for k, v := range a.Env {
		if other, exists := b.Env[k]; !exists || other != v {
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

func readyWhenEqual(a, b *config.ReadyWhen) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return *a == *b
}
