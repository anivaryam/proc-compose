package doctor

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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
	for name, proc := range cfg.Processes {
		if proc.Dir != "" {
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

	ports := map[int]string{}
	for name, proc := range cfg.Processes {
		port := processPort(proc)
		if port == 0 {
			continue
		}
		if other, ok := ports[port]; ok {
			report.Findings = append(report.Findings, Finding{
				Severity:   SeverityWarning,
				Code:       codeDuplicatePort,
				Message:    fmt.Sprintf("processes %q and %q both use port %d", other, name, port),
				Suggestion: "assign each process a unique PORT",
			})
			continue
		}
		ports[port] = name
	}

	for name, proc := range cfg.Processes {
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
	var cfg config.Config
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
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
	for name, proc := range cfg.Processes {
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

func processPort(proc config.Process) int {
	if proc.Env != nil {
		if value := proc.Env["PORT"]; value != "" {
			port, err := strconv.Atoi(strings.Trim(value, "\"'"))
			if err == nil {
				return port
			}
		}
	}
	if proc.ReadyWhen != nil && proc.ReadyWhen.TCP != "" {
		parts := strings.Split(proc.ReadyWhen.TCP, ":")
		port, err := strconv.Atoi(parts[len(parts)-1])
		if err == nil {
			return port
		}
	}
	return 0
}
