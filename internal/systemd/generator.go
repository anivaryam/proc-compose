// Package systemd generates systemd user unit files for the --survive feature.
package systemd

import (
	"fmt"
	"path/filepath"
	"strings"
)

type UnitOptions struct {
	Name       string
	ExecStart  string
	ConfigFile string
	WorkingDir string
	HomeDir    string
}

// validatePathForUnit rejects characters that systemd unit files cannot
// represent unambiguously inside an ExecStart line. systemd has its own
// quoting rules (see systemd.exec(5)) and embedding spaces, quotes, dollar
// signs, or backticks produces a unit that fails to load or runs the wrong
// command. We refuse rather than silently mangle.
func validatePathForUnit(label, p string) error {
	if p == "" {
		return fmt.Errorf("%s is empty", label)
	}
	if strings.ContainsAny(p, " \t\n\r\"'`$\\") {
		return fmt.Errorf("%s %q contains characters disallowed in a systemd unit (spaces, quotes, $, backslash); rename or move the file", label, p)
	}
	return nil
}

// GenerateUnit returns a rendered user-unit file for opts. It returns an
// error when any path field contains characters that would break the unit.
//
// The unit pins PATH to "<HomeDir>/.local/bin:/usr/local/bin:/usr/bin" so
// processes managed by proc-compose can find binaries that brokit and
// install.sh drop into ~/.local/bin (proc-compose, tunnel, merge-port,
// etc.). systemd user services otherwise inherit a minimal PATH from
// systemd-user.service that excludes ~/.local/bin and the merge-port /
// tunnel processes silently fail to launch. systemd does NOT expand $HOME
// inside Environment= values, so we substitute the absolute path here.
func GenerateUnit(opts UnitOptions) (string, error) {
	if err := validatePathForUnit("ExecStart", opts.ExecStart); err != nil {
		return "", err
	}
	if err := validatePathForUnit("ConfigFile", opts.ConfigFile); err != nil {
		return "", err
	}
	if err := validatePathForUnit("WorkingDir", opts.WorkingDir); err != nil {
		return "", err
	}
	if err := validatePathForUnit("HomeDir", opts.HomeDir); err != nil {
		return "", err
	}
	execStart := fmt.Sprintf("%s up --silent --file %s", opts.ExecStart, opts.ConfigFile)
	pathEnv := fmt.Sprintf("%s/.local/bin:/usr/local/bin:/usr/bin", opts.HomeDir)
	return fmt.Sprintf(`[Unit]
Description=proc-compose: %s

[Service]
Type=forking
ExecStart=%s
Restart=on-failure
WorkingDirectory=%s
Environment=HOME=%s
Environment=PATH=%s

[Install]
WantedBy=default.target
`, opts.Name, execStart, opts.WorkingDir, opts.HomeDir, pathEnv), nil
}

func UnitPath(name string) string {
	return filepath.Join(".config", "systemd", "user", fmt.Sprintf("proc-compose-%s.service", name))
}