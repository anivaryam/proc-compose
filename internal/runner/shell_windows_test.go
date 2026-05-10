//go:build windows

package runner

import "testing"

func TestShellTranslation(t *testing.T) {
	tests := []struct {
		name      string
		cmd       string
		wantShell string
		wantArgs  string
	}{
		{"simple var", "echo $PORT", "cmd", "echo %PORT%"},
		{"braced var", "echo ${DB_HOST}", "cmd", "echo %DB_HOST%"},
		{"path", "go run ./cmd/server", "cmd", "go run ./cmd/server"},
		{"piped", "cat a | grep b", "powershell", "cat a | grep b"},
		{"and", "npm run dev && npm run build", "powershell", "npm run dev && npm run build"},
		{"redirect", "echo hi > out.txt", "powershell", "echo hi > out.txt"},
		{"or", "cmd1 || cmd2", "powershell", "cmd1 || cmd2"},
		{"multiple pipes", "a | b | c", "powershell", "a | b | c"},
		{"env var in path", "$HOME/bin/app", "cmd", "%HOME%/bin/app"},
		{"cmd substitution", "echo $(date)", "cmd", "echo %(date)%"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			shell, args := shellCommand(tt.cmd)
			gotArgs := args[len(args)-1]
			if shell != tt.wantShell || gotArgs != tt.wantArgs {
				t.Errorf("shellCommand(%q) = %q, %v; want %q, %v", tt.cmd, shell, args, tt.wantShell, tt.wantArgs)
			}
		})
	}
}

func TestNeedsPowerShell(t *testing.T) {
	tests := []struct {
		cmd  string
		want bool
	}{
		{"echo hello", false},
		{"echo $PORT", false},
		{"echo ${VAR}", false},
		{"cat a | grep b", true},
		{"cmd && other", true},
		{"cmd || other", true},
		{"echo hi > out.txt", true},
		{"echo hi 2>&1", true},
		{"$(echo $(date))", false},
		{"$(cmd)", false},
	}

	for _, tt := range tests {
		t.Run(tt.cmd, func(t *testing.T) {
			got := needsPowerShell(tt.cmd)
			if got != tt.want {
				t.Errorf("needsPowerShell(%q) = %v; want %v", tt.cmd, got, tt.want)
			}
		})
	}
}

func TestTranslateToCMD(t *testing.T) {
	tests := []struct {
		cmd  string
		want string
	}{
		{"echo $PORT", "echo %PORT%"},
		{"echo ${DB_HOST}", "echo %DB_HOST%"},
		{"go run ./cmd/server", "go run ./cmd/server"},
		{"$HOME/bin/app", "%HOME%/bin/app"},
		{"echo $(date)", "echo %(date)%"},
		{"echo $USER: $PATH", "echo %USER%: %PATH%"},
	}

	for _, tt := range tests {
		t.Run(tt.cmd, func(t *testing.T) {
			got := translateToCMD(tt.cmd)
			if got != tt.want {
				t.Errorf("translateToCMD(%q) = %q; want %q", tt.cmd, got, tt.want)
			}
		})
	}
}
