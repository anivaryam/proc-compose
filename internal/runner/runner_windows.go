//go:build windows

package runner

import (
	"context"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"syscall"
)

var (
	reVar    = regexp.MustCompile(`\$[a-zA-Z_][a-zA-Z0-9_]*|\$\{[^}]+\}`)
	reCmdSub = regexp.MustCompile(`\$\([^)]+\)`)
	// reSleep matches a command that is sh's sleep with one argument: a number
	// of seconds, which may have a fraction, as sh's sleep accepts it.
	reSleep = regexp.MustCompile(`(?i)^(\s*)sleep\s+(\d+\.?\d*|\.\d+)(\s*)$`)
)

// shDoubleQuoteEscapes undoes the backslash escapes sh honours inside a
// double-quoted string. A backslash before any other byte is literal there.
var shDoubleQuoteEscapes = strings.NewReplacer(`\$`, `$`, "\\`", "`", `\"`, `"`, `\\`, `\`, "\\\n", "")

// shellExec returns the command that runs command through the platform shell.
//
// os/exec quotes arguments for programs that split their command line with
// CommandLineToArgvW, and cmd.exe is not one of them: given the script as one
// argument, os/exec wraps it in quotes and backslash-escapes every quote inside,
// and cmd runs those backslashes as part of the command. The command line is
// therefore written here and handed to CreateProcess as it stands, through
// SysProcAttr.CmdLine. /s makes cmd remove exactly the quote pair around the
// script and run what is inside it unchanged, whatever quotes the script
// contains; without /s, cmd keeps or removes the pair depending on the script.
//
// The SysProcAttr is set here, before the process group's Setup, which adds its
// creation flag to an existing SysProcAttr instead of replacing it.
func shellExec(ctx context.Context, command string) *exec.Cmd {
	shell, args := shellCommand(command)
	cmd := exec.CommandContext(ctx, shell, args...)
	// args is /c and the script.
	cmd.SysProcAttr = &syscall.SysProcAttr{CmdLine: shell + ` /s /c "` + args[1] + `"`}
	return cmd
}

// shellCommand returns the platform shell and the arguments that run command
// under it. shellExec is what delivers them to cmd intact.
//
// Every command runs under cmd, which has the operators a config uses natively:
// &&, ||, | and the redirections >, >>, < and 2>&1. Windows PowerShell 5.1, the
// "powershell" on PATH, has no && or ||, writes a > redirection as UTF-16 with a
// byte-order mark, and reads $(...) as its own subexpression, so no command is
// routed to it; PowerShell syntax has to name powershell in the command itself.
func shellCommand(command string) (string, []string) {
	script := command
	if !isExplicitPowerShellCommand(command) {
		// Only the sh -c shim is requoted: anywhere else the command is cmd
		// syntax, where an apostrophe is ordinary text.
		if inner, ok := strings.CutPrefix(command, "sh -c "); ok {
			script = requoteForCMD(unquoteShellWord(inner))
		}
		script = translateToCMD(script)
	}
	return "cmd", []string{"/c", script}
}

func isExplicitPowerShellCommand(command string) bool {
	cmd := strings.TrimSpace(command)
	lower := strings.ToLower(cmd)
	return strings.HasPrefix(lower, "powershell ") ||
		strings.HasPrefix(lower, "powershell.exe ") ||
		strings.HasPrefix(lower, "pwsh ") ||
		strings.HasPrefix(lower, "pwsh.exe ")
}

// unquoteShellWord returns the script an sh -c argument holds, removing its
// quoting the way sh does: a single-quoted word is taken literally, and inside a
// double-quoted word a backslash before $, `, ", \ or a line break is removed.
// Anything else — an unquoted script, an unterminated quote, or text after the
// closing quote — is returned unchanged.
func unquoteShellWord(s string) string {
	if len(s) < 2 {
		return s
	}
	switch s[0] {
	case '\'':
		if strings.IndexByte(s[1:], '\'') == len(s)-2 {
			return s[1 : len(s)-1]
		}
	case '"':
		if closingDoubleQuote(s, 0) == len(s)-1 {
			return shDoubleQuoteEscapes.Replace(s[1 : len(s)-1])
		}
	}
	return s
}

// requoteForCMD rewrites the quoted strings of an sh script into the quoting
// cmd and the programs it starts understand, wherever that leaves what a
// program receives unchanged:
//
//   - a quoted string made only of letters, digits and -_./:=+ loses its
//     quotes, as sh would remove them, so echo prints x for 'x' as sh does;
//   - any other single-quoted string is put in double quotes, the only quotes
//     cmd and the C runtime's argument parser know, unless it contains ", %, $
//     or a line break, or ends in a backslash: cmd, the parser or the $VAR
//     translation would change those, so they are kept as written.
//
// Other double-quoted strings are kept as written: the C runtime's argument
// parser reads them as sh does, escaped quotes included, except that it keeps
// the backslash of \\, \$ and \`. Everything outside quotes is kept as written
// too. A script with an unterminated quote is returned unchanged.
func requoteForCMD(script string) string {
	var b strings.Builder
	for i := 0; i < len(script); i++ {
		switch script[i] {
		case '\\':
			// Outside quotes a backslash escapes the next byte, so that byte
			// cannot open a quote. Both are kept as written.
			b.WriteString(script[i:min(i+2, len(script))])
			i++
		case '\'':
			end := strings.IndexByte(script[i+1:], '\'')
			if end < 0 {
				return script
			}
			text := script[i+1 : i+1+end]
			switch {
			case isPlainShellText(text):
				b.WriteString(text)
			case strings.ContainsAny(text, "\"%$\r\n") || strings.HasSuffix(text, `\`):
				b.WriteString(script[i : i+end+2])
			default:
				b.WriteString(`"` + text + `"`)
			}
			i += end + 1
		case '"':
			end := closingDoubleQuote(script, i)
			if end < 0 {
				return script
			}
			if text := script[i+1 : end]; isPlainShellText(text) {
				b.WriteString(text)
			} else {
				b.WriteString(script[i : end+1])
			}
			i = end
		default:
			b.WriteByte(script[i])
		}
	}
	return b.String()
}

// closingDoubleQuote returns the index of the quote that closes the
// double-quoted sh string opened at s[open], or -1 when it is unterminated. A
// backslash inside the string escapes the byte after it.
func closingDoubleQuote(s string, open int) int {
	for i := open + 1; i < len(s); i++ {
		switch s[i] {
		case '\\':
			i++
		case '"':
			return i
		}
	}
	return -1
}

// isPlainShellText reports whether s is non-empty and made only of letters,
// digits and -_./:=+, which neither sh nor cmd treats specially, so quoting s
// changes nothing.
func isPlainShellText(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !('a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' || strings.IndexByte("-_./:=+", c) >= 0) {
			return false
		}
	}
	return true
}

func translateToCMD(cmd string) string {
	result := reVar.ReplaceAllStringFunc(cmd, func(match string) string {
		varName := match[1:]
		if len(varName) > 0 && varName[0] == '{' {
			varName = varName[1 : len(varName)-1]
		}
		return "%" + varName + "%"
	})
	result = reCmdSub.ReplaceAllStringFunc(result, func(match string) string {
		subCmd := match[2 : len(match)-1]
		return "%(" + subCmd + ")%"
	})
	return translateSleep(result)
}

// translateSleep rewrites each command of script that is sh's sleep into one
// that waits as long under cmd. The commands are the text between cmd's
// operators &, | and the parentheses, outside double quotes and not escaped with
// ^, so a sleep that is only text, as in echo sleep 5, is left alone. So is a
// sleep with any other argument, or with more than one: cmd then runs it as
// written, and fails unless a sleep program is on PATH, much as sh's sleep fails
// on an argument it cannot read.
func translateSleep(script string) string {
	var b strings.Builder
	start, quoted := 0, false
	for i := 0; i < len(script); i++ {
		switch c := script[i]; {
		case c == '"':
			quoted = !quoted
		case quoted:
			// Inside double quotes the operators and ^ are text.
		case c == '^':
			i++
		case c == '&' || c == '|' || c == '(' || c == ')':
			b.WriteString(sleepCommand(script[start:i]))
			b.WriteByte(c)
			start = i + 1
		}
	}
	return b.String() + sleepCommand(script[start:])
}

// sleepCommand returns the cmd command that waits as long as command when
// command is sh's sleep, and command unchanged otherwise.
//
// timeout /t cannot stand in for sleep: it refuses to run unless its input is a
// console, and a managed process's input is not one. ping sends its echo
// requests a second apart, so n+1 of them to the loopback address take n
// seconds. cmd has no finer wait, so a fraction of a second is rounded up to
// the next whole second: the wait is never shorter than sh's. (PowerShell's
// Start-Sleep is finer, but PowerShell can take seconds to start on a cold
// machine, longer than the fraction it would wait.)
func sleepCommand(command string) string {
	m := reSleep.FindStringSubmatch(command)
	if m == nil {
		return command
	}
	ms, ok := sleepMillis(m[2])
	if !ok {
		return command
	}
	seconds := (ms + 999) / 1000
	return m[1] + "ping -n " + strconv.FormatUint(seconds+1, 10) + " 127.0.0.1 >nul" + m[3]
}

// sleepMillis returns seconds, a decimal number, in milliseconds, rounded up so
// that the wait is never shorter than sh's. It reports false for a number too
// large to count.
func sleepMillis(seconds string) (uint64, bool) {
	whole, fraction, _ := strings.Cut(seconds, ".")
	fraction += "000"
	// At most 63 bits, so that rounding up cannot overflow.
	ms, err := strconv.ParseUint(whole+fraction[:3], 10, 63)
	if err != nil {
		return 0, false
	}
	if strings.Trim(fraction[3:], "0") != "" {
		ms++
	}
	return ms, true
}
