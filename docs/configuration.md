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
    health_when:                 # optional — ongoing health check
      http: http://localhost:3000/health
    health_interval: 10          # optional — seconds between health checks
    health_action: report        # optional — report or restart
    foreign_listener: allow      # optional — allow or reject another process's answer
    depends_on:                  # optional — wait for these processes
      - db
```

## Process Fields

| Field | Required | Default | Description |
|-------|----------|---------|-------------|
| `cmd` | Yes | — | Shell command to run. Linux/macOS use `sh -c`; Windows uses `cmd /c`. |
| `mode` | No | `service` | Process mode: `service` for long-running processes, `task` for one-shot work, `teardown` for cleanup after the stack stops (see [Teardown Processes](#teardown-processes)) |
| `dir` | No | `.` | Working directory for the process; relative paths are resolved from the config file's directory, not the invocation CWD |
| `env_file` | No | — | Path to `.env` file; values are merged before `env` block |
| `env` | No | — | Extra environment variables (merged with system env; overrides `env_file`) |
| `restart` | No | `never` | Restart policy: `never`, `on-failure`, or `always` |
| `max_restarts` | No | `0` (unlimited) | Stop restarting after N restarts; process enters "failed" state |
| `shutdown_timeout` | No | `5` | Seconds to wait for graceful shutdown before SIGKILL |
| `ready_when` | No | — | Readiness probe (see below) |
| `ready_timeout` | No | `60` | Seconds to wait for readiness before failing (`-1` = no limit) |
| `health_when` | No | — | Ongoing health check after readiness: `http`, `tcp`, or `cmd` (see [Health Checks](#health-checks)) |
| `health_interval` | No | `10` | Seconds between health checks; `0` uses the default; requires `health_when` |
| `health_action` | No | `report` | `report` marks the service unhealthy; `restart` restarts it through `restart`; requires `health_when` |
| `foreign_listener` | No | `allow` | What an `http` or `tcp` probe does with an answer from a process outside this process's tree: `allow` passes and reports it; `reject` fails the attempt (see [Who answered the probe](#who-answered-the-probe)); requires an `http` or `tcp` `ready_when` or `health_when` |
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
| `ready_when` | Allowed (`http` / `tcp` / `cmd` / `log`) | Not allowed |
| `ready_timeout` | Allowed (default 60s, `-1` = none) | Not allowed |
| `health_when` and its fields | Allowed (`http` / `tcp` / `cmd`) | Not allowed |
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

## Teardown Processes

A process with `mode: teardown` releases what the stack held: test uploads,
temporary buckets, rows a run created. It never starts with the stack. It runs
once the stack has stopped and every managed process is verified terminated,
while the session's ports and resource leases are still held.

```yaml
processes:
  api:
    cmd: npm start
  cleanup-uploads:
    mode: teardown
    cmd: node scripts/cleanup-uploads.mjs
    env_file: .env.test.local
```

- **When it runs:** after `stop`, `stop --force`, Ctrl+C, a foreground stack
  that ends on its own, and a stack stopped by a failed task. It runs for the
  whole stack, also when `up` started only some processes.
- **Order and budget:** teardown processes run one at a time, in name order,
  within a fixed budget of 15 seconds for all of them. That includes each
  one's `shutdown_timeout` (at most 5): a teardown process still running when
  its share runs out is terminated like a managed process, and one that cannot
  start before the budget ends is reported as not run.
- **Never beside running processes:** if any managed process could not be
  verified terminated, every teardown process is skipped and reported, so
  nothing is released while something may still use it. If the daemon itself
  crashes, teardown does not run.
- **Environment:** it gets `cmd`, `dir`, `env` and `env_file` like any process,
  the session variables (`PROC_COMPOSE_SESSION`, `PROC_COMPOSE_SESSION_DIR`,
  `PROC_COMPOSE_RUN_ID`) and `${ports...}`, `${urls...}` and `${session...}`
  references.
- **Restrictions:** it cannot use `ready_when`, `ready_timeout`, `restart`
  (other than `never`), `max_restarts`, `health_when`, `health_interval`,
  `health_action`, `ports` or `depends_on`, and no process can depend on it.
  It cannot be named as a process to start in `up` or `verify`.
- **Result:** its output is logged under its name, and the journal records
  `teardown.started`, `teardown.completed`, `teardown.failed` and
  `teardown.skipped`. A teardown process that does not pass makes the stop
  verdict and `up` fail with a line starting `teardown:`; that does not change
  whether the managed processes were verified terminated. `config plan` lists
  teardown processes under `teardown`.

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

Use `ready_when` to define how proc-compose detects when a process is ready to accept traffic. Set exactly one of four probe types:

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

  queue:
    cmd: node queue.js
    ready_when:
      cmd: node queue.js --check           # command exits 0
```

A `cmd` probe suits a process that opens no port and prints no reliable ready
line. The command runs through the same shell as `cmd`, in the process's `dir`,
with its environment (`env_file` and `env`) and the same `${...}` references.
proc-compose runs it again one second after each failed attempt, and stops an
attempt that runs longer than 10 seconds or past `ready_timeout`. Probe runs are
not processes of the stack: their state and output do not appear in `status`,
the monitor or the logs. They are stopped with the process they probe, and a
probe run that cannot be terminated is reported like any other managed process.
With `--verbose`, each failed attempt after the fifth consecutive failure is
logged as a `readiness.progress` event with its exit status and the start of
its stderr. If `ready_timeout` passes first, the `readiness.failed` error and
the process's last error name the last completed attempt's exit status and
stderr, so they are visible without `--verbose`.

`http` and `tcp` probes make their first attempt as soon as the process starts and then retry about once a second until success or process exit. The default readiness deadline is 60 seconds — set `ready_timeout` to override (`-1` disables the limit). Without `ready_when`, a service is considered ready as soon as it starts. Tasks cannot use `ready_when` or `ready_timeout`; they become ready only after they exit successfully.

### Who answered the probe

An `http` or `tcp` probe passes when *any* process answers at its address. A
server already listening there, such as another worktree's dev server, a stale
run or a manual `npm run dev`, makes the new process ready on the first
attempt, before it has bound anything. This happens most with hardcoded ports:
two copies of a stack that both use `http://localhost:3000/health` probe the
same server. Named [ports](sessions-and-logs.md) avoid it.

On Linux and Windows, proc-compose records who answered each `http` or `tcp`
readiness probe and health check that got an answer. Every attempt opens its
own connection (probes do not reuse connections), and as soon as it is
connected, before the request is sent, proc-compose finds the server side of
that connection and the process holding it: on Linux in the kernel's socket
table (`/proc/net/tcp` and `tcp6`) and `/proc/<pid>/fd`, on Windows in the TCP
table, which names each socket's owning process. Doing this before the server
can answer means a server that answers and then hands its port to another
process is still the one named. A connection the server has not accepted yet
is attributed to the listener whose queue holds it. proc-compose then
compares the holder with the incarnation's process tree: the process group on
Linux, the Job Object on Windows.

| Result | Meaning |
|---|---|
| `owned` | A process in the incarnation's own process tree holds the socket. `pid` and `command` name it. |
| `foreign` | A process outside that tree, running as the same user as proc-compose, holds it. `pid` and `command` (its process name) name it. |
| `unattributable` | The holder could not be identified, or may not be named. `reason` says why: another user's process, which is never named (for example a root-owned `docker-proxy`, or a service on Windows); a server that is not in this network namespace (a remote host, a container address, a NAT rule), where a local process on the same port is never taken for it; on Windows a process that cannot be opened, such as the System process behind `http.sys`; on macOS always, because sockets are not attributed there yet. |

The result appears as `readiness_owner` and `health_owner` in `status --json`,
as `owner` on each `verify` readiness observation, and, when it is `foreign`,
as a `readiness.foreign_listener` or `health.foreign_listener` warning event.
The health warning is logged when the answer becomes foreign or changes to
another foreign holder, not on every check. Like `last_error`,
`readiness_owner` describes the latest incarnation until the next one starts;
`health_owner` is shown only while the process runs.

By default (`foreign_listener: allow`) attribution only reports. It does not
change readiness, health, `depends_on`, `--wait-ready` or the `verify` verdict.
It never signals the processes it names, and it reads only their process
name, not their command line, environment or working directory.

Set `foreign_listener: reject` to refuse foreign answers:

```yaml
processes:
  web:
    cmd: npm run dev
    ready_when:
      http: http://localhost:5173/
    foreign_listener: reject
```

With `reject`, a probe attempt answered by a `foreign` process counts as a
failed attempt. Readiness keeps polling, so `depends_on`, `up --wait-ready` and
`verify` wait for the process's own server. If it never answers,
`ready_timeout` fails the process with an error that names the holder, for
example `readiness probe (http) timed out (last attempt: answered at
127.0.0.1:5173 by PID 4242 (node), outside web's process tree)`. The first
refusal is logged as a `readiness.foreign_listener` warning, and the refused
answer is kept as `readiness_owner`, so it is still there when the process
fails for another reason, such as its own bind failing. A health check
answered by a `foreign` process counts as a failed check, with the holder in
`health_error`. An `unattributable` answer is never refused, so on macOS
`reject` changes nothing. The same holds for a port published by rootful
Docker: its `docker-proxy` runs as root, so the answer is `unattributable`.
`reject` then neither refuses your own container nor catches another
copy's container on that port.

Do not use `reject` for a service that hands its listening socket to a
process outside its tree, such as a detached daemon, PM2, WSL's port relay or
rootless Podman's port forwarder: those answers are `foreign` even though they
are expected, and `reject` would hold readiness until it times out.

## Health Checks

`ready_when` answers "has this incarnation started serving?" once.
`health_when` keeps asking after that, for as long as the service runs:

```yaml
processes:
  worker:
    cmd: node worker.js
    restart: on-failure
    max_restarts: 5
    ready_when:
      cmd: node worker.js --check
    health_when:
      cmd: node worker.js --check          # or http: / tcp:
    health_interval: 15                    # seconds; default 10 (also used for 0)
    health_action: restart                 # report (default) or restart
```

`health_when` takes one `http`, `tcp`, or `cmd` probe, with the same meaning,
environment and `${...}` references as in `ready_when`. `log` is not accepted:
a log line matches once and cannot be checked again.

- Checks start once the incarnation is ready and run one at a time:
  each starts `health_interval` seconds after the previous one finished. A
  check that takes longer than `health_interval` or 10 seconds, whichever is
  shorter, fails, so a slow command needs a longer interval.
- The first passing check makes the service `healthy`. Three failed checks in
  a row make it `unhealthy`; one passing check makes it `healthy` again.
  Before the first verdict it is `unknown`.
- `health_action: report` (the default) only reports: the service keeps
  running. `health_action: restart` stops the unhealthy incarnation and counts
  it as a failure, so it restarts with the usual backoff and `max_restarts`
  applies. It needs `restart: on-failure` or `always`; with `restart: never`
  the config is rejected.
- Health does not change readiness. `/ready`, `up --wait-ready` and
  `depends_on` keep using the incarnation's `ready_when` result. `/health`,
  `status --json`, the monitor and `verify` report the health verdict.
- `cmd` health checks run like `cmd` readiness probes: they are not processes
  of the stack, their output stays out of the logs, and they are stopped with
  the service. The failed check's exit status and the start of its stderr are
  reported as `health_error`. A check proc-compose cannot launch at all (for
  example when the crash-recovery record cannot be written) is skipped and
  logged, not counted as a failure, so it never restarts a service. While
  checks are skipped, `health` is `unknown` and `health_error` names why.

Tasks cannot use `health_when`, `health_interval` or `health_action`. Setting
`health_interval` or `health_action` without `health_when` is an error.

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

Tasks can depend on tasks, so a chain such as reset → seed → services runs each
step only after the previous one exited `0`; see
[Reset and seed before services](examples.md#reset-and-seed-before-services).

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

- Each upstream process (`client`/`server` in simple mode; every backing process in route mode) gets a `ready_when: tcp localhost:<port>` probe — but only when you haven't supplied your own `ready_when`. A `ready_when` you set yourself, of any kind, always wins.
- The injected `merge-port` process gets `depends_on` set to every upstream, so the runner blocks `merge-port` startup until the probes pass.

The upshot: a fresh `proc-compose up` on the simple example below boots `client` and `server`, waits for both to bind their ports, *then* starts `merge-port`. Requests that hit `:8080` after the daemon prints "ready" can never land on an unbound upstream.

Override the auto-detected client process with `merge.client_process` (also used by the env-injection step). Upstream ports that don't match any managed process are left alone — proc-compose won't fabricate dependencies on external services.

Before starting, proc-compose always checks that the merge-port proxy output port (`merge.port`, or `$PORT`/`8080`) is free. It also checks upstream ports owned by managed processes — ownership is determined by a process `env.PORT` matching `merge.client`, `merge.server`, or a `merge.routes` target port. Managed upstream ports must be free because proc-compose is about to start those processes. Upstream ports with no matching managed process are treated as external services and may already be listening; proc-compose does not reject those occupied external targets.

### Client API URL auto-injection

When a `merge:` section is present, proc-compose looks at the client process's `.env.example` (or `.env`) and, for any of these well-known frontend variables present in the file, injects the merge-port URL into the client's environment:

`VITE_API_BASE_URL`, `VITE_API_URL`, `VITE_SERVER_URL`, `REACT_APP_API_URL`, `REACT_APP_BASE_URL`, `REACT_APP_API_BASE_URL`, `NEXT_PUBLIC_API_URL`, `NEXT_PUBLIC_API_BASE_URL`, `NUXT_PUBLIC_API_BASE`, `PUBLIC_API_URL`.

The path suffix from the example value is preserved (e.g. `http://1.2.3.4:3000/api` → `http://localhost:8080/api`). Anything you set explicitly under the process's `env:` block always wins. The client process is detected by matching `merge.client` against each process's `PORT` env var, or by name (`client`, `frontend`, `web`, `ui`, `app`); set `merge.client_process` to override.
