# CLI reference

Run `proc-compose <command> --help` for the full flag list of any command, or `proc-compose man --dir DIR` to generate man pages.

## Commands

```
proc-compose bootstrap [--write] [--verify] [--json]    Generate setup and optionally validate config (dry run)
proc-compose doctor [--write] [--json]                  Scan project and diagnose setup
proc-compose init [--template T]                        Generate starter config
proc-compose validate                                   Parse config without starting
proc-compose up [processes...]                          Start all or named processes
proc-compose up --silent                                Run daemon in background
proc-compose monitor                                    Open live TUI for daemon
proc-compose status [--json]                            Show daemon process states
proc-compose logs [process] [--json] [--follow]         Query or follow bounded, structured process events
proc-compose restart <process>                          Restart one running process
proc-compose reload                                     Reload config and restart changed processes
proc-compose stop [--timeout N] [--force]               Stop daemon and verify its processes ended
proc-compose list                                       List config processes
proc-compose verify [processes...] -- PROGRAM [ARG...]  Owned stack + check + verified cleanup + receipt
proc-compose attempts [id|latest] [--json]              List or show verification receipts
proc-compose agent-guide                                Print the offline operating guide for coding agents
proc-compose why <process> | --attempt <id|latest>      Bounded failure evidence (live, retained, or one attempt)
proc-compose sessions [--live] [--json]                 Discover sessions, owners, URLs, latest attempt
proc-compose compare --baseline D --candidate D -- CMD  Run one check against two worktrees
proc-compose config check|plan|schema [--json]          Structured config diagnostics, startup plan, schema
proc-compose man [--dir DIR]                            Generate man pages
proc-compose uninstall --name <n>                       Remove systemd unit
proc-compose completion <shell>                         Generate shell completion (bash/zsh/fish/powershell)
proc-compose --version                                  Print version

Aliases:
  up=u  monitor=m  restart=r  reload=rl  status=st|ps  list=l
  init=i  doctor=doc  validate=check  uninstall=un

Global flags:
      --session string     Named session with independent identity, ports, logs, and runtime directory
  -f, --file string        Config file path (default "proc-compose.yml"; falls back to proc-compose.yaml and walks parent dirs)
  -v, --version            Print version (note: on `up`, -v is --verbose; use --version to be explicit)

Important flags (up):
  -s, --silent             Daemonize — run in the background and exit
  -v, --verbose            Verbose debug output (overrides root -v on this subcommand)
      --wait-ready         With --silent, block until every started process passes its readiness probe
      --wait-timeout int   Seconds to wait when --wait-ready is set (default 60)
      --log-file string    Write all output to a file (use with --silent)
      --max-log-size int   Rotate log file when it exceeds this size in bytes (0 = no rotation)
      --survive            Generate a systemd user unit for auto-restart on reboot
      --name string        Name used for the systemd unit file (used with --survive)
      --install            Install and enable the systemd unit (used with --survive)
      --force              Overwrite an existing unit file (used with --install)
      --dry-run            Validate config and list processes without starting
      --no-color           Disable ANSI color output (also: NO_COLOR env var)
      --log-format string  Log output format: text or json (default "text")
      --health-port int    HTTP port for /health endpoint (0 = disabled)

Important flags (bootstrap):
      --write              Create proc-compose.yml when no config exists
      --force              Overwrite an existing config when used with --write
      --verify             Validate generated config with a dry run; does not start processes
      --json               Emit machine-readable JSON

Flags (stop):
      --timeout int        Seconds to wait for a verified shutdown (default 10, plus 15 when the stack has teardown processes)
      --force              Force-terminate managed processes; clears a crashed daemon's state

Flags (init):
      --template string    Starter template: minimal (default), node, go, python

Flags (logs): see `proc-compose logs --help` and [Sessions and logs](sessions-and-logs.md)
```

## Environment variables

| Variable | Read by | Effect |
|----------|---------|--------|
| `NO_COLOR` | runner, monitor | Any non-empty value disables ANSI colour output |
| `PORT` | merge-port config | Used as `merge.port` when not set explicitly (cloud platforms) |
| `XDG_RUNTIME_DIR` | daemon | Preferred runtime directory; otherwise uses the per-user cache's `proc-compose` directory, then the OS temporary directory if the cache is unavailable |
| `PROC_COMPOSE_INSTALL_DIR` | `install.sh` | Override install target (default `~/.local/bin`) |
| `PROC_COMPOSE_VERSION` | `install.sh` | Pin a specific release tag (e.g. `v1.2.3`) |

## Daemon file paths

Each daemon is scoped by its absolute config path and optional `--session` name
(hashed into a stable id). The runtime directory is `$XDG_RUNTIME_DIR` when set;
otherwise proc-compose creates `proc-compose` under the OS per-user cache
directory with mode `0700`. If that directory cannot be obtained or created,
it falls back to the OS temporary directory, which may be shared and provides
no privacy guarantee.

On Unix, the paths are:

```
<runtime-dir>/pc-<hash>.sock          # IPC socket (status/monitor/restart/reload/stop talk to this)
<runtime-dir>/pc-<hash>.pid           # daemon PID file
<runtime-dir>/pc-<hash>.log           # default human log when --log-file is not set
<runtime-dir>/pc-<hash>.events.jsonl  # independent structured event journal
```

Windows uses a named pipe (`\\.\pipe\pc-<hash>`) for IPC instead of a Unix socket.

The PID file is JSON and is replaced atomically, so a reader never sees a
partial file. On Linux and macOS it also records every process group the
daemon launched and has not yet proven empty, together with the boot and
namespace identity (Linux) needed to interpret those IDs later. An entry is
written before its command starts, so if the record cannot be written the
launch fails instead of starting an unrecorded process. The exception is a
PID file that cannot be updated when the stack starts, or that is deleted
while it runs (for example when `$XDG_RUNTIME_DIR` is removed at logout),
including one that another daemon has since recreated as its own: recording
is then turned off for the rest of that run with a warning, processes keep
launching unrecorded, and the other daemon's record is left untouched. Windows records no groups; Job Objects
end managed processes together with the daemon.

`logs` queries the structured event journal, not the human `.log` file.
`up --silent --log-file PATH` redirects human output to `PATH` without disabling
`logs`, its JSON pages, or cursor resumption. If a session was started by an
older binary without a structured journal, `logs` reports that limitation and
the legacy text-log path instead. See [Sessions and logs](sessions-and-logs.md#logs-for-coding-agents).

## Common tasks

### Start all processes

```sh
proc-compose up
```

### Start specific processes

```sh
proc-compose up frontend backend
```

### Run in the background

```sh
proc-compose up --silent --log-file app.log
```

The process starts daemonized. Logs are written to `app.log`. Use `monitor` to watch it live or `stop` to shut it down. A task-only stack runs in the foreground because no service remains for the daemon to manage, so `--silent` requires at least one service process.

`up` refuses to start over a crashed run whose recorded process groups may
still be running, because starting would overwrite the only record of them.
It names those groups and asks you to run `proc-compose stop` first; `up`
itself never signals anything. A crashed run whose groups are all gone, and a
PID file without a group record, are cleaned up as before. On Linux and macOS
an unreadable PID file also refuses the start; clear it with `stop --force`.

### Monitor a running daemon

```sh
proc-compose monitor
```

Opens an interactive TUI showing process status, readiness, resource usage, and a scrollable log area:

```
proc-compose monitor               q=quit  ?=help  ↑↓/jk=nav  ⏎=filter  a=all  PgUp/Dn=scroll
  ready 3/3  tasks 1 done  failed 0  restarting 0
  PROCESS    STATUS        RESTARTS  CPU%    MEM       READY      UPTIME
▶ backend    ● running     0         12.5%    45.2MB   ready      1m32s
  frontend   ● running     0          8.1%    32.1MB   ready      1m32s
  merge-port ● running     0          0.2%     8.4MB   ready      1m32s
  migrate    ✓ completed   0          0.0%     0.0MB   done       1m32s
──────────────────────────────────────────────────────────────────────────────────────────────
Logs: all
backend    │ listening on :3001
frontend   │ VITE v5.4.2  ready in 237ms
merge-port │ merge-port is running on port 8080
```

Keys:

| Key             | Action                                |
|-----------------|---------------------------------------|
| `q` / `Ctrl+C`  | Quit                                  |
| `?` / `h`       | Toggle help overlay (Esc, Space, or any key dismisses; `q` quits) |
| `↑` / `↓` or `k` / `j` | Navigate processes             |
| `Enter`         | Toggle log filtering for the selected process |
| `a`             | Show all logs (clear filter)          |
| `PgUp` / `PgDn` | Scroll log area                       |
| `G`             | Jump to live tail                     |
| `g`             | Jump to top of log buffer             |

`NO_COLOR=1` (or `--no-color` on `up`) disables colour output in both the runner and the monitor.

Completed task rows stay visible as `✓ completed` with readiness `done`.

### Auto-restart on reboot (--survive)

To make proc-compose survive machine reboots, generate a systemd user unit:

```sh
proc-compose up --survive --name myapp --install
```

This writes a unit to `~/.config/systemd/user/proc-compose-<name>.service` and enables it.
The service starts `proc-compose up --silent` in daemon mode, which manages all processes.

To manually install without enabling immediately:

```sh
proc-compose up --survive --name myapp > ~/.config/systemd/user/proc-compose-myapp.service
systemctl --user daemon-reload
systemctl --user enable --now proc-compose-myapp
```

### Restart a single process

```sh
proc-compose restart backend
```

Sends a restart signal to the named process without affecting other running processes. The process is stopped and restarted immediately.

### Reload config

```sh
proc-compose reload
```

Re-reads the config file (`proc-compose.yml`, or `proc-compose.yaml` if you
prefer that extension) and restarts any processes whose definition has
changed (command, env, dir, etc.). Unchanged processes keep running.

If you added or removed processes, `reload` reports `partial` and lists
which names were skipped — hot-add and hot-remove aren't supported, so
restart the daemon to pick those up.

### Stop a running daemon

```sh
proc-compose stop                # graceful, 10s timeout
proc-compose stop --timeout 30   # graceful, 30s timeout
proc-compose stop --force        # force-terminate managed processes
```

`stop` asks the daemon to shut down. The daemon terminates every managed
process, including descendants, and reports whether it verified that all of
them exited. `stop` exits 0 only when the daemon verified this. When
termination could not be verified, `stop` names what it could not confirm and
exits non-zero. `--timeout` (default 10) bounds the wait for that verdict. If
the daemon cannot be reached and has to be killed directly, the final kill
can add up to 5 seconds.

When the stack declares [teardown processes](configuration.md#teardown-processes),
the daemon runs them after termination is verified (also with `--force`), and
the default `--timeout` grows by their 15-second budget; an explicit
`--timeout` is used as given. If only teardown failed, `stop` says that
termination was verified but teardown did not pass, lists each teardown line
(`teardown: <name> <status>: <reason>`), and exits non-zero.

If the daemon crashed or was killed, it never reported a verdict, so `stop`
recovers from its PID file instead. On Linux and macOS that file records each
process group the daemon launched and had not yet proven empty. Under the
session lease (if another proc-compose command holds it, `stop` fails and
signals nothing), each recorded group ends up in one of these states:

- **empty**: nothing is left in the group, the machine has rebooted since, or
  the group's ID now belongs to an unrelated process. Nothing is signalled.
- **terminated** (Linux only): the group's leader is still alive with the
  recorded start time, which proves the group is the one the daemon launched.
  Each member is signalled through a pidfd, which cannot reach a process that
  later reuses the PID. Members get SIGTERM, and whatever is left at
  `--timeout` gets SIGKILL (`--force` sends SIGKILL at once).
- **unknown**: the group cannot be proven to be the daemon's, and it is never
  signalled. This covers a group whose leader has already exited and been
  reaped while other members keep running, a launch that crashed before its
  process group existed, a record or machine whose boot ID cannot be read
  (Linux; without both IDs a reboot cannot be ruled out), pids recorded in
  another PID or time namespace (or read through a `/proc` mounted for
  another PID namespace), a kernel or sandbox without pidfds, and every live
  group on macOS (macOS has no start-time check, so it never signals).
  Processes that moved out of the group with `setsid` or `setpgid` are not in
  the record at all.

`stop` prints one line of evidence per group, for example:

```
recovered group=web pgid=912 leader_start=51301 outcome=terminated reason=leader_proven signals=SIGTERM members=912,915
recovered group=worker pgid=944 outcome=unknown reason=leader_gone live_members=1 members=950
```

`stop` exits 0 and removes the PID file only when every group is empty or
terminated. Otherwise it fails, keeps the PID file, and lists the groups that
are NOT verified. A retry checks every group again. Inspect the leftovers
(`ps -eo pid,ppid,pgid,args`), then run `stop --force`: it clears the stale
state but still exits non-zero when any group stayed unknown.

A PID file written by a daemon that records no groups (an older
proc-compose), or one that cannot be read, proves nothing. `stop` fails and
keeps it. `stop --force` clears it and warns that the managed processes were
NOT verified. It exits 0 for a readable file without groups, and non-zero for
an unreadable file, which may have recorded groups that are still running.
On Linux the PID file also records the daemon's start time, so a retry gives
the same answer even after the PID is reused. macOS has no start-time check:
once another process takes the dead daemon's PID, that process counts as the
daemon. A retried `stop`, or `stop --force`, then cannot reach it over IPC,
signals that unrelated process directly (SIGTERM and then SIGKILL; SIGKILL at
once with `--force`), and once it exits deletes the PID file with its group
record. Before retrying on macOS, check the daemon PID the failed `stop`
reported with `ps -p <PID> -o pid,args`. If it is not a proc-compose daemon,
do not run `stop` again and remove nothing blindly: inspect the leftovers
with `ps -eo pid,ppid,pgid,args` first.

On Windows, managed processes run in Job Objects that the kernel ends together
with the daemon, so a dead daemon is cleaned up and reported as stopped.

A daemon that crashed never ran its teardown processes. On every platform,
`stop` adds `teardown did not run: <names>` to its report (as a warning when
the stop itself succeeds), so the cleanup they do can be run by hand.

### Show daemon status

```sh
proc-compose status              # human-readable table
proc-compose status --json       # machine-readable, scriptable
```

`status` reads a one-shot snapshot from the running daemon and exits. The
text table includes a `MODE` column, and successful tasks appear as
`completed`. The `--json` output includes a `ready` flag per process that
distinguishes "started" from "started and passed its readiness probe". A
process with `health_when` also has `health` (`unknown`, `healthy`, or
`unhealthy`) and, while checks are failing, `health_error` with the last
failure. `ready` stays true while a service is unhealthy. Health describes the
running incarnation only: while the process is restarting, exited, or failed,
`health` is `unknown`, and the last verdict stays in the log.

With `--health-port`, `GET /health` returns 200 only when every service is
`running` or `restarting` and not `unhealthy`, and every task has `completed`;
otherwise it returns 503. Each process in its body carries `health` when it
has `health_when`. `GET /ready` reports readiness only and ignores health.

### Validate a config

```sh
proc-compose validate            # parse-only, no processes started
```

Returns non-zero if the config has any errors (unknown fields, bad
`restart` policies, circular `depends_on`, invalid regex, etc.).

### Diagnose or generate config

```sh
proc-compose doctor          # scan project and report suggestions
proc-compose doctor --write  # create proc-compose.yml when none exists
proc-compose doctor --json   # emit machine-readable report
```

`doctor` helps first-time setup and existing config debugging. Without a config file, it scans common Node, Go, and Python project layouts, then prints suggested processes, ports, readiness probes, and YAML. It understands npm/yarn/pnpm/bun package runners, common frontend dev servers (Next.js, Vite, SvelteKit, Astro), Go `cmd/**/main.go` binaries, and Python Django/Flask/Uvicorn/Hypercorn entrypoints. With an existing config, it checks paths, env files, port conflicts, readiness gaps, restart loops, merge-port setup, and likely missing binaries.

`--write` is safe by default: it only creates `proc-compose.yml` when neither `proc-compose.yml` nor `proc-compose.yaml` exists.

When you pass `-f path/to/proc-compose.yml`, `doctor` scans and resolves relative paths from that config file's directory instead of the current directory.

### Bootstrap a working setup

```sh
proc-compose bootstrap                  # dry run: show generated config
proc-compose bootstrap --write          # create proc-compose.yml when none exists
proc-compose bootstrap --write --force  # overwrite an existing generated or stale config
proc-compose bootstrap --verify         # validate generated config without writing it (dry run)
proc-compose bootstrap --write --verify # write, then verify
```

`bootstrap` builds on `doctor`: it scans the repo, proposes a config, and can validate the config through `proc-compose up --dry-run`. This checks configuration and process selection only; it does not start application processes or establish readiness. The default mode is safe and read-only. Use `--write` to create `proc-compose.yml`; it refuses to overwrite existing configs unless you also pass `--force`. Use `--verify` without `--write` to validate a temporary generated config without creating the final file; use `--write --verify` to validate the file it wrote. `--verify` exits nonzero if validation fails.

Like `doctor`, `bootstrap -f path/to/proc-compose.yml` scans and writes relative to that config file's directory.

When you do not pass `-f`, proc-compose searches upward from the current directory for `proc-compose.yml` or `proc-compose.yaml`. This lets `up`, `monitor`, `status`, `doctor`, and `bootstrap` work from nested package directories in a monorepo while still using the repository-level config.

Use `doctor` when you want diagnostics only. Use `bootstrap` when you want a first-run setup path.

### Wait for readiness when daemonising

```sh
proc-compose up --silent --log-file app.log --wait-ready --wait-timeout 60
```

Without `--wait-ready`, `up --silent` returns once the daemon has bound
its IPC socket — processes may still be starting. With `--wait-ready` the
command blocks until every started process passes its `ready_when` probe
and every task completes successfully (or `--wait-timeout` elapses). Useful
in container entrypoints and CI fixtures that need a hard guarantee before
running the next step. A task-only stack cannot daemonize because no service
remains to manage.

### Log rotation

When running with `--log-file`, use `--max-log-size` to automatically rotate logs when the file exceeds a given size. Up to 3 rotated files are kept.

```sh
proc-compose up --silent --log-file app.log --max-log-size 52428800  # 50 MB
```

### Use a different config file

```sh
proc-compose up -f services.yml
```

### List defined processes

```sh
proc-compose list
```

```
proc-compose processes

  backend    go run ./cmd/server                    (on-failure)
  frontend   npm run dev                            (never)
  merge-port merge-port --client 5173 --server ...  (on-failure)
```
