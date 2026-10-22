package doctor

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/anivaryam/proc-compose/internal/config"
	"gopkg.in/yaml.v3"
)

const (
	codeInvalidConfig              = "invalid_config"
	codeMissingDir                 = "missing_dir"
	codeMissingEnvFile             = "missing_env_file"
	codeDuplicatePort              = "duplicate_port"
	codeDependencyWithoutReadiness = "dependency_without_readiness"
	codeMissingMergePort           = "missing_merge_port"
	codeUncappedAlwaysRestart      = "uncapped_always_restart"
	codeMissingTunnel              = "missing_tunnel"
)

var mergePortInPath = func() bool {
	_, err := exec.LookPath("merge-port")
	return err == nil
}

var tunnelInPath = func() bool {
	_, err := exec.LookPath("tunnel")
	return err == nil
}

func checkExistingConfig(report *Report, root string) {
	precheckEnvFiles(report)

	cfg, shapeErr := loadConfigShape(report.ConfigPath)
	loadCfg, loadErr := config.Load(report.ConfigPath)
	if loadErr != nil {
		report.Findings = append(report.Findings, Finding{
			Severity:   SeverityError,
			Code:       codeInvalidConfig,
			Message:    "config does not parse cleanly",
			Detail:     loadErr.Error(),
			Suggestion: "fix validation errors, then run doctor again",
		})
	}
	if shapeErr != nil {
		return
	}
	if loadCfg != nil {
		cfg = loadCfg
	}

	configDir := filepath.Dir(report.ConfigPath)
	for _, name := range config.SortedProcessNames(cfg.Processes) {
		proc := cfg.Processes[name]
		// A dir that still holds a runtime template is expanded by the
		// session at start-up, so it cannot be resolved statically here.
		if proc.Dir != "" && !config.HasRuntimeTemplate(proc.Dir) {
			path := resolveConfigPath(configDir, proc.Dir)
			if _, err := os.Stat(path); err != nil {
				report.Findings = append(report.Findings, Finding{
					Severity:   SeverityError,
					Code:       codeMissingDir,
					Message:    fmt.Sprintf("process %q dir does not exist", name),
					Detail:     proc.Dir,
					Suggestion: "create the directory or update dir",
				})
			}
		}

		if proc.EnvFile != "" {
			path := resolveConfigPath(configDir, proc.EnvFile)
			if _, err := os.Stat(path); err != nil && !hasFindingWithDetail(report, codeMissingEnvFile, proc.EnvFile) {
				report.Findings = append(report.Findings, Finding{
					Severity:   SeverityError,
					Code:       codeMissingEnvFile,
					Message:    fmt.Sprintf("process %q env_file does not exist", name),
					Detail:     proc.EnvFile,
					Suggestion: "create the env file or update env_file",
				})
			}
		}
	}

	type portUse struct {
		name  string
		named []string
	}
	ports := map[int]portUse{}
	served := config.ServedPorts(cfg.Processes)
	for _, name := range config.SortedProcessNames(cfg.Processes) {
		proc := cfg.Processes[name]
		port, named := processPort(served, name, proc)
		// A teardown process runs after the stack stopped; a PORT in its env
		// names a service to reach, not a port it binds.
		if port == 0 || proc.Mode == config.ProcessModeTeardown {
			continue
		}
		if other, ok := ports[port]; ok {
			// Two different named ports that only prefer the same port do
			// not collide: the session moves one of them.
			if len(other.named) > 0 && len(named) > 0 && !slices.ContainsFunc(named, func(n string) bool { return slices.Contains(other.named, n) }) {
				continue
			}
			report.Findings = append(report.Findings, Finding{
				Severity:   SeverityWarning,
				Code:       codeDuplicatePort,
				Message:    fmt.Sprintf("processes %q and %q both use port %d", other.name, name, port),
				Suggestion: "assign each process a unique PORT",
			})
			continue
		}
		ports[port] = portUse{name, named}
	}

	for _, name := range config.SortedProcessNames(cfg.Processes) {
		proc := cfg.Processes[name]
		if proc.Restart == "always" && proc.MaxRestarts == 0 {
			report.Findings = append(report.Findings, Finding{
				Severity:   SeverityInfo,
				Code:       codeUncappedAlwaysRestart,
				Message:    fmt.Sprintf("process %q uses restart: always without max_restarts", name),
				Suggestion: "set max_restarts if repeated clean exits should stop instead of loop forever",
			})
		}
		for _, dep := range proc.DependsOn {
			depProc, ok := cfg.Processes[dep]
			if ok && depProc.ReadyWhen == nil && depProc.EffectiveMode() != config.ProcessModeTask {
				report.Findings = append(report.Findings, Finding{
					Severity:   SeverityInfo,
					Code:       codeDependencyWithoutReadiness,
					Message:    fmt.Sprintf("process %q depends on %q, but dependency has no ready_when", name, dep),
					Suggestion: "add ready_when to the dependency for safer startup ordering",
				})
			}
		}
	}

	if cfg.Merge != nil && !mergePortInPath() {
		report.Findings = append(report.Findings, Finding{
			Severity:   SeverityWarning,
			Code:       codeMissingMergePort,
			Message:    "merge: section requires merge-port but it is not on PATH",
			Suggestion: "run: brokit install merge-port  — or remove the merge: section",
		})
	}

	if usesTunnel(cfg) && !tunnelInPath() {
		report.Findings = append(report.Findings, Finding{
			Severity:   SeverityWarning,
			Code:       codeMissingTunnel,
			Message:    "a tunnel process is configured but tunnel is not on PATH",
			Suggestion: "install tunnel or update the process command",
		})
	}

	_ = root
}

// usesTunnel reports whether a config likely depends on the optional tunnel
// binary. It checks process names and shell commands, treating exact or
// whitespace-delimited `tunnel` command usage as a match while leaving unrelated
// words alone.
func usesTunnel(cfg *config.Config) bool {
	for name, proc := range cfg.Processes {
		cmd := strings.TrimSpace(proc.Cmd)
		if name == "tunnel" || strings.HasPrefix(name, "tunnel-") || strings.HasSuffix(name, "-tunnel") {
			return true
		}
		if cmd == "tunnel" || strings.HasPrefix(cmd, "tunnel ") || strings.Contains(cmd, " tunnel ") {
			return true
		}
	}
	return false
}

func loadConfigShape(path string) (*config.Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return parseConfigShape(data)
}

// parseConfigShape decodes a proc-compose config document. It is the single
// parser for both the config on disk and the config the doctor suggests, so
// the two are judged by the same rules.
func parseConfigShape(data []byte) (*config.Config, error) {
	var cfg config.Config
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// checkSuggestedConfig reports requirements the doctor's own suggested config
// cannot satisfy. The suggestion is what the user is told to run, so a
// missing binary in it is worth reporting before the user tries.
//
// The finding carries the "suggested config" detail so it can be told apart
// from the finding about the config that exists on disk. It is produced once
// per report, so at most one such finding exists.
func checkSuggestedConfig(report *Report) {
	if strings.TrimSpace(report.SuggestedYAML) == "" {
		return
	}
	cfg, err := parseConfigShape([]byte(report.SuggestedYAML))
	if err != nil {
		// A suggestion that does not parse is not this check's business; the
		// existing-config checks already report config problems.
		return
	}
	if cfg.Merge == nil || mergePortInPath() {
		return
	}
	report.Findings = append(report.Findings, Finding{
		Severity:   SeverityWarning,
		Code:       codeMissingMergePort,
		Message:    "the suggested config's merge: section requires merge-port but it is not on PATH",
		Detail:     "suggested config",
		Suggestion: "run: brokit install merge-port  — or remove the merge: section",
	})
}

func precheckEnvFiles(report *Report) {
	data, err := os.ReadFile(report.ConfigPath)
	if err != nil {
		return
	}
	var cfg struct {
		Processes map[string]struct {
			EnvFile string `yaml:"env_file"`
		} `yaml:"processes"`
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(&cfg); err != nil {
		return
	}
	configDir := filepath.Dir(report.ConfigPath)
	for _, name := range config.SortedProcessNames(cfg.Processes) {
		proc := cfg.Processes[name]
		if proc.EnvFile == "" {
			continue
		}
		path := resolveConfigPath(configDir, proc.EnvFile)
		if _, err := os.Stat(path); err != nil {
			report.Findings = append(report.Findings, Finding{
				Severity:   SeverityError,
				Code:       codeMissingEnvFile,
				Message:    fmt.Sprintf("process %q env_file does not exist", name),
				Detail:     proc.EnvFile,
				Suggestion: "create the env file or update env_file",
			})
		}
	}
}

func hasFindingWithDetail(report *Report, code, detail string) bool {
	for _, finding := range report.Findings {
		if finding.Code == code && finding.Detail == detail {
			return true
		}
	}
	return false
}

func resolveConfigPath(configDir, path string) string {
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(configDir, path)
}

// processPort returns the port process name uses, for the duplicate-port
// check, and the named ports the session assigns it from (none for a port the
// config fixes). It is the PORT the process is started with, read the way Load
// matches merge upstreams (config.ServedPorts: env.PORT, else the env_file
// PORT, else ports.http, a ${ports.*} reference resolved to its preferred
// port), else the port its ready_when tcp probe targets.
func processPort(served map[string]config.ServedPort, name string, proc config.Process) (int, []string) {
	if s := served[name]; s.Port > 0 {
		return s.Port, s.Named
	}
	if proc.ReadyWhen != nil && proc.ReadyWhen.TCP != "" {
		parts := strings.Split(proc.ReadyWhen.TCP, ":")
		port, err := strconv.Atoi(parts[len(parts)-1])
		if err == nil {
			return port, nil
		}
	}
	return 0, nil
}
