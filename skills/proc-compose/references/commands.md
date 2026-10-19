# Command reference

`proc-compose <command> --help` lists every flag of the installed version.

## Global flags

```sh
proc-compose -f /abs/path/proc-compose.yml --session feature status --json
```

| Flag | Meaning |
|---|---|
| `-f`, `--file` | Config path. Default: search for `proc-compose.yml`, then `proc-compose.yaml`, upward from the current directory. |
| `--session NAME` | Named session with its own daemon, ports, logs, and runtime directory. |
| `--version` | Print the version. On `up`, `-v` means `--verbose`. |

Global flags may also follow the command: `proc-compose stop -f myapp.yml`.

## Install and update

```sh
curl -sSL https://raw.githubusercontent.com/anivaryam/proc-compose/main/install.sh | bash   # Linux, macOS
go install github.com/anivaryam/proc-compose/cmd/proc-compose@latest                     # any OS with Go
brokit install proc-compose                                                               # also installs companions
brokit install merge-port                                                                 # needed for merge:
```

`install.sh` honors `PROC_COMPOSE_INSTALL_DIR` and `PROC_COMPOSE_VERSION`.
Interactive runs print a notice when a newer release exists; proc-compose
never updates itself.

```sh
proc-compose completion bash        # also zsh, fish, powershell
proc-compose man --dir ~/.local/share/man/man1
```

## Set up and validate

| Command | What it does |
|---|---|
| `proc-compose doctor [--write] [--json]` | Scans the project (or checks an existing config: paths, env files, port conflicts, readiness gaps, restart loops, merge-port, missing binaries). `--write` creates `proc-compose.yml` only when none exists. Alias `doc`. |
| `proc-compose bootstrap [--write] [--force] [--verify] [--json]` | Generates a config from the scan. Read-only by default. `--write` creates the file, `--force` overwrites, `--verify` validates it with a dry run (starts nothing). |
| `proc-compose init [--template minimal\|node\|go\|python]` | Writes a starter config. `node` includes merge-port. Alias `i`. |
| `proc-compose validate` | Parses and validates; non-zero on errors (unknown fields, bad policies, dependency cycles, invalid regex). Alias `check`. |
| `proc-compose config check [--json]` | Validation diagnostics with stable `code`, YAML `path`, `line`, `column`, `message`, and `hint`. Non-zero when any diagnostic is reported. |
| `proc-compose config plan [processes...] [--json]` | Static startup order, readiness kinds, endpoint references, and teardown processes. Starts nothing. |
| `proc-compose config schema` | Versioned JSON Schema for the config. |
| `proc-compose list` | Processes in the config with their restart policy. Alias `l`. |
| `proc-compose up --dry-run` | Validates and lists the processes `up` would start. |

## Run and control

### up

```sh
proc-compose up                                   # all processes, foreground; Ctrl+C stops all
proc-compose up api web                           # a subset; dependencies are added automatically
proc-compose up --silent --wait-ready --wait-timeout 90 --json
```

| Flag | Meaning |
|---|---|
| `-s`, `--silent` | Run as a background daemon. Needs at least one service. |
| `--wait-ready` | With `--silent`: block until every started process is ready and every task completed. A failure leaves the daemon running. |
| `--wait-timeout N` | Seconds for `--wait-ready` (default 60). |
| `--json` | With `--silent`: one object whose `project` has `run_id`, resolved ports and URLs, and log and journal paths. |
| `--log-file PATH`, `--max-log-size BYTES` | Human log file, rotated with up to 3 old files. Structured `logs` keep working. |
| `--log-format json`, `--no-color` | Output for log aggregators. `NO_COLOR=1` also disables color. |
| `--health-port N` | Serve `GET /health` and `GET /ready` (see [deployment](deployment.md)). |
| `-v`, `--verbose` | Debug output, including repeated readiness failures. |
| `--survive --name N [--install] [--force]` | systemd user unit for start on boot (Linux). |

`up` refuses to start over a crashed run whose process groups may still be
running; run `proc-compose stop` first.

### monitor

`proc-compose monitor` (alias `m`) opens a TUI for a running daemon: status,
restarts, CPU, memory, readiness, uptime, and logs. Keys: `j`/`k` or arrows
select, Enter filters logs to the selected process, `a` shows all, PgUp/PgDn
scroll, `G` live tail, `g` top, `?` help, `q` quit. It is interactive; agents
use `status --json` instead.

### status

`proc-compose status [--json]` (aliases `st`, `ps`) prints one snapshot. JSON
gives each process `mode`, state, `ready`, and, when configured, `health`,
`health_error`, `readiness_owner`, and `health_owner`. `ready` stays true while
a service is unhealthy.

### restart

```sh
proc-compose restart api                               # acknowledges the request only
proc-compose restart api --wait-ready --timeout 60 --json
```

`--wait-ready` waits for the new incarnation (generation) to pass readiness.
JSON: `operation`, `status` (`ok`, `partial`, `busy`, `error`), `message`,
`process`, `generation`, and `ready` (only with `--wait-ready`; `false` is not
a pass). Tasks cannot be restarted. Alias `r`.

### reload

`proc-compose reload [--json]` (alias `rl`) re-reads the config and restarts
only processes whose definition changed. Added or removed processes are
skipped and the result is `partial` (exit 0, so check `status`); mode or
dependency changes that need a full restart are rejected. Endpoint leases are
kept. It does not wait for readiness.

### stop

```sh
proc-compose stop                   # graceful, waits up to 10 s for a verified shutdown
proc-compose stop --timeout 30
proc-compose stop --force           # force-terminate; clears a crashed daemon's state
```

Exit 0 only when every managed process, including descendants, is verified
gone. Otherwise it names what it could not confirm and exits non-zero. With
teardown processes, the default timeout grows by their 15 second budget. After
a daemon crash, `stop` recovers from the PID file and prints one `recovered
group=...` line per process group; groups it cannot prove are never signalled.
Inspect leftovers with `ps -eo pid,ppid,pgid,args` before `--force`. On macOS
check the reported daemon PID with `ps -p <PID> -o pid,args` before retrying.
On Windows `stop` always force-kills the tree (`taskkill /T /F`).

### uninstall

`proc-compose uninstall --name myapp` (alias `un`) removes the systemd unit
that `up --survive --install` created. Linux only.

## Test and diagnose

| Command | What it does | Details |
|---|---|---|
| `proc-compose verify [processes...] -- PROGRAM [ARG...]` | Fresh owned session, readiness, one check, verified cleanup, receipt | [testing](testing.md#verify) |
| `proc-compose attempts [id\|latest] [--json]` | Verification receipts for this session, newest first | [testing](testing.md#receipts-and-attempt-evidence) |
| `proc-compose compare --baseline DIR --candidate DIR -- PROGRAM` | Same check against two worktrees | [testing](testing.md#compare) |
| `proc-compose logs [processes...]` | Bounded, structured event queries; `--follow` streams | [sessions and logs](sessions-and-logs.md#logs) |
| `proc-compose why <process> \| --attempt <id\|latest>` | Bounded failure evidence | [sessions and logs](sessions-and-logs.md#why) |
| `proc-compose sessions [--live] [--json]` | Every session: owner, worktree, URLs, latest attempt, stop command | [sessions and logs](sessions-and-logs.md#sessions) |
| `proc-compose agent-guide` | Offline operating guide for coding agents, matched to this binary | |

## JSON output and exit codes

- Commands with `--json` print one JSON object (or newline-delimited pages for
  `logs --follow --json`). Flag-parsing errors still print plain text on
  stderr with empty stdout, so check the exit code and stderr before decoding.
- `verify`: 0 passed, 1 failed, 2 nothing started (usage, invalid config,
  occupied session), 130 interrupted.
- `compare`: 0 comparable (both sides started, ran the check, and cleaned
  up; the checks may still have failed or differ), 1 not comparable, 2 usage,
  130 interrupted.
- `restart --json` and `reload --json`: read `status`; a `partial` reload
  exits 0.

## Environment variables

| Variable | Effect |
|---|---|
| `NO_COLOR` | Disable ANSI color in `up` and `monitor`. |
| `PORT` | Used as `merge.port` when it is not set (cloud platforms). |
| `XDG_RUNTIME_DIR` | Runtime directory for sockets, PID files, journals, and receipts; otherwise the per-user cache directory. |
| `PROC_COMPOSE_OWNER` | Owner label shown by `sessions`. |
| `PROC_COMPOSE_INSTALL_DIR`, `PROC_COMPOSE_VERSION` | `install.sh` target directory and release tag. |

Runtime files are `<runtime-dir>/pc-<hash>.sock` (a named pipe on Windows),
`.pid`, `.log`, and `.events.jsonl`, where the hash comes from the absolute
config path and session name. They may disappear at logout or reboot. Never
delete them by hand.
