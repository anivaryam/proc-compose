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
proc-compose stop [--timeout N] [--force]               Stop daemon gracefully (or SIGKILL)
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
      --timeout int        Seconds to wait for graceful shutdown before SIGKILL (default 10)
      --force              SIGKILL immediately without graceful shutdown

Flags (init):
      --template string    Starter template: minimal (default), node, go, python

Flags (logs): see `proc-compose logs --help` and [Sessions and logs](sessions-and-logs.md)
```

## Environment variables

| Variable | Read by | Effect |
|----------|---------|--------|
| `NO_COLOR` | runner, monitor | Any non-empty value disables ANSI colour output |
| `PORT` | merge-port config | Used as `merge.port` when not set explicitly (cloud platforms) |
| `XDG_RUNTIME_DIR` | daemon | Base directory for socket / PID / log files; falls back to `$TMPDIR` |
| `PROC_COMPOSE_INSTALL_DIR` | `install.sh` | Override install target (default `~/.local/bin`) |
| `PROC_COMPOSE_VERSION` | `install.sh` | Pin a specific release tag (e.g. `v1.2.3`) |

## Daemon file paths

Each daemon is scoped per absolute config path (hashed into a stable id):

```
$XDG_RUNTIME_DIR/pc-<hash>.sock   # IPC socket (status/monitor/restart/reload/stop talk to this)
$XDG_RUNTIME_DIR/pc-<hash>.pid    # daemon PID file
$XDG_RUNTIME_DIR/pc-<hash>.log    # default log file when --log-file is not set
```

`logs` reads the default `.log` path. If you ran `up --silent --log-file PATH`, the daemon writes to `PATH` instead and `logs` will report "no log file found" — read `PATH` directly.

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

### Monitor a running daemon

```sh
proc-compose monitor
```

Opens an interactive TUI showing process status, readiness, resource usage, and a scrollable log area:

```
proc-compose monitor               q=quit  ?=help  ↑↓/jk=nav  ⏎=filter  a=all  PgUp/Dn=scroll
  ready 3/3  tasks 1 done  failed 0  restarting 0
  PROCESS    STATUS        RESTARTS  CPU%    MEM       READY     UPTIME
▶ backend    ● running     0         12.5%    45.2MB   ready     1m32s
  frontend   ● running     0          8.1%    32.1MB   ready     1m32s
  merge-port ● running     0          0.2%     8.4MB   ready     1m32s
  migrate    ✓ completed   0          0.0%     0.0MB   done      1m32s
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
proc-compose stop                # graceful, 10s timeout, then SIGKILL
proc-compose stop --timeout 30   # graceful, 30s timeout
proc-compose stop --force        # SIGKILL immediately
```

`stop` sends SIGTERM and waits up to `--timeout` seconds (default 10) for
the daemon to exit cleanly. If the deadline passes the process is force-killed.

### Show daemon status

```sh
proc-compose status              # human-readable table
proc-compose status --json       # machine-readable, scriptable
```

`status` reads a one-shot snapshot from the running daemon and exits. The
text table includes a `MODE` column, and successful tasks appear as
`completed`. The `--json` output includes a `ready` flag per process that
distinguishes "started" from "started and passed its readiness probe".

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
