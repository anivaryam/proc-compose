# Installation

## With brokit (recommended)

[brokit](https://github.com/anivaryam/brokit) handles install, update, and uninstall across the whole tool family in one command:

XX(https://github.com/anivaryam/brokit)** (recommended — handles install, update, and uninstall across the whole tool family in one command):

```sh
brokit install proc-compose         # install latest release
brokit update proc-compose          # upgrade to latest
brokit list                         # see installed tools and versions
brokit remove proc-compose          # uninstall
```

`brokit` reads the GitHub releases for this repo, verifies the binary, and drops it into `/usr/local/bin`. It also installs the optional companions (`merge-port`, `tunnel`, `env-vault`, `proxy-relay`) the same way.

## Release binary (Linux and macOS)

```sh
curl -sSL https://raw.githubusercontent.com/anivaryam/proc-compose/main/install.sh | bash
```

The script downloads a release binary for your platform into `~/.local/bin/`
(override with `PROC_COMPOSE_INSTALL_DIR=/path/to/dir`). Pin a specific
version with `PROC_COMPOSE_VERSION=v1.2.3`. If no release matches your
platform but `go` is on `$PATH`, the script falls back to `go install`.

## Go

```sh
go install github.com/anivaryam/proc-compose/cmd/proc-compose@latest
```

## From source

```sh
git clone https://github.com/anivaryam/proc-compose.git
cd proc-compose
make install  # copies to ~/.local/bin/
```

## Windows

```bash
# Via Go
go install github.com/anivaryam/proc-compose/cmd/proc-compose@latest

# Or download a release binary from:
# https://github.com/anivaryam/proc-compose/releases
```

Note: `merge-port` must be installed separately if you use the `merge:` feature.

## Optional binary dependencies

proc-compose unlocks additional capabilities when these binaries are available on your `PATH`. The simplest way to install them is `brokit install <name>`; alternatives are listed for reference.

| Binary | Enables | Install |
|--------|---------|---------|
| [`merge-port`](https://github.com/anivaryam/merge-port) | `merge:` config section — combine frontend + backend into one port | `brokit install merge-port` |
| [`tunnel`](https://github.com/anivaryam/tunnel) | Public URL for a local port (see [examples](examples.md#full-stack-with-public-tunnel)) | `brokit install tunnel` |

If `merge:` is configured but merge-port is not on PATH, `up`, `validate`, and `up --dry-run` fail with an actionable error. `doctor` reports a finding. Without `merge:`, merge-port is not required.

## Agent skill for Claude Code and OpenCode

The repository ships an agent skill, [`skills/proc-compose`](../skills/proc-compose/SKILL.md),
that teaches coding agents every proc-compose feature: setup, running, testing,
logs, sessions, and deployment. It uses the shared `SKILL.md` format, and
OpenCode also reads Claude Code's skill directories, so one copy serves both.

```sh
# From a clone: link it, so git pull keeps it current
mkdir -p ~/.claude/skills
ln -s "$PWD/skills/proc-compose" ~/.claude/skills/proc-compose

# Without a clone: copy it
git clone --depth 1 https://github.com/anivaryam/proc-compose.git /tmp/proc-compose-src
mkdir -p ~/.claude/skills
cp -r /tmp/proc-compose-src/skills/proc-compose ~/.claude/skills/
```

| Location | Claude Code | OpenCode |
|---|---|---|
| `~/.claude/skills/proc-compose/` | Every project | Every project |
| `<project>/.claude/skills/proc-compose/` | That project | That project |
| `~/.config/opencode/skills/proc-compose/` | No | Every project |

On Windows, copy the directory to `%USERPROFILE%\.claude\skills\proc-compose`.
A copy does not update itself; copy it again after upgrading proc-compose. The
skill tells agents to run `proc-compose agent-guide` first and to trust the
installed binary's help where the two differ.

## Update notifications

Interactive runs tell you when a newer stable release is available. The notice is
aimed at the copy you are actually running — it names the directory that copy
lives in, so the command it prints replaces that file rather than adding a second
one your shell would keep shadowing.

```
A new proc-compose release is available: v1.2.0 → v1.3.0
This copy is in /home/you/.local/bin. Update this exact file with:
  curl -sSfL https://raw.githubusercontent.com/anivaryam/proc-compose/main/install.sh | PROC_COMPOSE_INSTALL_DIR='/home/you/.local/bin' bash
If you installed with brokit, use 'brokit update proc-compose'.
If brokit is not managing it yet, that command explains how to hand it over.
```

That first command works whether or not brokit manages this copy, and it does not
assume anything about how the copy got there — `install.sh`, `make install`, and
`go install` all put files in the same directories. The assignment sits on the
installer, after the pipe, because `PROC_COMPOSE_INSTALL_DIR=x curl … | bash`
would set it for `curl` and leave the installer to write to its own default.

`brokit update proc-compose` is worth knowing about, but the notice deliberately
does not claim brokit manages your copy. brokit only ever installs into one
directory, and so do the other installers here, so the directory alone proves
nothing. What settles it is brokit's own output: for a copy it manages it
updates it, and for one it does not it prints the one-time command that takes
that exact file over. Either way it ends in an answer.

**Windows:** there is no `install.ps1` for proc-compose — this repository's
standalone installer is `install.sh`, for Linux and macOS. On Windows the flow
that can be aimed at one directory is brokit's own installer, so the notice
prints that in PowerShell syntax:

```
This copy is in C:\tools. Update this exact file with:
  $env:BROKIT_BIN = 'C:\tools'; brokit install --force proc-compose
That works whether or not brokit already manages this copy.
Keep BROKIT_BIN set to 'C:\tools' for later updates. The assignment above only lasts
as long as this terminal. Run this once to keep it in new terminals:
  [Environment]::SetEnvironmentVariable("BROKIT_BIN", 'C:\tools', "User")
Then keep it current with:
  brokit update proc-compose
```

`--force` is what lets one command cover both cases: brokit refuses a plain
`install` once a record exists, and the notice cannot tell a copy brokit already
manages from one it has never seen.

You do not have to wait for a notification to do any of this. To check and
migrate at any time:

```sh
brokit list                          # STATUS shows "unmanaged: <path>" for copies brokit does not own
brokit update proc-compose           # explains what it found and prints the command to take over
```

**If you take a copy over with `BROKIT_BIN` set to a custom directory, make that
setting permanent.** brokit records the version it installed but not the path, so
a later plain `brokit update proc-compose` would otherwise install into
`~/.local/bin` and leave the copy your shell runs untouched. brokit's diagnostic
reminds you and prints the command; add the export to your shell profile to make
it stick.

The check is notification-only. proc-compose never downloads, installs, or replaces anything, and never runs brokit or any other command to work out how it was installed.

**When you see it:** only when stderr is a terminal *and* the command's output is meant for a person. Help, `--version`, `completion`, `man`, `--json` output, `up --silent`, `up --survive`, and `up --log-format json` never print it — and neither does a cron job or a CI step, whose stderr is a log file.

**Network policy:** lookups use `https://api.github.com/repos/anivaryam/proc-compose/releases/latest`, with a 3-second timeout and a bounded response read. Recorded attempts suppress further lookups for 24 hours; simultaneous invocations with a missing or stale cache may each make a request. Draft and prerelease releases are ignored. Development builds (`dev`, a bare commit hash, or a `-dirty` / describe-suffixed version) skip checks entirely.

**Cache:** the last result is recorded in `$XDG_CACHE_HOME/proc-compose/update-check.json` (normally `~/.cache/proc-compose/` on Linux, or `~/Library/Caches/proc-compose/` on macOS). The file is shared by every project for the same user and never written into a project directory. Failed attempts are recorded for the same 24 hours, so an offline or rate-limited machine does not retry on every command. If the user cache directory cannot be created, update checks are skipped.

The check never blocks a command. The notice is printed from the cache alone; when that cache is missing or stale the lookup runs in the background, so a fresh result shows up on the *next* invocation instead of delaying the current one. Because the cache is shared and long-lived, one `proc-compose up` or `proc-compose monitor` session is enough to arm it for the next 24 hours — a short command such as `list` finishes long before a background request could land, so it never waits and never blocks, it just may be the invocation that does not arm the cache.
