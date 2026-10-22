//go:build windows

package runner

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// shellCases is the Windows shell contract, read by both TestShellTranslation and
// TestShellExec: a configured command, the script cmd /c runs for it, and what
// running that script prints on Windows — or, when file is set, the bytes it
// writes to that file. A case with fails set is malformed, and cmd reports it on
// stderr with a non-zero exit.
//
// TestShellExec runs the commands with PORT=3000, DB_HOST=db and HOME=C:\home,
// and with %ARGV% expanding to a program that prints each argument it receives
// as [arg] on its own line.
var shellCases = []struct {
	name   string
	cmd    string
	script string
	out    string
	file   string
	fails  bool
}{
	{name: "plain command uses cmd", cmd: "echo hello", script: "echo hello", out: "hello\r\n"},
	{name: "cmd env syntax is native", cmd: "echo %PORT%", script: "echo %PORT%", out: "3000\r\n"},
	{name: "posix env remains best effort compatibility", cmd: "echo $PORT", script: "echo %PORT%", out: "3000\r\n"},
	{name: "braced posix env remains best effort compatibility", cmd: "echo ${DB_HOST}", script: "echo %DB_HOST%", out: "db\r\n"},
	{name: "path text is not normalized", cmd: "echo go run ./cmd/server", script: "echo go run ./cmd/server", out: "go run ./cmd/server\r\n"},
	{name: "env var in path is best effort compatibility", cmd: "echo $HOME/bin/app", script: "echo %HOME%/bin/app", out: `C:\home/bin/app` + "\r\n"},
	// cmd leaves an undefined variable as written.
	{name: "cmd substitution is best effort compatibility", cmd: "echo $(date)", script: "echo %(date)%", out: "%(date)%\r\n"},
	{name: "unbalanced substitution uses the cmd path", cmd: "echo $(date", script: "echo $(date", out: "$(date\r\n"},

	// Quotes reach cmd as written, so a program receives what the C runtime's
	// argument parser makes of them, and a command can start with a quoted path.
	{name: "double quotes reach the program", cmd: `%ARGV% "a b" c`, script: `%ARGV% "a b" c`, out: "[a b]\n[c]\n"},
	{name: "quoted program path runs", cmd: `"%ARGV_EXE%" -test.run=TestShellArgvHelper -- "a b"`, script: `"%ARGV_EXE%" -test.run=TestShellArgvHelper -- "a b"`, out: "[a b]\n"},
	{name: "explicit powershell remains user command text", cmd: `powershell -NoProfile -Command "Write-Output $env:PORT"`, script: `powershell -NoProfile -Command "Write-Output $env:PORT"`, out: "3000\r\n"},
	{name: "explicit powershell keeps its inner quotes", cmd: `powershell -NoProfile -Command "Write-Host 'hello from proc-compose'"`, script: `powershell -NoProfile -Command "Write-Host 'hello from proc-compose'"`, out: "hello from proc-compose\n"},

	// Every operator is cmd's own. cmd's echo prints the space before an
	// operator, and cmd ends each line with CRLF.
	{name: "and command uses the cmd path", cmd: "echo a && echo b", script: "echo a && echo b", out: "a \r\nb\r\n"},
	{name: "or command uses the cmd path", cmd: "cd nosuchdir 2>nul || echo fallback", script: "cd nosuchdir 2>nul || echo fallback", out: "fallback\r\n"},
	{name: "piped command uses the cmd path", cmd: "echo a | findstr a", script: "echo a | findstr a", out: "a \r\n"},
	{name: "multiple pipes use the cmd path", cmd: "echo a | findstr a | findstr a", script: "echo a | findstr a | findstr a", out: "a \r\n"},
	{name: "mixed and and pipe uses the cmd path", cmd: "echo a && echo b | findstr b", script: "echo a && echo b | findstr b", out: "a \r\nb \r\n"},
	{name: "mixed pipe and and uses the cmd path", cmd: "echo a | findstr a && echo c", script: "echo a | findstr a && echo c", out: "a \r\nc\r\n"},
	{name: "mixed or and pipe uses the cmd path", cmd: "cd nosuchdir 2>nul || echo b | findstr b", script: "cd nosuchdir 2>nul || echo b | findstr b", out: "b \r\n"},
	{name: "redirect command uses the cmd path", cmd: "echo hi > out.txt", script: "echo hi > out.txt", out: "hi \r\n", file: "out.txt"},
	{name: "append redirect uses the cmd path", cmd: "echo a>out.txt&& echo b>>out.txt", script: "echo a>out.txt&& echo b>>out.txt", out: "a\r\nb\r\n", file: "out.txt"},
	{name: "input redirect uses the cmd path", cmd: "echo hi>in.txt&& findstr hi < in.txt", script: "echo hi>in.txt&& findstr hi < in.txt", out: "hi\r\n"},
	{name: "stderr redirect uses the cmd path", cmd: "echo hi 2>&1", script: "echo hi 2>&1", out: "hi \r\n"},

	// The sh -c shim removes the quoting sh would, then requotes what it can.
	{name: "sh -c drops single quotes around plain text", cmd: `sh -c "echo 'x'"`, script: "echo x", out: "x\r\n"},
	{name: "sh -c drops double quotes around plain text", cmd: `sh -c 'echo "x"'`, script: "echo x", out: "x\r\n"},
	{name: "sh -c unescapes quotes as sh does", cmd: `sh -c "echo \"hello\""`, script: "echo hello", out: "hello\r\n"},
	{name: "sh -c without inner quotes is unchanged", cmd: "sh -c echo hello", script: "echo hello", out: "hello\r\n"},
	{name: "sh -c with a quoted inner command drops only that pair", cmd: `sh -c "echo hello"`, script: "echo hello", out: "hello\r\n"},
	{name: "sh -c still translates posix env", cmd: `sh -c "echo $PORT"`, script: "echo %PORT%", out: "3000\r\n"},
	{name: "sh -c double quotes single-quoted text for a program", cmd: `sh -c "%ARGV% 'a b' 'c&d' x"`, script: `%ARGV% "a b" "c&d" x`, out: "[a b]\n[c&d]\n[x]\n"},
	{name: "sh -c passes an escaped quote to a program", cmd: `sh -c '%ARGV% "say \"hi\""'`, script: `%ARGV% "say \"hi\""`, out: "[say \"hi\"]\n"},
	// The C runtime keeps a backslash sh would drop, and $VAR is translated.
	{name: "sh -c keeps other escapes in double quotes", cmd: `sh -c '%ARGV% "a\\b" "\$PORT"'`, script: `%ARGV% "a\\b" "\%PORT%"`, out: `[a\\b]` + "\n" + `[\3000]` + "\n"},
	// cmd's echo prints the quotes a program would not receive.
	{name: "sh -c echo prints the double quotes", cmd: `sh -c "echo 'a b'"`, script: `echo "a b"`, out: "\"a b\"\r\n"},
	// cmd would expand or re-split these, so they keep their single quotes.
	{name: "sh -c keeps single quotes around a double quote", cmd: `sh -c "%ARGV% 'say \"hi\"'"`, script: `%ARGV% 'say "hi"'`, out: "['say]\n[hi']\n"},
	{name: "sh -c keeps single quotes around a percent sign", cmd: `sh -c "%ARGV% '%PORT%'"`, script: `%ARGV% '%PORT%'`, out: "['3000']\n"},
	{name: "sh -c keeps single quotes around a trailing backslash", cmd: `sh -c "%ARGV% 'C:\dir\'"`, script: `%ARGV% 'C:\dir\'`, out: `['C:\dir\']` + "\n"},
	{name: "sh -c keeps single quotes around a dollar sign", cmd: `sh -c "echo '\$PORT'"`, script: "echo '%PORT%'", out: "'3000'\r\n"},
	{name: "sh -c with mismatched outer quotes is untouched", cmd: `sh -c "echo 'x'`, script: `"echo 'x'`, fails: true},
	{name: "sh -c with a single quote byte is untouched", cmd: `sh -c "`, script: `"`, fails: true},
}

func TestShellTranslation(t *testing.T) {
	for _, tt := range shellCases {
		t.Run(tt.name, func(t *testing.T) {
			shell, args := shellCommand(tt.cmd)
			if want := []string{"/c", tt.script}; shell != "cmd" || !slices.Equal(args, want) {
				t.Errorf("shellCommand(%q) = %q, %q; want %q, %q", tt.cmd, shell, args, "cmd", want)
			}
		})
	}
}

// TestShellExec runs every shellCases command the way the runner launches it and
// checks what cmd really does with it, so the table states cmd's behaviour and
// not only the translation.
func TestShellExec(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	env := append(os.Environ(),
		"PORT=3000", "DB_HOST=db", `HOME=C:\home`,
		"ARGV_EXE="+exe, `ARGV="`+exe+`" -test.run=TestShellArgvHelper --`,
		shellArgvHelperEnv+"=1")
	for _, tt := range shellCases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			dir := t.TempDir()
			cmd := shellExec(ctx, tt.cmd)
			cmd.Dir = dir
			cmd.Env = env
			var stdout, stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			err := cmd.Run()
			if tt.fails {
				if err == nil {
					t.Errorf("%q succeeded; want a non-zero exit", tt.cmd)
				}
			} else if err != nil || stderr.Len() > 0 {
				t.Fatalf("%q: %v, stderr %q", tt.cmd, err, stderr.String())
			}
			got := stdout.String()
			if tt.file != "" {
				if got != "" {
					t.Errorf("%q printed %q; want nothing", tt.cmd, got)
				}
				b, err := os.ReadFile(filepath.Join(dir, tt.file))
				if err != nil {
					t.Fatal(err)
				}
				got = string(b)
			}
			if got != tt.out {
				t.Errorf("%q gave %q; want %q", tt.cmd, got, tt.out)
			}
		})
	}
}

// TestShellExecKeepsCmdLineThroughSetup pins the order shellExec relies on: the
// process group's Setup runs after it and must add its creation flag without
// dropping the command line shellExec wrote.
func TestShellExecKeepsCmdLineThroughSetup(t *testing.T) {
	cmd := shellExec(context.Background(), `echo "a b"`)
	pg := newProcessGroup()
	defer pg.Close()
	pg.Setup(cmd)
	if got, want := cmd.SysProcAttr.CmdLine, `cmd /s /c "echo "a b""`; got != want {
		t.Errorf("CmdLine = %q; want %q", got, want)
	}
	if cmd.SysProcAttr.CreationFlags&windows.CREATE_SUSPENDED == 0 {
		t.Errorf("CreationFlags = %#x; want CREATE_SUSPENDED", cmd.SysProcAttr.CreationFlags)
	}
}

const shellArgvHelperEnv = "PROC_COMPOSE_TEST_PRINT_ARGV"

// TestShellArgvHelper is not a test of its own. TestShellExec runs this binary
// through cmd, as %ARGV%, to print each argument after -- as [arg].
func TestShellArgvHelper(t *testing.T) {
	if os.Getenv(shellArgvHelperEnv) != "1" {
		return
	}
	for i, arg := range os.Args {
		if arg == "--" {
			for _, a := range os.Args[i+1:] {
				fmt.Printf("[%s]\n", a)
			}
			break
		}
	}
	os.Exit(0)
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
