// Choosing the right upgrade command for the copy that is actually running.
//
// proc-compose can be on a machine in more than one shape: installed by brokit,
// dropped in by this repository's own install.sh, copied by `make install`, or
// built with `go install`. Any single unconditional instruction is wrong for
// most of those shapes.
//
// Two things are deliberately not done:
//
//   - brokit's state file is never read. Its format is brokit's own business,
//     and this check must keep working when brokit is not installed at all.
//   - brokit is never executed. A notification cannot afford to spawn a process,
//     wait for the network, and fail, on somebody's every interactive command.
//
// What is used instead is the directory the running binary lives in. That is
// enough for one thing that matters most: every command printed here is aimed at
// that directory, so whichever one the user runs, the file their shell executes
// is the file that gets replaced. Nothing here adds a second copy that the shell
// would keep shadowing.
//
// Ownership is deliberately NOT inferred from the directory. brokit installs
// into one directory, but so does this repository's own install.sh and its
// Makefile, and a copy sitting in that directory may well have come from either.
// Telling an unregistered copy in ~/.local/bin to run `brokit update` would be
// exactly the dead end this notice exists to avoid, so the directory alone never
// earns the claim that brokit manages the copy. `brokit update` is named as one
// of the ways to update, not asserted to be the way — and because brokit's own
// diagnostic explains itself when it finds an unmanaged binary, that command
// still ends in an answer rather than a bare refusal.
package update

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/anivaryam/proc-compose/internal/ansi"
)

// Location is what is known about the copy of proc-compose that is running.
type Location struct {
	// Dir holds the running binary. Empty when it cannot be determined.
	Dir string
}

// Detect describes the running copy. It resolves symlinks first, so a copy
// reached through a convenience alias is recognised by the directory of the
// real binary — the one an installer has to write to.
func Detect() Location {
	exe, err := os.Executable()
	if err != nil {
		return Location{}
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	dir := filepath.Dir(exe)
	if dir == "" {
		return Location{}
	}
	return Location{Dir: dir}
}

// MessageFor renders the notice for a known location.
//
// Colour goes through ansi.Wrap so NO_COLOR / --no-color are honoured by the same
// switch the runner and monitor already use.
func MessageFor(installed, latest string, loc Location) string {
	bold, reset := ansi.Wrap(ansi.Bold), ansi.Wrap(ansi.Reset)
	head := fmt.Sprintf("A new proc-compose release is available: %s%s%s → %s%s%s\n",
		bold, installed, reset, bold, latest, reset)
	return head + guidance(loc, bold, reset)
}

// guidance prints one command that is guaranteed to update this copy, plus the
// brokit route as an alternative rather than a claim.
//
// The first command is the important one: it replaces the running file wherever
// it lives, so it is correct whether or not brokit manages it. The note names the
// supported lifecycle for anyone who wants it; brokit's own output is what
// settles whether it applies, which is why nothing here asserts it.
func guidance(loc Location, bold, reset string) string {
	if loc.Dir == "" {
		// Nothing is known about where this binary lives, so there is no
		// directory to aim a command at. Keep the established guidance rather
		// than inventing a path.
		return fmt.Sprintf("Run: %s%s%s\n", bold, UpdateCommand, reset)
	}
	command, note := updateAdvice(loc.Dir)
	return fmt.Sprintf("This copy is in %s. Update this exact file with:\n"+
		"  %s%s%s\n%s",
		quoteArg(loc.Dir), bold, command, reset, note)
}

// updateAdvice returns the command that replaces the running copy in its own
// directory, and what to do afterwards on this platform.
//
// The two platforms differ because the supported flows differ. This repository
// ships install.sh for Linux and macOS but has no Windows installer script, so on
// Windows the flow that can be aimed at one specific directory is brokit's own
// installer — which also means BROKIT_BIN has to stick, or the next plain
// `brokit update` would write to brokit's default directory instead.
func updateAdvice(dir string) (command, note string) {
	if runtime.GOOS == "windows" {
		return windowsUpdateAdvice(dir)
	}
	return posixUpdateAdvice(dir)
}

// posixUpdateAdvice uses this repository's own installer, pointed at the running
// copy's directory with the assignment on the receiving command. Putting it
// before `curl` would set the variable for curl, leaving the installer to fall
// back to its own default and update a different file than the one in use.
func posixUpdateAdvice(dir string) (string, string) {
	command := fmt.Sprintf("curl -sSfL %s | PROC_COMPOSE_INSTALL_DIR=%s bash",
		InstallScriptURL, posixQuoteArg(dir))
	return command, "If you installed with brokit, use 'brokit update proc-compose'.\n" +
		"If brokit is not managing it yet, that command explains how to hand it over.\n"
}

// windowsUpdateAdvice hands the copy to brokit in place, because brokit is the
// only supported Windows flow that can target one directory.
//
// `install --force` is the linchpin, and deliberately so: brokit refuses a plain
// `install` when a record already exists, which is exactly the case for a copy
// brokit already manages. `--force` installs and records either way, so one
// command covers a managed update and an unmanaged handover without this notice
// having to know which it is looking at.
//
// The follow-up is mandatory, not optional: BROKIT_BIN only reaches the process
// that sets it, so the plain `brokit update` written below would install into
// brokit's default directory and leave the copy the shell runs untouched. The
// profile line is needed for new terminals; the current one keeps the assignment
// above.
func windowsUpdateAdvice(dir string) (string, string) {
	quoted := windowsQuoteArg(dir)
	command := fmt.Sprintf("$env:BROKIT_BIN = %s; brokit install --force proc-compose", quoted)
	note := fmt.Sprintf("That works whether or not brokit already manages this copy.\n"+
		"Keep BROKIT_BIN set to %s for later updates. The assignment above only lasts\n"+
		"as long as this terminal. Run this once to keep it in new terminals:\n"+
		"  [Environment]::SetEnvironmentVariable(\"BROKIT_BIN\", %s, \"User\")\n"+
		"Then keep it current with:\n"+
		"  brokit update proc-compose\n", quoted, quoted)
	return command, note
}

// quoteArg renders a directory for this platform's shell.
func quoteArg(path string) string {
	if runtime.GOOS == "windows" {
		return windowsQuoteArg(path)
	}
	return posixQuoteArg(path)
}

// posixQuoteArg renders a path so it survives being pasted into a POSIX shell.
// The commands are copy-paste instructions, so a path with a space in it has to
// be quoted or it silently targets somewhere else.
func posixQuoteArg(path string) string {
	if path != "" && !strings.ContainsAny(path, " \t\n\r'\"\\$`&|;<>()*?[]{}#~!") {
		return path
	}
	// Close the quoted run, emit an escaped quote, reopen: the only quoting
	// form POSIX shells accept without expanding anything.
	return "'" + strings.ReplaceAll(path, "'", `'\''`) + "'"
}

// windowsQuoteArg renders a path for PowerShell. Single quotes are literal there,
// so nothing inside needs escaping but a single quote itself, which is doubled.
func windowsQuoteArg(path string) string {
	return "'" + strings.ReplaceAll(path, "'", "''") + "'"
}
