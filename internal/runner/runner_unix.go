//go:build !windows

package runner

import (
	"context"
	"os/exec"
)

// shellExec returns the command that runs command through the platform shell.
func shellExec(ctx context.Context, command string) *exec.Cmd {
	shell, args := shellCommand(command)
	return exec.CommandContext(ctx, shell, args...)
}

// shellCommand returns the platform shell and arguments for running a command string.
func shellCommand(command string) (string, []string) {
	return "sh", []string{"-c", command}
}
