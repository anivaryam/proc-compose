package runner

import (
	"os/exec"
)

type ProcessGroup interface {
	Setup(cmd *exec.Cmd)
	Track(cmd *exec.Cmd) error
	Kill() error
	Close() error
}
