// Package config handles parsing, validation, and processing of proc-compose YAML configuration files.
package config

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// ReadyWhen defines a probe that proc-compose uses to detect when a process is
// accepting traffic. Exactly one of HTTP, TCP, Cmd, or Log must be set.
type ReadyWhen struct {
	HTTP string `yaml:"http,omitempty"` // URL to GET; ready when response is 2xx
	TCP  string `yaml:"tcp,omitempty"`  // host:port; ready when TCP connect succeeds
	Cmd  string `yaml:"cmd,omitempty"`  // shell command run in the process's dir and env; ready when it exits 0
	Log  string `yaml:"log,omitempty"`  // Go regex; ready when matched in stdout
}

const (
	ProcessModeService = "service"
	ProcessModeTask    = "task"
	// ProcessModeTeardown runs once after the stack has stopped and every
	// managed process is verified terminated, to release what the stack
	// held. It never starts with the stack and nothing can depend on it.
	ProcessModeTeardown = "teardown"
)

// MaxTeardownShutdownTimeout caps a teardown process's shutdown_timeout so
// its termination grace always leaves the command most of the fixed teardown
// budget.
const MaxTeardownShutdownTimeout = 5

// Health actions: what proc-compose does when a running service fails its
// health_when probe healthThreshold times in a row.
const (
	HealthActionReport  = "report"  // mark the service unhealthy and keep it running
	HealthActionRestart = "restart" // cancel the incarnation through the restart policy
)

// Foreign-listener policies: what an http or tcp probe does with an answer
// from a process outside the probed process's tree (see the runner's probe
// attribution).
const (
	ForeignListenerAllow  = "allow"  // the probe passes; the holder is reported
	ForeignListenerReject = "reject" // the attempt fails, naming the holder
)

// DefaultHealthInterval is the health_when period in seconds when
// health_interval is unset.
const DefaultHealthInterval = 10

type Process struct {
	Cmd             string            `yaml:"cmd"`
	Dir             string            `yaml:"dir,omitempty"`
	EnvFile         string            `yaml:"env_file,omitempty"` // path to .env file; merged before env block
	Env             map[string]string `yaml:"env,omitempty"`
	Mode            string            `yaml:"mode,omitempty"`             // "service" (default), "task", "teardown"
	Restart         string            `yaml:"restart,omitempty"`          // "never" (default), "on-failure", "always"
	MaxRestarts     int               `yaml:"max_restarts,omitempty"`     // 0 = unlimited; only applies when restart != "never"
	ShutdownTimeout int               `yaml:"shutdown_timeout,omitempty"` // seconds before SIGKILL; 0 = platform default (5s on Unix)
	ReadyWhen       *ReadyWhen        `yaml:"ready_when,omitempty"`       // probe to determine when process is ready
	ReadyTimeout    int               `yaml:"ready_timeout,omitempty"`    // seconds to wait for readiness before failing; 0 = default (60s); -1 = no limit
	DependsOn       []string          `yaml:"depends_on,omitempty"`       // wait for these processes to be ready before starting
	Ports           map[string]int    `yaml:"ports,omitempty"`            // named preferred ports; 0 selects an ephemeral port
	HealthWhen      *ReadyWhen        `yaml:"health_when,omitempty"`      // ongoing probe after readiness: http, tcp or cmd (no log)
	HealthInterval  int               `yaml:"health_interval,omitempty"`  // seconds between health checks; 0 = default (10); requires health_when
	HealthAction    string            `yaml:"health_action,omitempty"`    // "report" (default) or "restart"; requires health_when
	ForeignListener string            `yaml:"foreign_listener,omitempty"` // "allow" (default) or "reject"; requires an http or tcp ready_when or health_when
}

func (p Process) EffectiveMode() string {
	if p.Mode == "" {
		return ProcessModeService
	}
	return p.Mode
}

// EffectiveHealthInterval is the health_when period in seconds.
func (p Process) EffectiveHealthInterval() int {
	if p.HealthInterval <= 0 {
		return DefaultHealthInterval
	}
	return p.HealthInterval
}

// EffectiveForeignListener is the configured foreign_listener or its default.
func (p Process) EffectiveForeignListener() string {
	if p.ForeignListener == "" {
		return ForeignListenerAllow
	}
	return p.ForeignListener
}

// EffectiveHealthAction is the configured health_action or its default.
func (p Process) EffectiveHealthAction() string {
	if p.HealthAction == "" {
		return HealthActionReport
	}
	return p.HealthAction
}

type Merge struct {
	Client        int      `yaml:"client,omitempty"`
	Server        int      `yaml:"server,omitempty"`
	Port          int      `yaml:"port,omitempty"`
	ApiPrefixes   []string `yaml:"api_prefixes,omitempty"`
	Routes        []string `yaml:"routes,omitempty"`
	ClientProcess string   `yaml:"client_process,omitempty"` // override which process is the client
}

type Config struct {
	Merge     *Merge             `yaml:"merge,omitempty"`
	Processes map[string]Process `yaml:"processes"`
	Resources []string           `yaml:"resources,omitempty"` // exclusive resource labels held while the stack runs
}

var processNameRe = regexp.MustCompile(`^[A-Za-z0-9][-A-Za-z0-9._]*[A-Za-z0-9]$|^[A-Za-z0-9]$`)

// portNameRe is the port-name rule: a process name without the dot. An
// endpoint reference name is the process name and the port name joined by a
// dot, and both the session expander and config plan split that name at the
// last dot — so a dot in a port name lets two different endpoints claim one
// reference name. Forbidding the dot makes the mapping injective, so the
// plan and the session agree by construction.
var portNameRe = regexp.MustCompile(`^[A-Za-z0-9][-A-Za-z0-9_]*[A-Za-z0-9]$|^[A-Za-z0-9]$`)

// MergePortProcessName is the name of the proxy process the merge section
// generates. It is reserved: a user process with this name would be
// overwritten by the generated one.
//
// It is exported so the session can tell whether the proxy is part of the
// selected process set, which is what decides whether the merge port is
// leased.
const MergePortProcessName = "merge-port"

// maxResourceLabel is the longest resource label, in bytes of the trimmed
// label, that Load accepts.
const maxResourceLabel = 256

// maxEnvFileLine is the longest env_file line, in bytes without its line
// terminator, that parseEnvFile accepts.
const maxEnvFileLine = 1 << 20

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("cannot read %s: %w", path, err)
	}

	var cfg Config
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("invalid YAML in %s: %w", path, err)
	}

	if len(cfg.Processes) == 0 && cfg.Merge == nil {
		return nil, invalid(CodeNoProcesses, nil, nil, "no processes defined in %s", path)
	}

	// Resolve config dir to absolute so relative process dirs work from any CWD.
	configDir := filepath.Dir(path)
	if abs, err := filepath.Abs(configDir); err == nil {
		configDir = abs
	}
	// Invariant after Load: every non-empty relative YAML process.dir is an
	// absolute path rooted at the config file directory, not the caller's CWD.
	normalizeProcessDirs(cfg.Processes, configDir)

	// Every env_file is read once, up front: the merge wiring needs the PORT
	// an env_file declares before the validation loop merges the file into
	// the process env. A read error is kept and reported by that loop, at the
	// process's env_file and in the usual order.
	envFiles := readEnvFiles(cfg.Processes, configDir)

	if cfg.Merge != nil {
		// The merge section generates a proxy process under a reserved name.
		// A user process with that name would be silently overwritten, so
		// the config is refused instead.
		if _, exists := cfg.Processes[MergePortProcessName]; exists {
			return nil, invalid(CodeReservedProcessName, []any{"processes", MergePortProcessName}, nil,
				"process name %q is reserved for the proxy the merge section generates; rename the process", MergePortProcessName)
		}

		// Only services can be merge upstreams, and every selection below —
		// the api_prefixes scan, the upstream wiring and the client env
		// injection — picks from the same candidates, so they never disagree
		// about which process is the client or the server.
		candidates := serviceUpstreams(cfg.Processes)
		if err := checkClientProcess(cfg.Processes, cfg.Merge); err != nil {
			return nil, err
		}
		served := servedPorts(cfg.Processes, envFiles)

		// Auto-detect api_prefixes by scanning server source when none are configured.
		if len(cfg.Merge.ApiPrefixes) == 0 && len(cfg.Merge.Routes) == 0 {
			if server := findServerProc(candidates, served, cfg.Merge); server != "" {
				if detected := scanRoutes(procDir(candidates[server].Dir, configDir)); len(detected) > 0 {
					cfg.Merge.ApiPrefixes = detected
				}
			}
		}

		cmd, err := buildMergeCmd(cfg.Merge)
		if err != nil {
			// A field-scoped failure (an out-of-range merge port) carries its
			// own path, so the diagnostic names the offending field instead
			// of the whole merge section. The other merge failures are about
			// the section as a whole.
			var verr *ValidationError
			if errors.As(err, &verr) {
				return nil, verr
			}
			return nil, invalid(CodeInvalidMerge, []any{"merge"}, err, "%s", err)
		}
		if cfg.Processes == nil {
			cfg.Processes = make(map[string]Process)
		}

		// Identify upstream processes (those merge-port will proxy to) and
		// wire dependency + readiness so the proxy can never start before
		// its backends are accepting connections. Without this, merge-port
		// boots in parallel with the client/server and any traffic that
		// arrives before the backends bind their ports gets a 502.
		upstreams, err := resolveUpstreamProcesses(candidates, served, cfg.Merge)
		if err != nil {
			return nil, err
		}
		for name, port := range upstreams {
			injectTCPReadyIfMissing(cfg.Processes, name, port)
		}

		mergeProc := Process{
			Cmd:     cmd,
			Restart: "on-failure",
			// Probe merge-port's own listen port so any downstream process
			// (e.g. a tunnel that depends on merge-port) doesn't race the
			// proxy's bind. cfg.Merge.Port is finalised inside buildMergeCmd.
			ReadyWhen: &ReadyWhen{TCP: fmt.Sprintf("localhost:%d", cfg.Merge.Port)},
		}
		// depends_on must be deterministic so the runner's startup order
		// (and any error messages) are stable across runs.
		depNames := make([]string, 0, len(upstreams))
		for name := range upstreams {
			depNames = append(depNames, name)
		}
		sort.Strings(depNames)
		mergeProc.DependsOn = depNames
		cfg.Processes[MergePortProcessName] = mergeProc

		// Auto-inject the merge-port URL into the client process env so users
		// don't need to edit .env or remove hardcoded fallback ports.
		if clientName := findClientProc(candidates, served, cfg.Merge); clientName != "" {
			clientDir := procDir(cfg.Processes[clientName].Dir, configDir)
			injectClientAPIEnv(cfg.Processes, clientName, clientDir, cfg.Merge)
		}
	}

	// Validate in name order so the first reported error is deterministic.
	for _, name := range SortedProcessNames(cfg.Processes) {
		proc := cfg.Processes[name]
		at := func(field ...any) []any { return append([]any{"processes", name}, field...) }
		portNames := SortedProcessNames(proc.Ports)
		for _, portName := range portNames {
			// The port-name rule runs before the collision safety net: a
			// dotted port name is a name error, not a collision.
			if !portNameRe.MatchString(portName) {
				return nil, invalid(CodeInvalidPort, at("ports", portName), nil, "process %q: invalid port name %q (`.` is not allowed in port names)", name, portName)
			}
			if preferred := proc.Ports[portName]; preferred < 0 || preferred > 65535 {
				return nil, invalid(CodeInvalidPort, at("ports", portName), nil, "process %q: invalid named port %q (%d)", name, portName, preferred)
			}
		}
		if !processNameRe.MatchString(name) {
			return nil, invalid(CodeInvalidProcessName, at(), nil, "process %q has an invalid name (must match [-A-Za-z0-9._], start/end with alphanumeric)", name)
		}
		if proc.Cmd == "" {
			return nil, invalid(CodeMissingCmd, at(), nil, "process %q has no cmd", name)
		}
		if proc.Restart == "" {
			proc.Restart = "never"
		}
		if !slices.Contains(RestartPolicies, proc.Restart) {
			return nil, invalid(CodeInvalidRestart, at("restart"), nil, "process %q has invalid restart policy %q (use never, on-failure, or always)", name, proc.Restart)
		}
		if proc.Mode != "" && !slices.Contains(ProcessModes, proc.Mode) {
			return nil, invalid(CodeInvalidMode, at("mode"), nil, "process %q: invalid mode %q (use service, task, or teardown)", name, proc.Mode)
		}
		if proc.Mode == ProcessModeTeardown {
			if err := validateTeardown(name, at, proc); err != nil {
				return nil, err
			}
		}
		if proc.Mode == ProcessModeTask {
			if proc.ReadyWhen != nil {
				return nil, invalid(CodeTaskIncompatible, at("ready_when"), nil, "process %q: mode: task cannot use ready_when", name)
			}
			if proc.ReadyTimeout != 0 {
				return nil, invalid(CodeTaskIncompatible, at("ready_timeout"), nil, "process %q: mode: task cannot use ready_timeout", name)
			}
			if proc.Restart != "never" {
				return nil, invalid(CodeTaskIncompatible, at("restart"), nil, "process %q: mode: task cannot use restart: %s (only restart: never is allowed)", name, proc.Restart)
			}
			if proc.MaxRestarts > 0 {
				return nil, invalid(CodeTaskIncompatible, at("max_restarts"), nil, "process %q: mode: task cannot use max_restarts", name)
			}
			if proc.HealthWhen != nil {
				return nil, invalid(CodeTaskIncompatible, at("health_when"), nil, "process %q: mode: task cannot use health_when", name)
			}
			if proc.HealthInterval != 0 {
				return nil, invalid(CodeTaskIncompatible, at("health_interval"), nil, "process %q: mode: task cannot use health_interval", name)
			}
			if proc.HealthAction != "" {
				return nil, invalid(CodeTaskIncompatible, at("health_action"), nil, "process %q: mode: task cannot use health_action", name)
			}
			if proc.ForeignListener != "" {
				return nil, invalid(CodeTaskIncompatible, at("foreign_listener"), nil, "process %q: mode: task cannot use foreign_listener", name)
			}
		}
		if proc.MaxRestarts < 0 {
			return nil, invalid(CodeInvalidValue, at("max_restarts"), nil, "process %q: max_restarts must be >= 0", name)
		}
		if proc.MaxRestarts > 0 && proc.Restart == "never" {
			return nil, invalid(CodeInvalidValue, at("max_restarts"), nil, "process %q: max_restarts has no effect with restart: never", name)
		}
		if proc.ShutdownTimeout < 0 {
			return nil, invalid(CodeInvalidValue, at("shutdown_timeout"), nil, "process %q: shutdown_timeout must be >= 0", name)
		}
		if proc.ReadyTimeout < -1 {
			return nil, invalid(CodeInvalidValue, at("ready_timeout"), nil, "process %q: ready_timeout must be >= -1 (-1 = no limit)", name)
		}
		if proc.ReadyWhen != nil {
			if err := validateProbe(name, at("ready_when"), proc.ReadyWhen, true); err != nil {
				return nil, err
			}
		}
		if err := validateHealth(name, at, proc); err != nil {
			return nil, err
		}
		if err := validateForeignListener(name, at, proc); err != nil {
			return nil, err
		}
		if proc.EnvFile != "" {
			fileVars, err := envFiles[name].vars, envFiles[name].err
			if err != nil {
				return nil, invalid(CodeEnvFile, at("env_file"), err, "process %q: %w", name, err)
			}
			if len(fileVars) > 0 {
				merged := make(map[string]string, len(fileVars)+len(proc.Env))
				for k, v := range fileVars {
					merged[k] = v
				}
				// explicit env block overrides env_file
				for k, v := range proc.Env {
					merged[k] = v
				}
				proc.Env = merged
			}
		}
		cfg.Processes[name] = proc
	}

	// Safety net: two endpoints whose reference names collide would make one
	// of them unreachable. The dot-free port-name rule above makes this
	// unreachable through Load; the check stays so the two representations
	// stay injective if that rule is ever relaxed.
	if err := checkReferenceCollisions(cfg.Processes); err != nil {
		return nil, err
	}

	// Validate depends_on references after all processes (including merge-port) are registered.
	for _, name := range SortedProcessNames(cfg.Processes) {
		for i, dep := range cfg.Processes[name].DependsOn {
			if dep == name {
				return nil, invalid(CodeSelfDependency, []any{"processes", name, "depends_on", i}, nil, "process %q cannot depend on itself", name)
			}
			depProc, ok := cfg.Processes[dep]
			if !ok {
				return nil, invalid(CodeUnknownDependency, []any{"processes", name, "depends_on", i}, nil, "process %q depends_on unknown process %q", name, dep)
			}
			if depProc.Mode == ProcessModeTeardown {
				return nil, invalid(CodeTeardownDependency, []any{"processes", name, "depends_on", i}, nil, "process %q depends_on teardown process %q, which runs only after the stack stops", name, dep)
			}
		}
	}

	if err := detectCycles(cfg.Processes); err != nil {
		return nil, err
	}
	// Resource labels are normalised once, here: the trimmed label is the
	// lease identity, the duplicate check and Project.Resources all use it.
	// Two independent checks: an exactly repeated label is still a
	// configuration error, while a label that differs only in surrounding
	// whitespace names the same resource and is skipped (first wins).
	seenResources := make(map[string]bool)
	seenTrimmed := make(map[string]bool)
	normalized := make([]string, 0, len(cfg.Resources))
	for i, resource := range cfg.Resources {
		if seenResources[resource] {
			return nil, invalid(CodeInvalidResource, []any{"resources", i}, nil, "invalid or duplicate resource label %q", resource)
		}
		seenResources[resource] = true
		trimmed := strings.TrimSpace(resource)
		if trimmed == "" || len(trimmed) > maxResourceLabel || strings.ContainsAny(trimmed, "\r\n\x00") {
			return nil, invalid(CodeInvalidResource, []any{"resources", i}, nil, "invalid or duplicate resource label %q", resource)
		}
		if seenTrimmed[trimmed] {
			continue
		}
		seenTrimmed[trimmed] = true
		normalized = append(normalized, trimmed)
	}
	if len(cfg.Resources) > 0 {
		cfg.Resources = normalized
	}

	return &cfg, nil
}

// validateTeardown refuses every field a teardown process cannot use: it
// runs once, after the stack stopped, so it has no readiness, restarts,
// health checks, ports or dependencies. Its shutdown_timeout is capped so the
// command keeps most of the teardown budget.
func validateTeardown(name string, at func(...any) []any, proc Process) error {
	refuse := func(field string) error {
		return invalid(CodeTaskIncompatible, at(field), nil, "process %q: mode: teardown cannot use %s", name, field)
	}
	switch {
	case proc.ReadyWhen != nil:
		return refuse("ready_when")
	case proc.ReadyTimeout != 0:
		return refuse("ready_timeout")
	case proc.Restart != "never":
		return invalid(CodeTaskIncompatible, at("restart"), nil, "process %q: mode: teardown cannot use restart: %s (only restart: never is allowed)", name, proc.Restart)
	case proc.MaxRestarts > 0:
		return refuse("max_restarts")
	case proc.HealthWhen != nil:
		return refuse("health_when")
	case proc.HealthInterval != 0:
		return refuse("health_interval")
	case proc.HealthAction != "":
		return refuse("health_action")
	case proc.ForeignListener != "":
		return refuse("foreign_listener")
	case len(proc.Ports) > 0:
		return refuse("ports")
	case len(proc.DependsOn) > 0:
		return refuse("depends_on")
	case proc.ShutdownTimeout > MaxTeardownShutdownTimeout:
		return invalid(CodeInvalidValue, at("shutdown_timeout"), nil, "process %q: mode: teardown allows shutdown_timeout up to %d", name, MaxTeardownShutdownTimeout)
	}
	return nil
}

// validateProbe checks one probe block at path (processes.<name>.<field>):
// exactly one kind is set, and a log probe is a valid regex. allowLog is false
// for health_when: a log line matches once, so it cannot be re-checked.
func validateProbe(name string, path []any, rw *ReadyWhen, allowLog bool) error {
	field := FormatPath(path[2:])
	n := 0
	for _, kind := range []string{rw.HTTP, rw.TCP, rw.Cmd, rw.Log} {
		if kind != "" {
			n++
		}
	}
	if !allowLog {
		if n == 1 && rw.Log != "" {
			return invalid(CodeInvalidReadyWhen, append(slices.Clone(path), "log"), nil, "process %q: %s cannot use log (use http, tcp, or cmd)", name, field)
		}
		if n != 1 {
			return invalid(CodeInvalidReadyWhen, path, nil, "process %q: %s must specify exactly one of http, tcp, or cmd", name, field)
		}
		return nil
	}
	if n != 1 {
		return invalid(CodeInvalidReadyWhen, path, nil, "process %q: %s must specify exactly one of http, tcp, cmd, or log", name, field)
	}
	if rw.Log != "" {
		if _, err := regexp.Compile(rw.Log); err != nil {
			return invalid(CodeInvalidReadyWhen, append(slices.Clone(path), "log"), err, "process %q: %s.log is not a valid regex: %w", name, field, err)
		}
	}
	return nil
}

// validateHealth checks the health_* fields of a service. proc.Restart must
// already carry its default. Task processes are refused earlier.
func validateHealth(name string, at func(...any) []any, proc Process) error {
	if proc.HealthWhen == nil {
		if proc.HealthInterval != 0 {
			return invalid(CodeInvalidValue, at("health_interval"), nil, "process %q: health_interval requires health_when", name)
		}
		if proc.HealthAction != "" {
			return invalid(CodeInvalidValue, at("health_action"), nil, "process %q: health_action requires health_when", name)
		}
		return nil
	}
	if err := validateProbe(name, at("health_when"), proc.HealthWhen, false); err != nil {
		return err
	}
	if proc.HealthInterval < 0 {
		return invalid(CodeInvalidValue, at("health_interval"), nil, "process %q: health_interval must be >= 0 (0 = default %d)", name, DefaultHealthInterval)
	}
	if proc.HealthAction != "" && !slices.Contains(HealthActions, proc.HealthAction) {
		return invalid(CodeInvalidValue, at("health_action"), nil, "process %q: invalid health_action %q (use report or restart)", name, proc.HealthAction)
	}
	if proc.HealthAction == HealthActionRestart && proc.Restart == "never" {
		return invalid(CodeInvalidValue, at("health_action"), nil, "process %q: health_action: restart needs restart: on-failure or always", name)
	}
	return nil
}

// validateForeignListener checks foreign_listener: a known policy, on a
// process with an http or tcp probe for it to apply to.
func validateForeignListener(name string, at func(...any) []any, proc Process) error {
	if proc.ForeignListener == "" {
		return nil
	}
	if !slices.Contains(ForeignListenerPolicies, proc.ForeignListener) {
		return invalid(CodeInvalidValue, at("foreign_listener"), nil, "process %q: invalid foreign_listener %q (use allow or reject)", name, proc.ForeignListener)
	}
	socketProbe := func(rw *ReadyWhen) bool { return rw != nil && (rw.HTTP != "" || rw.TCP != "") }
	if !socketProbe(proc.ReadyWhen) && !socketProbe(proc.HealthWhen) {
		return invalid(CodeInvalidValue, at("foreign_listener"), nil, "process %q: foreign_listener requires an http or tcp ready_when or health_when", name)
	}
	return nil
}

// HasRuntimeTemplate reports whether a process dir still holds a runtime
// template placeholder. The session expands ${session.*} and ${config.*}
// when it starts, so such a dir cannot be resolved statically by anything
// that only reads the config (the doctor, for example).
func HasRuntimeTemplate(dir string) bool {
	return strings.Contains(dir, "${session.") || strings.Contains(dir, "${config.")
}

func normalizeProcessDirs(processes map[string]Process, configDir string) {
	for name, proc := range processes {
		if proc.Dir == "" || HasRuntimeTemplate(proc.Dir) {
			continue
		}
		if filepath.IsAbs(proc.Dir) {
			proc.Dir = filepath.Clean(proc.Dir)
		} else {
			proc.Dir = filepath.Clean(filepath.Join(configDir, proc.Dir))
		}
		processes[name] = proc
	}
}

// SelectedNames is the single dependency-closure used by planning, startup,
// readiness waiting, and dry-run output. Teardown processes are never
// selected; TeardownNames lists them.
func SelectedNames(cfg *Config, filter []string) []string {
	selected := make(map[string]bool)
	var include func(string)
	include = func(name string) {
		if selected[name] {
			return
		}
		proc, exists := cfg.Processes[name]
		if !exists || proc.Mode == ProcessModeTeardown {
			return
		}
		selected[name] = true
		for _, dep := range proc.DependsOn {
			include(dep)
		}
	}
	if len(filter) == 0 {
		for name := range cfg.Processes {
			include(name)
		}
	} else {
		for _, name := range filter {
			include(name)
		}
	}
	names := make([]string, 0, len(selected))
	for name := range selected {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// TeardownNames lists the teardown processes in the order they run: by name.
// They run for the whole stack, whatever filter started it.
func TeardownNames(cfg *Config) []string {
	var names []string
	for name, proc := range cfg.Processes {
		if proc.Mode == ProcessModeTeardown {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

// CheckSelector reports whether name can be selected to start: it must be a
// configured process and not a teardown process.
func CheckSelector(cfg *Config, name string) error {
	proc, ok := cfg.Processes[name]
	switch {
	case !ok:
		return fmt.Errorf("unknown process %q", name)
	case proc.Mode == ProcessModeTeardown:
		return fmt.Errorf("process %q is a teardown process; it runs only after the stack stops and cannot be selected", name)
	}
	return nil
}

// detectCycles checks for circular dependencies using DFS with three-color marking.
func detectCycles(processes map[string]Process) error {
	const (
		white = 0 // unvisited
		gray  = 1 // visiting (on current path)
		black = 2 // fully explored
	)

	color := make(map[string]int, len(processes))
	path := make([]string, 0)

	var visit func(name string) error
	visit = func(name string) error {
		color[name] = gray
		path = append(path, name)
		for _, dep := range processes[name].DependsOn {
			switch color[dep] {
			case gray:
				// Found a cycle — build the cycle path from dep back to dep.
				cycle := []string{dep}
				for i := len(path) - 1; i >= 0; i-- {
					cycle = append(cycle, path[i])
					if path[i] == dep {
						break
					}
				}
				// Reverse to show forward order.
				for i, j := 0, len(cycle)-1; i < j; i, j = i+1, j-1 {
					cycle[i], cycle[j] = cycle[j], cycle[i]
				}
				return invalid(CodeDependencyCycle, []any{"processes", name, "depends_on", slices.Index(processes[name].DependsOn, dep)}, nil, "circular dependency: %s", strings.Join(cycle, " -> "))
			case white:
				if err := visit(dep); err != nil {
					return err
				}
			}
		}
		path = path[:len(path)-1]
		color[name] = black
		return nil
	}

	// Sort names for deterministic error messages.
	names := make([]string, 0, len(processes))
	for name := range processes {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		if color[name] == white {
			if err := visit(name); err != nil {
				return err
			}
		}
	}
	return nil
}

func buildMergeCmd(m *Merge) (string, error) {
	routeMode := len(m.Routes) > 0
	simpleMode := m.Client != 0 || m.Server != 0 || len(m.ApiPrefixes) > 0

	if routeMode && simpleMode {
		return "", fmt.Errorf("merge: routes cannot be combined with client, server, or api_prefixes")
	}
	if !routeMode && !simpleMode {
		return "", fmt.Errorf("merge: must specify client/server or routes")
	}

	var parts []string
	parts = append(parts, "merge-port")

	if routeMode {
		for i, r := range m.Routes {
			if err := checkRouteTarget(i, r); err != nil {
				return "", err
			}
			parts = append(parts, fmt.Sprintf("--route %s", r))
		}
	} else {
		if m.Client == 0 {
			return "", fmt.Errorf("merge.client port is required")
		}
		if m.Server == 0 {
			return "", fmt.Errorf("merge.server port is required")
		}
		if err := checkPortRange("client", m.Client); err != nil {
			return "", err
		}
		if err := checkPortRange("server", m.Server); err != nil {
			return "", err
		}
		parts = append(parts, fmt.Sprintf("--client %d", m.Client))
		parts = append(parts, fmt.Sprintf("--server %d", m.Server))
		for _, p := range m.ApiPrefixes {
			parts = append(parts, fmt.Sprintf("--api-prefix %s", p))
		}
	}

	if m.Port == 0 {
		if portEnv := os.Getenv("PORT"); portEnv != "" {
			if p, err := strconv.Atoi(portEnv); err == nil && p > 0 {
				m.Port = p
			}
		}
		if m.Port == 0 {
			m.Port = 8080
		}
	}
	// The range check runs after the $PORT fallback: 0 means unset and is
	// resolved to a real port before the port is judged.
	if err := checkPortRange("port", m.Port); err != nil {
		return "", err
	}
	parts = append(parts, fmt.Sprintf("--port %d", m.Port))

	return strings.Join(parts, " "), nil
}

// checkPortRange refuses a port outside 1-65535. Zero means unset and must be
// resolved before the check. The error carries the precise merge field path,
// so the diagnostic names the offending field instead of the whole merge
// section; the code stays invalid_merge.
func checkPortRange(field string, port int) error {
	if port < 1 || port > 65535 {
		return invalid(CodeInvalidMerge, []any{"merge", field}, nil,
			"merge.%s port %d is out of range (1-65535)", field, port)
	}
	return nil
}

// integerRe matches a route target written as an integer: a port number, as
// opposed to a URL. It is the form strconv.Atoi parses.
var integerRe = regexp.MustCompile(`^[+-]?[0-9]+$`)

// checkRouteTarget refuses a route whose target is a port number outside
// 1-65535, at merge.routes[i]. A target written as a URL is passed to
// merge-port as written.
func checkRouteTarget(i int, route string) error {
	target, ok := routeTarget(route)
	if !ok || !integerRe.MatchString(target) {
		return nil
	}
	if port, err := strconv.Atoi(target); err != nil || port < 1 || port > 65535 {
		return invalid(CodeInvalidMerge, []any{"merge", "routes", i}, nil,
			"merge.routes[%d] target port %s is out of range (1-65535)", i, target)
	}
	return nil
}

// ─── Route auto-detection ─────────────────────────────────────────────────────

var routeRegexps = []*regexp.Regexp{
	// Express/Koa/Fastify/Hono: app.use('/x'), server.get('/x'), fastify.all('/x')
	// Deliberately excludes 'router' — sub-router paths are partial and only
	// meaningful relative to the prefix they're mounted under (e.g.
	// router.post('/login') inside a file mounted at '/api/auth' is NOT a
	// top-level prefix; it would incorrectly add '/login' to api_prefixes).
	regexp.MustCompile(`(?:app|server|fastify)\s*\.\s*(?:use|get|post|put|patch|delete|all|route)\s*\(\s*['"](\s*/[^/'")\s]+)`),
	// Go chi/gorilla: r.Route("/x"), r.Mount("/x"), mux.Handle("/x"), http.HandleFunc("/x")
	regexp.MustCompile(`(?:\w+)\s*\.\s*(?:Route|Mount|Handle|HandleFunc)\s*\(\s*"(/[^/"]+)`),
}

var skipDirs = map[string]bool{
	"node_modules": true, ".git": true, "dist": true, "build": true,
	".next": true, "vendor": true, "coverage": true, "__pycache__": true,
	".cache": true, "tmp": true, "temp": true, ".turbo": true,
}

func scanRoutes(serverDir string) []string {
	seen := make(map[string]bool)

	filepath.WalkDir(serverDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if !isSourceFile(path) {
			return nil
		}

		f, err := os.Open(path)
		if err != nil {
			return nil
		}
		defer f.Close()

		scanner := bufio.NewScanner(f)
		for scanner.Scan() {
			line := scanner.Text()
			for _, re := range routeRegexps {
				if m := re.FindStringSubmatch(line); len(m) >= 2 {
					prefix := topPrefix(strings.TrimSpace(m[1]))
					if prefix != "/" && strings.HasPrefix(prefix, "/") {
						seen[prefix] = true
					}
				}
			}
		}
		return nil
	})

	prefixes := make([]string, 0, len(seen))
	for p := range seen {
		prefixes = append(prefixes, p)
	}
	sort.Strings(prefixes)
	return prefixes
}

func isSourceFile(path string) bool {
	base := filepath.Base(path)
	lower := strings.ToLower(base)
	// Skip test files.
	if strings.Contains(lower, ".test.") || strings.Contains(lower, ".spec.") ||
		strings.HasSuffix(lower, "_test.go") {
		return false
	}
	switch strings.ToLower(filepath.Ext(path)) {
	case ".js", ".ts", ".mjs", ".cjs", ".go":
		return true
	}
	return false
}

// topPrefix returns the first path segment: "/api/users" → "/api".
func topPrefix(path string) string {
	trimmed := strings.TrimPrefix(path, "/")
	parts := strings.SplitN(trimmed, "/", 2)
	if parts[0] == "" {
		return "/"
	}
	return "/" + parts[0]
}

// ─── Client env auto-injection ────────────────────────────────────────────────

// Common frontend env var names that hold the API base URL.
var apiEnvVars = []string{
	"VITE_API_BASE_URL",
	"VITE_API_URL",
	"VITE_SERVER_URL",
	"REACT_APP_API_URL",
	"REACT_APP_BASE_URL",
	"REACT_APP_API_BASE_URL",
	"NEXT_PUBLIC_API_URL",
	"NEXT_PUBLIC_API_BASE_URL",
	"NUXT_PUBLIC_API_BASE",
	"PUBLIC_API_URL",
}

func injectClientAPIEnv(procs map[string]Process, clientName, clientDir string, m *Merge) {
	proc := procs[clientName]

	// Prefer .env.example as the canonical shape; fall back to .env.
	envFile := filepath.Join(clientDir, ".env.example")
	if _, err := os.Stat(envFile); err != nil {
		envFile = filepath.Join(clientDir, ".env")
	}
	fileEnv, _ := parseEnvFile(envFile) // speculative; OK if missing

	mergeBase := fmt.Sprintf("http://localhost:%d", m.Port)
	changed := false

	for _, varName := range apiEnvVars {
		// Never override what the user explicitly set in the proc-compose env block.
		if proc.Env != nil {
			if _, userSet := proc.Env[varName]; userSet {
				continue
			}
		}

		exampleVal, found := fileEnv[varName]
		if !found {
			continue
		}

		injected := rebaseURL(exampleVal, mergeBase)
		if proc.Env == nil {
			proc.Env = make(map[string]string)
		}
		proc.Env[varName] = injected
		changed = true
	}

	if changed {
		procs[clientName] = proc
	}
}

// rebaseURL replaces the scheme+host of exampleVal with mergeBase while
// preserving any path suffix (e.g. "/api" from "http://192.168.1.45:3000/api").
func rebaseURL(exampleVal, mergeBase string) string {
	if exampleVal == "" {
		return mergeBase
	}
	u, err := url.Parse(exampleVal)
	if err != nil || u.Host == "" {
		return mergeBase
	}
	path := strings.TrimRight(u.Path, "/")
	return mergeBase + path
}

// ─── Process lookup helpers ───────────────────────────────────────────────────

// resolveUpstreamProcesses returns a map of {process_name → port} for every
// process that merge-port will proxy traffic to. Used by Load() to wire
// readiness probes onto upstreams and depends_on onto merge-port itself.
//
// procs holds the candidates only (serviceUpstreams): a task or teardown
// process is not waited for, and the merge wiring must not write a
// ready_when probe into it.
//
// Simple mode: client + server (looked up by served PORT, then by name,
// honouring merge.client_process). Route mode: every process whose served
// PORT matches a "/prefix=PORT" target.
//
// Processes that already declare ready_when are left alone — the upstream
// dependency edge is still added (the user's probe is what we wait on).
func resolveUpstreamProcesses(procs map[string]Process, served map[string]ServedPort, m *Merge) (map[string]int, error) {
	out := make(map[string]int)
	if len(m.Routes) > 0 {
		for _, route := range m.Routes {
			port := parseRoutePort(route)
			if port == 0 {
				continue
			}
			if name := procWithPort(procs, served, port); name != "" {
				out[name] = port
			}
		}
		return out, nil
	}
	if name := findClientProc(procs, served, m); name != "" && m.Client > 0 {
		if err := checkUpstreamPort("client", name, served, m.Client); err != nil {
			return nil, err
		}
		out[name] = m.Client
	}
	if name := findServerProc(procs, served, m); name != "" && m.Server > 0 {
		if err := checkUpstreamPort("server", name, served, m.Server); err != nil {
			return nil, err
		}
		out[name] = m.Server
	}
	return out, nil
}

// serviceUpstreams returns the processes that may act as merge upstreams.
// The filter runs first: a non-service candidate is never selected, so it
// gets neither a readiness probe nor a dependency edge.
func serviceUpstreams(procs map[string]Process) map[string]Process {
	out := make(map[string]Process, len(procs))
	for name, proc := range procs {
		if proc.EffectiveMode() == ProcessModeService {
			out[name] = proc
		}
	}
	return out
}

// checkClientProcess refuses a merge.client_process that names a task or
// teardown process. Only a service can be an upstream, so the upstream wiring
// would skip it while the client env injection still targeted it. A name
// that matches no process is left to the auto-detection, as before.
func checkClientProcess(procs map[string]Process, m *Merge) error {
	proc, ok := procs[m.ClientProcess]
	if m.ClientProcess == "" || !ok || proc.EffectiveMode() == ProcessModeService {
		return nil
	}
	return invalid(CodeInvalidMerge, []any{"merge", "client_process"}, nil,
		"merge.client_process names %q, a %s process: only a service can be the merge client", m.ClientProcess, proc.EffectiveMode())
}

// ServedPort is the PORT a process is started with, as far as the config can
// know it before the session assigns the named ports.
type ServedPort struct {
	// Port is the port; 0 when the declared PORT is not a port number.
	Port int
	// Named lists the named ports the session assigns the PORT from, as
	// reference names ("web.http"): the process's own ports.http when it
	// declares no PORT, else each ${ports.*} reference in its PORT. Port is
	// then their preferred port. Named is empty for a PORT written as a
	// number, which the session never assigns.
	Named []string

	declared string // how the PORT is declared, for messages
}

// ServedPorts returns the PORT each process of a loaded config is started
// with, as far as the config can know it — the rule Load matches merge
// upstreams by (see servedPorts). A loaded process's env already holds its
// env_file.
func ServedPorts(procs map[string]Process) map[string]ServedPort {
	return servedPorts(procs, nil)
}

// servedPorts returns the PORT each process is started with, as far as Load
// can know it. The session starts a process with the PORT its env declares —
// the env block over its env_file, with ${ports.*} references resolved — and
// otherwise with the port it assigns to ports.http. A process is left out when
// Load cannot know its PORT: none is declared, the PORT is empty, the
// ports.http port is ephemeral (0), or a reference cannot be resolved before
// the session assigns the ports.
func servedPorts(procs map[string]Process, envFiles map[string]envFile) map[string]ServedPort {
	out := make(map[string]ServedPort, len(procs))
	for name, proc := range procs {
		value, declared := proc.Env["PORT"]
		where := ""
		if !declared {
			value, declared = envFiles[name].vars["PORT"]
			where = " in its env_file"
		}
		if !declared {
			// An out-of-range preferred port is refused by the port check
			// with its own diagnostic.
			if preferred := proc.Ports["http"]; preferred >= 1 && preferred <= 65535 {
				out[name] = ServedPort{Port: preferred, Named: []string{name + ".http"}, declared: fmt.Sprintf("is given PORT %d by ports.http", preferred)}
			}
			continue
		}
		resolved, named, ok := resolvePortReferences(procs, value)
		resolved = strings.TrimSpace(resolved)
		if !ok || resolved == "" {
			continue
		}
		s := ServedPort{Named: named, declared: fmt.Sprintf("declares PORT %q%s", value, where)}
		if resolved != strings.TrimSpace(value) {
			s.declared = fmt.Sprintf("declares PORT %q (%s)%s", value, resolved, where)
		}
		if port, err := strconv.Atoi(resolved); err == nil && port > 0 {
			s.Port = port
		}
		out[name] = s
	}
	return out
}

// resolvePortReferences replaces each ${ports.<process>.<port>} reference in
// value with that port's preferred port — the port the session assigns when
// it is free — and returns the reference names it replaced ("web.http"). ok
// is false when value holds a reference Load cannot resolve: an ephemeral (0)
// or undeclared port, or any other reference namespace.
func resolvePortReferences(procs map[string]Process, value string) (string, []string, bool) {
	ok := true
	var named []string
	resolved := referenceRe.ReplaceAllStringFunc(value, func(ref string) string {
		namespace, process, portName, split := splitReference(referenceRe.FindStringSubmatch(ref)[1])
		if preferred := procs[process].Ports[portName]; split && namespace == "ports" && preferred >= 1 && preferred <= 65535 {
			named = append(named, process+"."+portName)
			return strconv.Itoa(preferred)
		}
		ok = false
		return ref
	})
	return resolved, named, ok
}

// MergeUpstreamPorts returns the named ports a merge upstream of a loaded
// config is started on, as reference names ("web.http"), sorted. The proxy
// command and the readiness probe Load injects target the merge port the
// upstream was matched at, which is the preferred port of each of these: the
// session must assign them exactly that port, never move one.
func MergeUpstreamPorts(cfg *Config) []string {
	if cfg == nil || cfg.Merge == nil {
		return nil
	}
	served := ServedPorts(cfg.Processes)
	// Load refused a config whose upstream is served on another port, so
	// the selection cannot fail here.
	upstreams, _ := resolveUpstreamProcesses(serviceUpstreams(cfg.Processes), served, cfg.Merge)
	var named []string
	for name := range upstreams {
		named = append(named, served[name].Named...)
	}
	slices.Sort(named)
	return slices.Compact(named)
}

// checkUpstreamPort refuses an upstream that is started with a PORT other
// than the merge port it was selected for: the injected readiness probe would
// target a port the process does not serve, and the proxy points at the merge
// port too. A process whose PORT Load cannot know is probed at the merge port
// — the documented guess. The served port is compared numerically, so PORT:
// "05173" is the merge port 5173.
func checkUpstreamPort(field, name string, served map[string]ServedPort, mergePort int) error {
	s, known := served[name]
	if !known || s.Port == mergePort {
		return nil
	}
	return invalid(CodeInvalidMerge, []any{"merge", field}, nil,
		"process %q %s but merge.%s is %d: the proxy and its readiness probe target the merge port",
		name, s.declared, field, mergePort)
}

// routeTarget returns the trimmed target of a "/prefix=target" route spec:
// the text after its last "=". ok is false when the spec has no "=".
func routeTarget(route string) (string, bool) {
	idx := strings.LastIndex(route, "=")
	if idx < 0 {
		return "", false
	}
	return strings.TrimSpace(route[idx+1:]), true
}

// parseRoutePort extracts the port from a "/prefix=PORT" route spec.
// Returns 0 when the spec doesn't carry a port we can resolve.
func parseRoutePort(route string) int {
	target, ok := routeTarget(route)
	if !ok {
		return 0
	}
	port, err := strconv.Atoi(target)
	if err != nil || port <= 0 {
		return 0
	}
	return port
}

// procWithPort returns the first process (excluding merge-port itself)
// served on port. Candidates are visited in name order, so a duplicated PORT
// always resolves to the same process. Skips merge-port to avoid
// self-dependencies and returns "" when no candidate is found.
func procWithPort(procs map[string]Process, served map[string]ServedPort, port int) string {
	if port <= 0 {
		return ""
	}
	for _, name := range SortedProcessNames(procs) {
		if name == MergePortProcessName {
			continue
		}
		if s, known := served[name]; known && s.Port == port {
			return name
		}
	}
	return ""
}

// findServerProc mirrors findClientProc for the backend side. Used to wire
// the merge-port → server dependency edge in simple mode, and to find the
// source tree the api_prefixes auto-detection scans.
func findServerProc(procs map[string]Process, served map[string]ServedPort, m *Merge) string {
	if name := procWithPort(procs, served, m.Server); name != "" {
		return name
	}
	for _, name := range []string{"server", "backend", "api"} {
		if _, ok := procs[name]; ok {
			return name
		}
	}
	return ""
}

// injectTCPReadyIfMissing wires a TCP readiness probe on procs[name] to
// localhost:port. Skips processes that already have a ready_when block —
// the user's probe always wins (e.g. they may want an HTTP /health probe
// instead of a raw TCP accept, which catches cases where the port is
// listening but the app is still warming up).
func injectTCPReadyIfMissing(procs map[string]Process, name string, port int) {
	if name == "" || port == 0 {
		return
	}
	p, ok := procs[name]
	if !ok || p.ReadyWhen != nil {
		return
	}
	p.ReadyWhen = &ReadyWhen{TCP: fmt.Sprintf("localhost:%d", port)}
	procs[name] = p
}

// findClientProc finds the name of the client/frontend process. Candidates
// are visited in name order, so a duplicated PORT always resolves to the same
// process.
func findClientProc(procs map[string]Process, served map[string]ServedPort, m *Merge) string {
	if m.ClientProcess != "" {
		if _, ok := procs[m.ClientProcess]; ok {
			return m.ClientProcess
		}
	}
	if name := procWithPort(procs, served, m.Client); name != "" {
		return name
	}
	for _, name := range []string{"client", "frontend", "web", "ui", "app"} {
		if _, ok := procs[name]; ok {
			return name
		}
	}
	return ""
}

// procDir resolves a process dir relative to configDir.
func procDir(dir, configDir string) string {
	if dir == "" {
		return configDir
	}
	if filepath.IsAbs(dir) {
		return dir
	}
	return filepath.Join(configDir, dir)
}

// ─── .env file parser ─────────────────────────────────────────────────────────

// envFile is one process's parsed env_file, or the error reading it.
type envFile struct {
	vars map[string]string
	err  error
}

// readEnvFiles parses the env_file of every process that declares one.
// Relative paths resolve from the config directory.
func readEnvFiles(procs map[string]Process, configDir string) map[string]envFile {
	out := make(map[string]envFile)
	for name, proc := range procs {
		if proc.EnvFile == "" {
			continue
		}
		path := proc.EnvFile
		if !filepath.IsAbs(path) {
			path = filepath.Join(configDir, path)
		}
		vars, err := parseEnvFile(path)
		out[name] = envFile{vars: vars, err: err}
	}
	return out
}

func parseEnvFile(path string) (map[string]string, error) {
	result := make(map[string]string)
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("cannot read env file %s: %w", path, err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	// A 64 KiB token limit silently truncated long env values (base64
	// secrets, certificates). A line may be up to maxEnvFileLine bytes, and a
	// longer one is a located error instead of a truncated environment. The
	// buffer has room for the longest line plus a "\r\n" terminator, so the
	// limit is the same for every line ending; the length check enforces it.
	scanner.Buffer(make([]byte, 0, 64*1024), maxEnvFileLine+2)
	tooLong := func(lineNo int) error {
		return fmt.Errorf("cannot read env file %s: line %d is longer than 1 MiB (%d bytes)", path, lineNo, maxEnvFileLine)
	}
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		if len(scanner.Bytes()) > maxEnvFileLine {
			return nil, tooLong(lineNo)
		}
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		idx := strings.IndexByte(line, '=')
		if idx < 0 {
			continue
		}
		key := strings.TrimSpace(line[:idx])
		val := strings.TrimSpace(line[idx+1:])
		// Strip surrounding quotes.
		if len(val) >= 2 {
			if (val[0] == '"' && val[len(val)-1] == '"') ||
				(val[0] == '\'' && val[len(val)-1] == '\'') {
				val = val[1 : len(val)-1]
			}
		}
		result[key] = val
	}
	if err := scanner.Err(); err != nil {
		if errors.Is(err, bufio.ErrTooLong) {
			return nil, tooLong(lineNo + 1)
		}
		return nil, fmt.Errorf("cannot read env file %s: %w", path, err)
	}
	return result, nil
}

// checkReferenceCollisions reports two endpoints whose ${ports.*} / ${urls.*}
// reference names would be identical. A reference name is the process name
// and the port name joined by a dot, so a dot in either part lets two
// different endpoints claim one name. The dot-free port-name rule makes this
// unreachable through Load; the check is the safety net that keeps the two
// representations injective if that rule is ever relaxed.
func checkReferenceCollisions(procs map[string]Process) error {
	var collisions []string
	for _, namespace := range []string{"ports", "urls"} {
		seen := make(map[string]string)
		for _, name := range SortedProcessNames(procs) {
			for _, portName := range SortedProcessNames(procs[name].Ports) {
				ref := namespace + "." + name + "." + portName
				endpoint := fmt.Sprintf("process %q port %q", name, portName)
				if other, ok := seen[ref]; ok {
					collisions = append(collisions, fmt.Sprintf("%s is claimed by both %s and %s", ref, other, endpoint))
					continue
				}
				seen[ref] = endpoint
			}
		}
	}
	if len(collisions) > 0 {
		return invalid(CodeInvalidPort, nil, nil, "endpoint reference names collide: %s", strings.Join(collisions, "; "))
	}
	return nil
}
