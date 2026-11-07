package bootstrap

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"time"
)

type commandVerifier struct {
	Root       string
	Executable string
}

func (v commandVerifier) Verify(configPath string) (*Verification, error) {
	start := time.Now()
	report := &Verification{ConfigPath: configPath, ValidationStatus: "failed"}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	logFile, err := os.CreateTemp("", "proc-compose-bootstrap-*.log")
	if err != nil {
		return report, err
	}
	logPath := logFile.Name()
	_ = logFile.Close()
	report.LogFile = logPath
	// The log file is empty unless the child wrote to it. An empty file holds
	// no diagnostics — the failure diagnostics live in report.Message — so it
	// is removed instead of left behind in the shared temp dir.
	defer func() {
		if removeIfEmpty(logPath) {
			report.LogFile = ""
		}
	}()

	executable := v.Executable
	if executable == "" {
		executable = os.Args[0]
	}
	cmd := exec.CommandContext(ctx, executable, "up", "--dry-run", "--log-file", logPath, "-f", configPath)
	if v.Root != "" {
		cmd.Dir = v.Root
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err = cmd.Run()
	report.ElapsedMS = time.Since(start).Milliseconds()
	if err != nil {
		report.ValidationStatus = "failed"
		report.Message = stderr.String()
		return report, fmt.Errorf("verification failed: %w", err)
	}
	report.ValidationStatus = "passed"
	report.Message = "configuration validation passed (dry run; no application processes started)"
	return report, nil
}

// removeIfEmpty removes path when it exists and holds no bytes. It reports
// whether the file was removed.
func removeIfEmpty(path string) bool {
	info, err := os.Stat(path)
	if err != nil || info.Size() != 0 {
		return false
	}
	if err := os.Remove(path); err != nil {
		return false
	}
	return true
}
