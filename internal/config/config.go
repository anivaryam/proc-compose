// Package config handles parsing, validation, and processing of proc-compose YAML configuration files.
package config

import (
	"bufio"
	"bytes"
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

	if cfg.Merge != nil {
		// Auto-detect api_prefixes by scanning server source when none are configured.
		if len(cfg.Merge.ApiPrefixes) == 0 && len(cfg.Merge.Routes) == 0 {
			serverDir := findProcDir(cfg.Processes, cfg.Merge.Server, []string{"server", "backend", "api"}, configDir)
			if serverDir != "" {
				if detected := scanRoutes(serverDir); len(detected) > 0 {
					cfg.Merge.ApiPrefixes = detected
				}
			}
		}

		cmd, err := buildMergeCmd(cfg.Merge)
		if err != nil {
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
		upstreams := resolveUpstreamProcesses(cfg.Processes, cfg.Merge)
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
		cfg.Processes["merge-port"] = mergeProc

		// Auto-inject the merge-port URL into the client process env so users
		// don't need to edit .env or remove hardcoded fallback ports.
		if clientName := findClientProc(cfg.Processes, cfg.Merge); clientName != "" {
			clientDir := procDir(cfg.Processes[clientName].Dir, configDir)
			injectClientAPIEnv(cfg.Processes, clientName, clientDir, cfg.Merge)
		}
	}

	// Validate in name order so the first reported error is deterministic.
	for _, name := range sortedProcessNames(cfg.Processes) {
		proc := cfg.Processes[name]
		at := func(field ...any) []any { return append([]any{"processes", name}, field...) }
		portNames := make([]string, 0, len(proc.Ports))
		for portName := range proc.Ports {
			portNames = append(portNames, portName)
		}
		sort.Strings(portNames)
		for _, portName := range portNames {
			if preferred := proc.Ports[portName]; !processNameRe.MatchString(portName) || preferred < 0 || preferred > 65535 {
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
			envFilePath := proc.EnvFile
			if !filepath.IsAbs(envFilePath) {
				envFilePath = filepath.Join(configDir, envFilePath)
			}
			fileVars, err := parseEnvFile(envFilePath)
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

	// Validate depends_on references after all processes (including merge-port) are registered.
	for _, name := range sortedProcessNames(cfg.Processes) {
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
	seenResources := make(map[string]bool)
	for i, resource := range cfg.Resources {
		if strings.TrimSpace(resource) == "" || len(resource) > 256 || strings.ContainsAny(resource, "\r\n\x00") || seenResources[resource] {
			return nil, invalid(CodeInvalidResource, []any{"resources", i}, nil, "invalid or duplicate resource label %q", resource)
		}
		seenResources[resource] = true
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

func normalizeProcessDirs(processes map[string]Process, configDir string) {
	for name, proc := range processes {
		if proc.Dir == "" || strings.Contains(proc.Dir, "${session.") || strings.Contains(proc.Dir, "${config.") {
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
		for _, r := range m.Routes {
			parts = append(parts, fmt.Sprintf("--route %s", r))
		}
	} else {
		if m.Client == 0 {
			return "", fmt.Errorf("merge.client port is required")
		}
		if m.Server == 0 {
			return "", fmt.Errorf("merge.server port is required")
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
	parts = append(parts, fmt.Sprintf("--port %d", m.Port))

	return strings.Join(parts, " "), nil
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

// findProcDir finds the resolved directory for the process whose PORT env
// matches port, falling back to the first process whose name is in names.
func findProcDir(procs map[string]Process, port int, names []string, configDir string) string {
	portStr := fmt.Sprintf("%d", port)
	for _, proc := range procs {
		if proc.Env["PORT"] == portStr {
			return procDir(proc.Dir, configDir)
		}
	}
	for _, name := range names {
		if proc, ok := procs[name]; ok {
			return procDir(proc.Dir, configDir)
		}
	}
	return ""
}

// resolveUpstreamProcesses returns a map of {process_name → port} for every
// process that merge-port will proxy traffic to. Used by Load() to wire
// readiness probes onto upstreams and depends_on onto merge-port itself.
//
// Simple mode: client + server (looked up by PORT env match, then by name,
// honouring merge.client_process). Route mode: every process whose PORT
// matches a "/prefix=PORT" target.
//
// Processes that already declare ready_when are left alone — the upstream
// dependency edge is still added (the user's probe is what we wait on).
func resolveUpstreamProcesses(procs map[string]Process, m *Merge) map[string]int {
	out := make(map[string]int)
	if len(m.Routes) > 0 {
		for _, route := range m.Routes {
			port := parseRoutePort(route)
			if port == 0 {
				continue
			}
			if name := procWithPort(procs, port); name != "" {
				out[name] = port
			}
		}
		return out
	}
	if name := findClientProc(procs, m); name != "" && m.Client > 0 {
		out[name] = m.Client
	}
	if name := findServerProc(procs, m); name != "" && m.Server > 0 {
		out[name] = m.Server
	}
	return out
}

// parseRoutePort extracts the port from a "/prefix=PORT" route spec.
// Returns 0 when the spec doesn't carry a port we can resolve.
func parseRoutePort(route string) int {
	idx := strings.LastIndex(route, "=")
	if idx < 0 {
		return 0
	}
	port, err := strconv.Atoi(strings.TrimSpace(route[idx+1:]))
	if err != nil || port <= 0 {
		return 0
	}
	return port
}

// procWithPort returns the first process (excluding merge-port itself)
// whose PORT env equals port. Skips merge-port to avoid self-dependencies
// and returns "" when no candidate is found.
func procWithPort(procs map[string]Process, port int) string {
	want := fmt.Sprintf("%d", port)
	for name, proc := range procs {
		if name == "merge-port" {
			continue
		}
		if proc.Env["PORT"] == want {
			return name
		}
	}
	return ""
}

// findServerProc mirrors findClientProc for the backend side. Used to wire
// the merge-port → server dependency edge in simple mode.
func findServerProc(procs map[string]Process, m *Merge) string {
	if name := procWithPort(procs, m.Server); name != "" {
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

// findClientProc finds the name of the client/frontend process.
func findClientProc(procs map[string]Process, m *Merge) string {
	if m.ClientProcess != "" {
		if _, ok := procs[m.ClientProcess]; ok {
			return m.ClientProcess
		}
	}
	portStr := fmt.Sprintf("%d", m.Client)
	for name, proc := range procs {
		if name == "merge-port" {
			continue
		}
		if proc.Env["PORT"] == portStr {
			return name
		}
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

func parseEnvFile(path string) (map[string]string, error) {
	result := make(map[string]string)
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("cannot read env file %s: %w", path, err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
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
	return result, nil
}
