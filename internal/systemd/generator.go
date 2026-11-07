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
// represent exactly. systemd expands '%' specifiers in every setting value,
// expands '$VAR' and '${VAR}' in command lines, and strips quotes only in
// some settings (WorkingDirectory= keeps them), so a path carrying those
// characters would be silently rewritten rather than run as the user wrote
// it. We refuse rather than silently mangle.
//
// The refused set is: newline, carriage return, NUL, double quote, single
// quote, backslash and '$'. Space and tab are allowed — they are quoted
// where systemd honours quotes (ExecStart=, Environment=). '%' is allowed
// too: it is escaped as '%%' at render time.
func validatePathForUnit(label, p string) error {
	if p == "" {
		return fmt.Errorf("%s is empty", label)
	}
	if strings.ContainsAny(p, "\n\r\x00\"'\\$") {
		return fmt.Errorf("%s %q contains characters disallowed in a systemd unit (newline, carriage return, NUL, quotes, backslash, $); rename or move the file", label, p)
	}
	return nil
}

// escapeSpecifiers doubles '%' so systemd emits a literal '%'. systemd
// expands specifiers in every setting value (systemd.unit(5)); a lone '%'
// that is not a valid specifier makes the unit fail to load ("Invalid
// slot"), and a valid-looking one silently rewrites the path.
func escapeSpecifiers(s string) string {
	return strings.ReplaceAll(s, "%", "%%")
}

// quoteUnitArg wraps s in double quotes, escaping '\' and '"' as systemd's
// C-style rules require (systemd.syntax(7)). An unquoted command line is
// split on whitespace, so a path with a space would become two arguments.
func quoteUnitArg(s string) string {
	replacer := strings.NewReplacer(`\`, `\\`, `"`, `\"`)
	return `"` + replacer.Replace(s) + `"`
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
//
// Rendering rules, one per systemd behaviour:
//   - '%' is specifier-expanded in every setting, so it is escaped as '%%'.
//   - ExecStart= and Environment= honour quotes, so the executable, the
//     --file argument and each whole environment assignment are quoted.
//   - WorkingDirectory= does not strip quotes, so it stays unquoted.
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
	execStart := fmt.Sprintf("%s up --silent --file %s",
		quoteUnitArg(escapeSpecifiers(opts.ExecStart)),
		quoteUnitArg(escapeSpecifiers(opts.ConfigFile)))
	pathEnv := fmt.Sprintf("%s/.local/bin:/usr/local/bin:/usr/bin", escapeSpecifiers(opts.HomeDir))
	return fmt.Sprintf(`[Unit]
Description=proc-compose: %s

[Service]
Type=forking
ExecStart=%s
Restart=on-failure
WorkingDirectory=%s
Environment="HOME=%s"
Environment="PATH=%s"

[Install]
WantedBy=default.target
`, escapeSpecifiers(opts.Name), execStart, escapeSpecifiers(opts.WorkingDir), escapeSpecifiers(opts.HomeDir), pathEnv), nil
}

func UnitPath(name string) string {
	return filepath.Join(".config", "systemd", "user", fmt.Sprintf("proc-compose-%s.service", name))
}
