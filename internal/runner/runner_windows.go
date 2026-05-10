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
	if strings.HasPrefix(command, "sh -c ") {
		inner := strings.TrimPrefix(command, "sh -c ")
		inner = strings.Trim(inner, "\"'")
		return "cmd", []string{"/c", translateToCMD(inner)}
	}

	if needsPowerShell(command) {
		return "powershell", []string{"-Command", translateToPowerShell(command)}
	}
	return "cmd", []string{"/c", translateToCMD(command)}
}

func needsPowerShell(cmd string) bool {
	if strings.Contains(cmd, "|") {
		return true
	}
	if strings.Contains(cmd, "&&") || strings.Contains(cmd, "||") {
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
