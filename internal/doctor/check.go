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
)

var mergePortInPath = func() bool {
	_, err := exec.LookPath("merge-port")
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
		for _, dep := range proc.DependsOn {
			depProc, ok := cfg.Processes[dep]
			if ok && depProc.ReadyWhen == nil {
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
			Message:    "merge is configured but merge-port is not on PATH",
			Suggestion: "install merge-port or remove merge config",
		})
	}

	_ = root
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
