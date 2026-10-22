//go:build windows

package runner

import "testing"

func TestShellTranslation(t *testing.T) {
	tests := []struct {
		name      string
		cmd       string
		wantShell string
		wantArgs  []string
	}{
		{"plain command uses cmd", "echo hello", "cmd", []string{"/c", "echo hello"}},
		{"cmd env syntax is native", "echo %PORT%", "cmd", []string{"/c", "echo %PORT%"}},
		{"posix env remains best effort compatibility", "echo $PORT", "cmd", []string{"/c", "echo %PORT%"}},
		{"braced posix env remains best effort compatibility", "echo ${DB_HOST}", "cmd", []string{"/c", "echo %DB_HOST%"}},
		{"path text is not normalized", "go run ./cmd/server", "cmd", []string{"/c", "go run ./cmd/server"}},
		{"explicit powershell remains user command text", `powershell -NoProfile -Command "Write-Output $env:PORT"`, "cmd", []string{"/c", `powershell -NoProfile -Command "Write-Output $env:PORT"`}},
		{"piped command uses existing powershell path", "cat a | grep b", "powershell", []string{"-Command", "cat a | grep b"}},
		{"and command uses the cmd path", "npm run dev && npm run build", "cmd", []string{"/c", "npm run dev && npm run build"}},
		{"redirect command uses existing powershell path", "echo hi > out.txt", "powershell", []string{"-Command", "echo hi > out.txt"}},
		{"or command uses the cmd path", "cmd1 || cmd2", "cmd", []string{"/c", "cmd1 || cmd2"}},
		{"multiple pipes use existing powershell path", "a | b | c", "powershell", []string{"-Command", "a | b | c"}},
		{"mixed and and pipe uses the cmd path", "a && b | c", "cmd", []string{"/c", "a && b | c"}},
		{"mixed pipe and and uses the cmd path", "a | b && c", "cmd", []string{"/c", "a | b && c"}},
		{"env var in path is best effort compatibility", "$HOME/bin/app", "cmd", []string{"/c", "%HOME%/bin/app"}},
		{"cmd substitution is best effort compatibility", "echo $(date)", "cmd", []string{"/c", "echo %(date)%"}},
		{"sh -c keeps a single quoted inner command", `sh -c "echo 'x'"`, "cmd", []string{"/c", "echo 'x'"}},
		{"sh -c keeps a double quoted inner command", `sh -c 'echo "x"'`, "cmd", []string{"/c", `echo "x"`}},
		{"sh -c keeps escaped quotes verbatim", `sh -c "echo \"hello\""`, "cmd", []string{"/c", `echo \"hello\"`}},
		{"sh -c without inner quotes is unchanged", "sh -c echo hello", "cmd", []string{"/c", "echo hello"}},
		{"sh -c with a quoted inner command drops only that pair", `sh -c "echo hello"`, "cmd", []string{"/c", "echo hello"}},
		{"sh -c with mismatched outer quotes is untouched", `sh -c "echo 'x'`, "cmd", []string{"/c", `"echo 'x'`}},
		{"sh -c with a single quote byte is untouched", `sh -c "`, "cmd", []string{"/c", `"`}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			shell, args := shellCommand(tt.cmd)
			if shell != tt.wantShell || !equalStringSlices(args, tt.wantArgs) {
				t.Errorf("shellCommand(%q) = %q, %v; want %q, %v", tt.cmd, shell, args, tt.wantShell, tt.wantArgs)
			}
		})
	}
}

func equalStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestNeedsPowerShell(t *testing.T) {
	tests := []struct {
		cmd  string
		want bool
	}{
		{"echo hello", false},
		{"echo $PORT", false},
		{"echo ${VAR}", false},
		{`powershell -NoProfile -Command "Write-Output $env:PORT"`, false},
		{"cat a | grep b", true},
		{"cmd && other", false},
		{"cmd || other", false},
		{"a && b | c", false},
		{"a | b && c", false},
		{"a || b | c", false},
		{"echo hi > out.txt", true},
		{"echo hi 2>&1", true},
		{"$(echo $(date))", false},
		{"$(cmd)", false},
		{"echo $(date", true},
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
