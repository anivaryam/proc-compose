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
		"ExecStart=/usr/local/bin/proc-compose up --silent --file /home/user/project/proc-compose.yml",
		"WorkingDirectory=/home/user/project",
		"Environment=HOME=/home/user",
		"Environment=PATH=/home/user/.local/bin:/usr/local/bin:/usr/bin",
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
		{"space in config", UnitOptions{
			Name:       "x",
			ExecStart:  "/bin/proc-compose",
			ConfigFile: "/home/user/with space/proc-compose.yml",
			WorkingDir: "/home/user",
			HomeDir:    "/home/user",
		}},
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
