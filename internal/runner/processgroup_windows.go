//go:build windows

package runner

import (
	"os/exec"
	"time"

	"github.com/kolesnikovae/go-winjob"
)

type windowsProcessGroup struct {
	job       *winjob.JobObject
	waitDelay time.Duration
}

func newProcessGroup() ProcessGroup {
	j, err := winjob.Create("", winjob.WithKillOnJobClose())
	if err != nil {
		return &windowsProcessGroup{job: nil, waitDelay: 5 * time.Second}
	}
	return &windowsProcessGroup{job: j, waitDelay: 5 * time.Second}
}

func (pg *windowsProcessGroup) Setup(cmd *exec.Cmd) {
	// cmd.Cancel must be set before cmd.Start; assigning it later is a silent
	// no-op and falls back to os.Process.Kill, bypassing the Job Object.
	cmd.Cancel = func() error {
		return pg.Kill()
	}
	cmd.WaitDelay = pg.waitDelay
}

func (pg *windowsProcessGroup) Track(cmd *exec.Cmd) error {
	if pg.job == nil {
		return nil
	}
	return pg.job.Assign(cmd.Process)
}

func (pg *windowsProcessGroup) Kill() error {
	if pg.job == nil {
		return nil
	}
	return pg.job.Terminate()
}

func (pg *windowsProcessGroup) Close() error {
	if pg.job == nil {
		return nil
	}
	return pg.job.Close()
}
