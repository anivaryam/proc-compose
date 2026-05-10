//go:build !windows

package runner

import (
	"os/exec"
	"syscall"
	"time"
)

type unixProcessGroup struct{}

func newProcessGroup() ProcessGroup {
	return &unixProcessGroup{}
}

func (pg *unixProcessGroup) Setup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process != nil {
			return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
		}
		return nil
	}
	cmd.WaitDelay = 5 * time.Second
}

func (pg *unixProcessGroup) Track(cmd *exec.Cmd) error {
	return nil
}

func (pg *unixProcessGroup) Kill() error {
	return nil
}

func (pg *unixProcessGroup) Close() error {
	return nil
}
