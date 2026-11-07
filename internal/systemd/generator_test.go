package systemd

import (
	"strings"
	"testing"
)

func TestGenerateUnit(t *testing.T) {
	unit, err := GenerateUnit(UnitOptions{
		Name:       "myapp",
		ExecStart:  "/usr/local/bin/proc-compose",
		ConfigFile: "/home/user/project/proc-compose.yml",
		WorkingDir: "/home/user/project",
		HomeDir:    "/home/user",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	cases := []string{
		"Description=proc-compose: myapp",
		"Type=forking",
		"Restart=on-failure",
		"WantedBy=default.target",
		"ExecStart=\"/usr/local/bin/proc-compose\" up --silent --file \"/home/user/project/proc-compose.yml\"",
		"WorkingDirectory=/home/user/project",
		"Environment=\"HOME=/home/user\"",
		"Environment=\"PATH=/home/user/.local/bin:/usr/local/bin:/usr/bin\"",
	}
	for _, want := range cases {
		if !strings.Contains(unit, want) {
			t.Errorf("missing %q in unit:\n%s", want, unit)
		}
	}
}

func TestGenerateUnitRejectsBadPaths(t *testing.T) {
	cases := []struct {
		name string
		opts UnitOptions
	}{
		{"quote in execstart", UnitOptions{
			Name:       "x",
			ExecStart:  "/bin/\"evil",
			ConfigFile: "/cfg.yml",
			WorkingDir: "/home/user",
			HomeDir:    "/home/user",
		}},
		{"dollar in workdir", UnitOptions{
			Name:       "x",
			ExecStart:  "/bin/proc-compose",
			ConfigFile: "/cfg.yml",
			WorkingDir: "/home/$USER",
			HomeDir:    "/home/user",
		}},
		{"newline in config", UnitOptions{
			Name:       "x",
			ExecStart:  "/bin/proc-compose",
			ConfigFile: "/cfg\n.yml",
			WorkingDir: "/home/user",
			HomeDir:    "/home/user",
		}},
		{"carriage return in config", UnitOptions{
			Name:       "x",
			ExecStart:  "/bin/proc-compose",
			ConfigFile: "/cfg\r.yml",
			WorkingDir: "/home/user",
			HomeDir:    "/home/user",
		}},
		{"nul in config", UnitOptions{
			Name:       "x",
			ExecStart:  "/bin/proc-compose",
			ConfigFile: "/cfg\x00.yml",
			WorkingDir: "/home/user",
			HomeDir:    "/home/user",
		}},
		{"single quote in config", UnitOptions{
			Name:       "x",
			ExecStart:  "/bin/proc-compose",
			ConfigFile: "/cfg'.yml",
			WorkingDir: "/home/user",
			HomeDir:    "/home/user",
		}},
		{"backslash in config", UnitOptions{
			Name:       "x",
			ExecStart:  "/bin/proc-compose",
			ConfigFile: "/cfg\\.yml",
			WorkingDir: "/home/user",
			HomeDir:    "/home/user",
		}},
		{"empty home", UnitOptions{
			Name:       "x",
			ExecStart:  "/bin/proc-compose",
			ConfigFile: "/cfg.yml",
			WorkingDir: "/home/user",
			HomeDir:    "",
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := GenerateUnit(c.opts); err == nil {
				t.Fatalf("expected error for %s", c.name)
			}
		})
	}
}

// A path with a space is legal and systemd supports it when the value is
// quoted, so it must be accepted (issue #35).
func TestGenerateUnitAcceptsSpaceAndTabPaths(t *testing.T) {
	unit, err := GenerateUnit(UnitOptions{
		Name:       "myapp",
		ExecStart:  "/opt/my apps/proc-compose",
		ConfigFile: "/home/user/my project/proc-compose.yml",
		WorkingDir: "/home/user/my project",
		HomeDir:    "/home/user",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	cases := []string{
		"ExecStart=\"/opt/my apps/proc-compose\" up --silent --file \"/home/user/my project/proc-compose.yml\"",
		// WorkingDirectory= is not quote-stripped by systemd, so it stays
		// unquoted.
		"WorkingDirectory=/home/user/my project",
		"Environment=\"HOME=/home/user\"",
	}
	for _, want := range cases {
		if !strings.Contains(unit, want) {
			t.Errorf("missing %q in unit:\n%s", want, unit)
		}
	}

	tabbed, err := GenerateUnit(UnitOptions{
		Name:       "myapp",
		ExecStart:  "/opt/my\tapps/proc-compose",
		ConfigFile: "/home/user/proc-compose.yml",
		WorkingDir: "/home/user",
		HomeDir:    "/home/user",
	})
	if err != nil {
		t.Fatalf("unexpected error for a tab path: %v", err)
	}
	if !strings.Contains(tabbed, "ExecStart=\"/opt/my\tapps/proc-compose\"") {
		t.Errorf("tab path was not quoted:\n%s", tabbed)
	}
}

// '%' is specifier-expanded in every setting value, so it is escaped as
// '%%' (issue #34).
func TestGenerateUnitEscapesSpecifiers(t *testing.T) {
	unit, err := GenerateUnit(UnitOptions{
		Name:       "pct%20name",
		ExecStart:  "/tmp/pct%20dir/proc-compose",
		ConfigFile: "/tmp/pct%20dir/proc-compose.yml",
		WorkingDir: "/tmp/pct%20dir",
		HomeDir:    "/home/user%20x",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	cases := []string{
		"Description=proc-compose: pct%%20name",
		"ExecStart=\"/tmp/pct%%20dir/proc-compose\" up --silent --file \"/tmp/pct%%20dir/proc-compose.yml\"",
		"WorkingDirectory=/tmp/pct%%20dir",
		"Environment=\"HOME=/home/user%%20x\"",
		"Environment=\"PATH=/home/user%%20x/.local/bin:/usr/local/bin:/usr/bin\"",
	}
	for _, want := range cases {
		if !strings.Contains(unit, want) {
			t.Errorf("missing %q in unit:\n%s", want, unit)
		}
	}
	if strings.Contains(unit, "pct%20dir/proc-compose\"") {
		t.Errorf("a single '%%' was left unescaped:\n%s", unit)
	}
}

// Escaping is applied once per '%': an input with two percent signs renders
// as four, which systemd resolves back to two.
func TestGenerateUnitEscapesEachPercentOnce(t *testing.T) {
	unit, err := GenerateUnit(UnitOptions{
		Name:       "x",
		ExecStart:  "/bin/proc-compose",
		ConfigFile: "/cfg%%name.yml",
		WorkingDir: "/home/user",
		HomeDir:    "/home/user",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(unit, "--file \"/cfg%%%%name.yml\"") {
		t.Errorf("expected four percent signs in the rendered unit:\n%s", unit)
	}
}

// Environment= values are whole assignments: systemd splits an unquoted
// value at the first space and drops the remainder (issue #35).
func TestGenerateUnitQuotesEnvironmentAssignments(t *testing.T) {
	unit, err := GenerateUnit(UnitOptions{
		Name:       "x",
		ExecStart:  "/bin/proc-compose",
		ConfigFile: "/cfg.yml",
		WorkingDir: "/home/user",
		HomeDir:    "/home/user with space",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(unit, "Environment=\"HOME=/home/user with space\"") {
		t.Errorf("HOME assignment was not quoted as a whole:\n%s", unit)
	}
	if !strings.Contains(unit, "Environment=\"PATH=/home/user with space/.local/bin:/usr/local/bin:/usr/bin\"") {
		t.Errorf("PATH assignment was not quoted as a whole:\n%s", unit)
	}
}
