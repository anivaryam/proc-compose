//go:build !windows

package runner

// shellCommand returns the platform shell and arguments for running a command string.
func shellCommand(command string) (string, []string) {
	return "sh", []string{"-c", command}
}
