# Configuration

The config file is `proc-compose.yml` by default; if that's missing,
proc-compose falls back to `proc-compose.yaml` automatically. Pass
`-f path/to/file` to use any other path. Each process has the following fields:

```yaml
processes:
  name:
    cmd: "command to run"        # required, passed to the platform shell
    mode: service                # optional, service or task
    dir: "./working/directory"   # optional — working directory
    env_file: ".env.backend"     # optional — load env vars from file
    env:                         # optional — extra environment variables
      KEY: "value"
    restart: "never"             # optional — restart policy
    max_restarts: 5              # optional — give up after N restarts
    shutdown_timeout: 30         # optional — seconds before SIGKILL
    ready_when:                  # optional — readiness probe
      http: http://localhost:3000/health
    ready_timeout: 60            # optional — seconds to wait for readiness
    depends_on:                  # optional — wait for these processes
      - db
```

## Process Fields

| Field | Required | Default | Description |
|-------|----------|---------|-------------|
| `cmd` | Yes | — | Shell command to run. Linux/macOS use `sh -c`; Windows uses `cmd /c`. |
| `mode` | No | `service` | Process mode: `service` for long-running processes, `task` for one-shot work |
| `dir` | No | `.` | Working directory for the process; relative paths are resolved from the config file's directory, not the invocation CWD |
| `env_file` | No | — | Path to `.env` file; values are merged before `env` block |
| `env` | No | — | Extra environment variables (merged with system env; overrides `env_file`) |
| `restart` | No | `never` | Restart policy: `never`, `on-failure`, or `always` |
| `max_restarts` | No | `0` (unlimited) | Stop restarting after N restarts; process enters "failed" state |
| `shutdown_timeout` | No | `5` | Seconds to wait for graceful shutdown before SIGKILL |
| `ready_when` | No | — | Readiness probe (see below) |
| `ready_timeout` | No | `60` | Seconds to wait for readiness before failing (`-1` = no limit) |
| `depends_on` | No | — | List of process names to wait for before starting |

## Process Modes

Every process is either a **service** (long-running) or a **task** (one-shot).
The default is `service`. Set `mode: task` for migrations, seeding, codegen,
prestart build steps, or any command meant to run once and exit cleanly.

```yaml
processes:
  migrate:
    mode: task
    cmd: npm run migrate
  api:
    cmd: npm run dev
    depends_on: [migrate]   # api waits for migrate to exit 0
```

Differences:

| Aspect | Service (`mode: service`) | Task (`mode: task`) |
|--------|---------------------------|---------------------|
| Lifecycle | Long-running; expected to stay up | Runs once, exits |
| Ready when | Started (or `ready_when` passes) | Exits with code `0` |
| `ready_when` | Allowed (`http` / `tcp` / `log`) | Not allowed |
| `ready_timeout` | Allowed (default 60s, `-1` = none) | Not allowed |
| `restart` | `never`, `on-failure`, `always` | Must be `never` |
| `max_restarts` | Allowed when `restart != never` | Not allowed |
| As a dependency | Dependents start once it's ready | Dependents start once it exits 0 |
| Failure | Exit triggers restart per policy | Non-zero exit fails the stack |
| Status | `running`, `restarting`, `failed` | `completed` (✓) or `failed` |
| `up --silent` | Counts as a managed process | Daemon refuses a task-only stack |

`up --silent` requires at least one service in the started closure —
the daemon has nothing to manage once every task has exited, so
task-only stacks always run in the foreground.

The monitor's status line shows `tasks N done`; completed task rows
stay visible as `✓ completed` with readiness `done`. `status` and
`status --json` include a `MODE` column / `mode` field so scripts can
treat tasks differently from services.

## Restart Policies

Restart policies apply to service processes. Task processes always run once
and must use `restart: never`.

| Policy | Behavior |
|--------|----------|
| `never` | Process runs once. If it exits, it stays dead. |
| `on-failure` | Restarts if the process exits with a non-zero code. |
| `always` | Restarts on any exit, including clean exits. |

On restart, proc-compose uses exponential backoff starting at 1 second, doubling up to a max of 30 seconds. The backoff resets to 1 second after a process runs successfully for longer than the current backoff interval. Use `max_restarts` to cap the number of retries:

```yaml
processes:
  backend:
    cmd: npm run start
    restart: on-failure
    max_restarts: 5   # give up after 5 restarts, exit non-zero
```

## Readiness Probes

Use `ready_when` to define how proc-compose detects when a process is ready to accept traffic. Three probe types are supported:

```yaml
processes:
  backend:
    cmd: npm run start
    ready_when:
      http: http://localhost:3000/health   # GET returns 2xx

  db:
    cmd: postgres -D /data
    ready_when:
      tcp: localhost:5432                  # TCP connect succeeds

  worker:
    cmd: python worker.py
    ready_when:
      log: "Worker ready"                 # regex matched in stdout
```

Probes poll every 3 seconds until success or process exit. The default readiness deadline is 60 seconds — set `ready_timeout` to override (`-1` disables the limit). Without `ready_when`, a service is considered ready as soon as it starts. Tasks cannot use `ready_when` or `ready_timeout`; they become ready only after they exit successfully.

## Startup Dependencies

Use `depends_on` to control process startup order. A process waits until all its dependencies are ready (as determined by their `ready_when` probe) before starting:

```yaml
processes:
  db:
    cmd: postgres -D /data
    ready_when:
      tcp: localhost:5432
  backend:
    cmd: npm run start
    depends_on:
      - db
    ready_when:
      http: http://localhost:3000/health
```

When a `merge:` section is present, proc-compose auto-injects `depends_on` on `merge-port` for every upstream process so you don't need to wire it manually — see [merge-port Integration](#merge-port-integration) below.

If a dependency fails before becoming ready, dependent processes also fail.

Task dependencies are useful for one-time setup before services start:

```yaml
processes:
  migrate:
    mode: task
    cmd: npm run migrate
  api:
    cmd: npm run dev
    depends_on: [migrate]
    ready_when:
      http: http://localhost:3000/health
```

With `mode: task`, the command runs once. Exit code `0` marks the process
`completed`, sets `ready: true`, and lets dependents start. A non-zero exit
fails the stack and stops running processes. Tasks can't use `ready_when`,
`ready_timeout`, restart policies other than `never`, or `max_restarts`.

## Environment File

Use `env_file` to load environment variables from a `.env` file. The `env` block overrides any conflicting keys from `env_file`:

```yaml
processes:
  backend:
    cmd: npm run start
    env_file: ./server/.env
    env:
      PORT: "3000"  # overrides PORT from .env file
```

## merge-port Integration

The optional `merge` section auto-injects a [merge-port](https://github.com/anivaryam/merge-port) process to combine your client and server into a single port.

**Simple mode** — `api_prefixes` are auto-detected by scanning your server source files for top-level route registrations (`app.use`, `app.get`, etc.). You usually don't need to list them manually:

```yaml
merge:
  client: 5173
  server: 3001
```

**Explicit API prefixes** (override auto-detection, or when your backend serves non-standard prefixes):

```yaml
merge:
  client: 5173
  server: 5000
  api_prefixes:
    - /api
    - /health
    - /uploads
```

**Route mode** (multiple backends on different ports):

```yaml
merge:
  routes:
    - /api=3001
    - /auth=3002
    - /=3000
```

| Field | Default | Description |
|-------|---------|-------------|
| `client` | — | Client/frontend port (simple mode) |
| `server` | — | Server/backend port (simple mode) |
| `port` | `$PORT` → `8080` | Proxy listen port — reads `$PORT` env var if not set, then falls back to `8080` |
| `api_prefixes` | `[/api]` | Path prefixes routed to server (simple mode) |
| `routes` | — | Explicit `prefix=target` routes (route mode) |
| `client_process` | auto | Override the process name treated as the client (for env auto-injection) |

`routes` cannot be combined with `client`, `server`, or `api_prefixes`.

Auto-detection of `api_prefixes` only matches top-level Express/Koa/Fastify/Hono handlers (`app.use`, `server.get`, etc.) and Go chi/gorilla/`net/http` handlers (`Route`, `Mount`, `Handle`, `HandleFunc`). Sub-router files and other frameworks (Django, Rails, Flask, etc.) need explicit `api_prefixes`.

### Startup ordering and readiness

When a `merge:` section is present, proc-compose wires the upstream ports automatically so the proxy never starts before its backends are accepting connections:

- Each upstream process (`client`/`server` in simple mode; every backing process in route mode) gets a `ready_when: tcp localhost:<port>` probe — but only when you haven't supplied your own `ready_when`. User probes (`http`, `log`) always win.
- The injected `merge-port` process gets `depends_on` set to every upstream, so the runner blocks `merge-port` startup until the probes pass.

The upshot: a fresh `proc-compose up` on the simple example below boots `client` and `server`, waits for both to bind their ports, *then* starts `merge-port`. Requests that hit `:8080` after the daemon prints "ready" can never land on an unbound upstream.

Override the auto-detected client process with `merge.client_process` (also used by the env-injection step). Upstream ports that don't match any managed process are left alone — proc-compose won't fabricate dependencies on external services.

Before starting, proc-compose always checks that the merge-port proxy output port (`merge.port`, or `$PORT`/`8080`) is free. It also checks upstream ports owned by managed processes — ownership is determined by a process `env.PORT` matching `merge.client`, `merge.server`, or a `merge.routes` target port. Managed upstream ports must be free because proc-compose is about to start those processes. Upstream ports with no matching managed process are treated as external services and may already be listening; proc-compose does not reject those occupied external targets.

### Client API URL auto-injection

When a `merge:` section is present, proc-compose looks at the client process's `.env.example` (or `.env`) and, for any of these well-known frontend variables present in the file, injects the merge-port URL into the client's environment:

`VITE_API_BASE_URL`, `VITE_API_URL`, `VITE_SERVER_URL`, `REACT_APP_API_URL`, `REACT_APP_BASE_URL`, `REACT_APP_API_BASE_URL`, `NEXT_PUBLIC_API_URL`, `NEXT_PUBLIC_API_BASE_URL`, `NUXT_PUBLIC_API_BASE`, `PUBLIC_API_URL`.

The path suffix from the example value is preserved (e.g. `http://1.2.3.4:3000/api` → `http://localhost:8080/api`). Anything you set explicitly under the process's `env:` block always wins. The client process is detected by matching `merge.client` against each process's `PORT` env var, or by name (`client`, `frontend`, `web`, `ui`, `app`); set `merge.client_process` to override.
