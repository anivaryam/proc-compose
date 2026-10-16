# Features

A short tour of what proc-compose does, with a minimal example of each. Every
section links to the full reference.

## One config, one log stream

Declare every process in `proc-compose.yml` and start them all with
`proc-compose up`. Output from every process is interleaved into one stream
with stable, color-coded name prefixes. Ctrl+C stops the whole stack.

```yaml
processes:
  api:
    cmd: go run ./cmd/server
  web:
    cmd: npm run dev
    dir: ./web
```

Commands run through the platform shell (`sh -c` on Linux and macOS, `cmd /c`
on Windows), in their own process group or Job Object. See
[Configuration](docs/configuration.md).

## Services and tasks

Services are long-running. Tasks run once, for migrations, seeding, or code
generation. Dependents of a task start only after it exits 0, and a failed task
fails the stack.

```yaml
processes:
  migrate:
    mode: task
    cmd: npm run migrate
  api:
    cmd: npm run dev
    depends_on: [migrate]
```

See [Process modes](docs/configuration.md#process-modes).

## Ordered startup and readiness

`depends_on` holds a process until its dependencies are ready. Readiness comes
from an `http` (2xx response), `tcp` (connect succeeds), `cmd` (command exits
0), or `log` (regex match) probe, with a configurable `ready_timeout`. After
that, an optional `health_when` probe (`http`, `tcp`, or `cmd`) keeps checking
the service and either reports it unhealthy or restarts it. On Linux and
Windows, each `http` or `tcp` probe also records who answered it, so a server
from another copy of the stack on the same port is named instead of silently
counting as ready, together with the other live session it belongs to, if
any; `foreign_listener: reject` makes such an answer not count, and `verify`
refuses one from another live session by default.

```yaml
processes:
  db:
    cmd: postgres -D ./data
    ready_when:
      tcp: localhost:5432
  api:
    cmd: npm start
    depends_on: [db]
    ready_when:
      http: http://localhost:3000/health
```

See [Readiness probes](docs/configuration.md#readiness-probes),
[Who answered the probe](docs/configuration.md#who-answered-the-probe) and
[Health checks](docs/configuration.md#health-checks).

## Restart policies

`never`, `on-failure`, or `always`, with exponential backoff from 1 to 30
seconds and an optional `max_restarts` limit.

```yaml
processes:
  worker:
    cmd: python worker.py
    restart: on-failure
    max_restarts: 5
```

See [Restart policies](docs/configuration.md#restart-policies).

## Verified shutdown

On stop, proc-compose signals each whole process tree, escalates to a forced
kill after `shutdown_timeout`, and reports success only after termination is
confirmed. Processes that escape their group are reported, never assumed gone.
See [Platform support](docs/platforms.md).

## Background daemon and monitor

Run the stack in the background and control it from any terminal in the
project:

```sh
proc-compose up --silent --wait-ready
proc-compose monitor          # live TUI with status, CPU, memory, and logs
proc-compose status --json
proc-compose restart api
proc-compose reload           # restart only processes whose config changed
proc-compose stop
```

On Linux, `up --survive --install` generates a systemd user unit so the stack
comes back after a reboot. See the [CLI reference](docs/cli.md).

## Named sessions and ports

Run several isolated copies of the same stack, for example one per branch or
worktree. Each session has its own daemon, logs, runtime directory, and port
assignments.

```yaml
processes:
  api:
    cmd: node server.js
    ports:
      http: 3000          # advances to a free port if 3000 is taken
  web:
    cmd: npm run dev
    env:
      API_URL: '${urls.api.http}'
```

```sh
proc-compose --session feature up --silent --wait-ready --json
```

See [Sessions and logs](docs/sessions-and-logs.md).

## Structured logs and failure evidence

Every output line and lifecycle event is recorded in a rotating JSON-lines
journal. Query it in bounded pages with resumable cursors and filters, or ask
`why` for a compact evidence packet about a failure.

```sh
proc-compose logs api --json --level error --max-bytes 4096
proc-compose logs --json --after '<next_cursor>'
proc-compose why api --json
```

See [Sessions and logs](docs/sessions-and-logs.md#logs-for-coding-agents).

## Verified runs

`verify` turns "start the stack, run the tests, clean up" into one operation
with a receipt. Startup, check, and cleanup each report their own status, so a
passing test cannot hide a crashed service or a leaked process.

```sh
proc-compose verify --json -- npm run test:smoke
proc-compose attempts latest --json
```

See [Verified runs](docs/verify.md).

## Project scanning

`doctor` checks an existing config, or scans a project without one. It
recognizes Node, Go, and Python layouts. `bootstrap` turns the scan into a
working config.

```sh
proc-compose doctor
proc-compose bootstrap --write --verify
```

See [Diagnose or generate config](docs/cli.md#diagnose-or-generate-config).

## Single-port proxying with merge-port

A `merge:` section adds a [merge-port](https://github.com/anivaryam/merge-port)
proxy that serves the frontend and backend on one port, wires startup order, and
injects the API URL into the frontend's environment.

```yaml
merge:
  client: 5173
  server: 3001
```

See [merge-port integration](docs/configuration.md#merge-port-integration).

## Built for coding agents

`proc-compose agent-guide` prints an offline guide matched to the installed
version, covering session ownership, JSON contracts, cursors, and cleanup.
JSON output, bounded queries, and explicit exit codes make every command safe
to script.
