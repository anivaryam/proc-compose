//go:build windows

package runner

import (
	"regexp"
	"strings"
)

var (
	reVar    = regexp.MustCompile(`\$[a-zA-Z_][a-zA-Z0-9_]*|\$\{[^}]+\}`)
	reCmdSub = regexp.MustCompile(`\$\([^)]+\)`)
	reSleep  = regexp.MustCompile(`(?i)\bsleep\s+(\d+)\b`)
)

// shellCommand returns the platform shell and arguments for running a command string.
func shellCommand(command string) (string, []string) {
	if isExplicitPowerShellCommand(command) {
		return "cmd", []string{"/c", command}
	}

	if strings.HasPrefix(command, "sh -c ") {
		inner := strings.TrimPrefix(command, "sh -c ")
		inner = stripOuterQuotes(inner)
		return "cmd", []string{"/c", translateToCMD(inner)}
	}

	if needsPowerShell(command) {
		return "powershell", []string{"-Command", translateToPowerShell(command)}
	}
	return "cmd", []string{"/c", translateToCMD(command)}
}

func isExplicitPowerShellCommand(command string) bool {
	cmd := strings.TrimSpace(command)
	lower := strings.ToLower(cmd)
	return strings.HasPrefix(lower, "powershell ") ||
		strings.HasPrefix(lower, "powershell.exe ") ||
		strings.HasPrefix(lower, "pwsh ") ||
		strings.HasPrefix(lower, "pwsh.exe ")
}

// stripOuterQuotes removes exactly one matched outer quote pair from s and
// returns the inner bytes verbatim. A string whose ends do not match, or that
// is shorter than two bytes, is returned unchanged.
//
// The sh -c shim hands the inner text to cmd /c, so only the pair a shell
// would have removed is removed: cmd has no single-quote semantics and does
// not interpret a backslash-escaped quote, so the text after the pair is
// passed through as it stands.
func stripOuterQuotes(s string) string {
	if len(s) < 2 {
		return s
	}
	first, last := s[0], s[len(s)-1]
	if first != last || (first != '"' && first != '\'') {
		return s
	}
	return s[1 : len(s)-1]
}

// needsPowerShell reports whether a command has to run under PowerShell.
//
// The && and || test comes first and answers false, because cmd supports both
// natively while Windows PowerShell 5.1 — the "powershell" on PATH — has no
// chain operators at all. It has to be tested before the pipe test: "||"
// contains "|", so a pipe test evaluated first would send the common "a || b"
// form to a PowerShell that cannot parse it. A command that mixes operators
// therefore runs under cmd as well.
func needsPowerShell(cmd string) bool {
	if strings.Contains(cmd, "&&") || strings.Contains(cmd, "||") {
		return false
	}
	if strings.Contains(cmd, "|") {
		return true
	}
	if strings.Contains(cmd, ">") || strings.Contains(cmd, "2>") {
		return true
	}
	if strings.Contains(cmd, "$(") {
		depth := 0
		for i := 0; i < len(cmd); i++ {
			if cmd[i] == '$' && i+1 < len(cmd) && cmd[i+1] == '(' {
				depth++
				i++
			} else if cmd[i] == ')' {
				depth--
			}
		}
		if depth > 0 {
			return true
		}
	}
	return false
}

func translateToCMD(cmd string) string {
	result := reSleep.ReplaceAllStringFunc(cmd, func(match string) string {
		parts := reSleep.FindStringSubmatch(match)
		if len(parts) > 1 {
			return "timeout /t " + parts[1]
		}
		return match
	})
	result = reVar.ReplaceAllStringFunc(result, func(match string) string {
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
	return result
}

func translateToPowerShell(cmd string) string {
	return cmd
}
