# Sessions and logs

## Sessions

A session is one running copy of a stack. Its identity is the absolute config
path plus an optional `--session` name. Each session has its own daemon,
runtime directory, event journal, receipts, and port assignments, so one
checkout can run several copies, and each worktree runs its own.

```sh
proc-compose --session main up --silent --wait-ready --json
proc-compose --session feature up --silent --wait-ready --json
proc-compose --session feature status --json
proc-compose --session feature restart worker --wait-ready
proc-compose --session feature stop
```

### Named ports and references

```yaml
processes:
  api:
    cmd: node server.js
    ports:
      http: 3000               # preferred; the next free port if taken; 0 = ephemeral
    ready_when:
      http: '${urls.api.http}/health'
  web:
    cmd: npm run dev
    depends_on: [api]
    ports:
      http: 5173
    env:
      API_URL: '${urls.api.http}'
      UPLOAD_ROOT: '${session.dir}/uploads'
    ready_when:
      http: '${urls.web.http}'
```

- An `http` port exports `PORT` unless `env` sets it, so most dev servers
  pick it up unchanged.
- `${ports.P.N}` is the number, `${urls.P.N}` its `http://127.0.0.1` URL;
  `${session.name}`, `${session.id}`, `${session.dir}`, and `${config.dir}`
  are also available. Without `dir`, named-session commands run from the
  config directory.
- Assigned ports are leased among proc-compose sessions. An unrelated program
  can still take the port first; readiness probes and probe owners catch that.
- Hardcoded ports (in `cmd`, `env`, or probe URLs) are shared by every copy.
  Replace them with references before running copies side by side.

### Shared resources

Separate ports do not isolate databases, queues, uploads, or build outputs.
Give each session its own state (for example under `${session.dir}`, created
by a setup task), or declare what must not be shared:

```yaml
resources:
  - 'postgresql:local:role-ddl'
  - 'next-build:${config.dir}/apps/web'
```

Sessions in the same runtime directory hold these leases for their whole
lifetime. A second session declaring the same label fails before any command
starts.

### Discover sessions

```sh
proc-compose sessions                 # every session: owner, worktree, URLs, latest attempt, stop command
proc-compose sessions --live --json   # only sessions whose owner process is running
```

`sessions` only reads metadata. Set `PROC_COMPOSE_OWNER` when starting a
session to label who owns it; the label describes, it does not authorize.

### Ownership workflow for agents

1. Use one absolute `-f` and one `--session` for the whole task.
2. Before starting, check `sessions --json` and `status --json`. A failed
   status query does not prove the session is free. If the session belongs to
   someone else, choose a new name; never stop theirs to make room.
3. Start with `up --silent --wait-ready --wait-timeout 60 --json` and record
   `project.config_path`, `project.run_id`, and `project.urls`.
4. Before every restart, reload, or stop, recheck that the run is still yours.
5. Stop with `stop --timeout 30` on every exit path and check its verdict. If
   the user wants the stack kept, hand over the config path, session, run ID,
   URLs, and stop command instead.

## Logs

Every output line, lifecycle change, readiness result, and cleanup verdict is
an event in a rotating JSON-lines journal per session. `logs` queries that
journal, even when human output went to `--log-file`.

```sh
proc-compose logs api -n 50                                      # human rows, newest 50
proc-compose logs api --json --max-bytes 8192 -n 30              # one bounded JSON page
proc-compose logs --json --level error                           # errors from every process
proc-compose logs --json --after start -n 100                    # oldest first
proc-compose logs --json --after '<next_cursor>'                 # resume
proc-compose logs worker --generation 2 --stream stderr --json   # one incarnation's stderr
proc-compose logs worker --stream stderr --since 5m
proc-compose logs --follow                                       # until interrupted (humans)
```

| Filter | Values |
|---|---|
| processes / `--process` | Names, comma-separated or repeated |
| `--level` | `debug`, `info`, `warn`, `error`, `fatal` |
| `--kind` | `output`, `lifecycle`, `state`, `session`, `probe` |
| `--code` | A supervisor code such as `readiness.failed` |
| `--stream` | `stdout`, `stderr`, `supervisor` |
| `--search` | Case-insensitive substring |
| `--since` | Duration (`5m`) or RFC3339 time |
| `--run`, `--generation` | One daemon run or one process incarnation |
| `-n`, `--max-bytes` | Page size: default 50 events and 64 KiB; maximum 1000 and 1 MiB. `-n 0` pages through all retained history. |
| `--after` | `start`, or a `next_cursor` |

- A JSON page has `schema_version`, `events`, `next_cursor`, `has_more`,
  `gap` (cursor fell out of rotated history; you got a bounded tail),
  `truncated`, and `event_bytes`. Save `next_cursor`, not the last event ID,
  and keep the same filters when resuming.
- An event has `id`, `run_id`, `sequence`, `session`, `time`, `process`,
  `generation`, `pid`, `kind`, `level`, `code`, `stream`, and `message`.
  Identify an incarnation by `(run_id, process, generation)`, not by PID.
- Plain stderr is `stream: stderr`, not automatically `level: error`; an
  application's own JSON `level` or `severity` is normalized.
- An empty page is not proof of health.
- Supervisor codes include `readiness.failed`, `readiness.progress`,
  `readiness.foreign_listener`, `process.failed`, `process.restarting`,
  `process.state`, `shutdown.verified`, `health.check_failed`,
  `health.unhealthy`, `health.recovered`, `health.check_skipped`,
  `health.foreign_listener`, `teardown.started`, `teardown.completed`,
  `teardown.failed`, and `teardown.skipped`.
- `--follow --json` and `-n 0 --json` print newline-delimited pages.
  Interrupting `--follow` does not stop the session.

## why

```sh
proc-compose why api --json                  # live or retained evidence for one process
proc-compose --session feature why worker
proc-compose why --attempt latest --json     # one verify attempt, its own events only
```

`why` returns a bounded evidence packet: observed state, exit code, last
error, unresolved dependencies (`blocked_by`), and recent relevant events.
`source` is `live` or `retained`; retained evidence is history after a stop,
not current liveness. It reports observed failures and never infers
business-level causes from log text.
