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
	Root string
}

func (v commandVerifier) Verify(configPath string) (*Verification, error) {
	start := time.Now()
	report := &Verification{ConfigPath: configPath}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	logFile, err := os.CreateTemp("", "proc-compose-bootstrap-*.log")
	if err != nil {
		return report, err
	}
	logPath := logFile.Name()
	_ = logFile.Close()
	report.LogFile = logPath

	cmd := exec.CommandContext(ctx, os.Args[0], "up", "--dry-run", "--log-file", logPath, "-f", configPath)
	if v.Root != "" {
		cmd.Dir = v.Root
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err = cmd.Run()
	report.ElapsedMS = time.Since(start).Milliseconds()
	report.Started = err == nil
	report.Ready = err == nil
	report.Stopped = true
	if err != nil {
		report.Message = stderr.String()
		return report, fmt.Errorf("verification failed: %w", err)
	}
	report.Message = "dry-run verification passed"
	return report, nil
}
