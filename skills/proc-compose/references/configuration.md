# Configuration reference

The config is `proc-compose.yml` (fallback `proc-compose.yaml`), found by
walking up from the current directory, or given with `-f PATH`. Relative paths
(`dir`, `env_file`) resolve from the config file's directory. Unknown fields
are errors. `proc-compose config schema` prints the JSON Schema of the
installed version.

## Every field

```yaml
resources:                       # optional: exclusive lease labels held while the stack runs
  - 'postgresql:local:app'
  - 'next-build:${config.dir}/web'

merge:                           # optional: single-port proxy via merge-port (see below)
  client: 5173
  server: 3001
  port: 8080                     # default: $PORT, then 8080
  api_prefixes: [/api]           # default [/api]; auto-detected from server routes
  client_process: web            # default: detected from PORT or the name
  # routes: ['/api=3001', '/=3000']   # route mode; not combined with client/server/api_prefixes

processes:
  api:                           # name: letters, digits, '-', '.', '_'; starts and ends alphanumeric
    cmd: npm run dev             # required; Linux/macOS run it with sh -c, Windows with cmd /c
    mode: service                # service (default) | task | teardown
    dir: ./server                # working directory (default: config directory)
    env_file: .env               # loaded first
    env:                         # overrides env_file and the inherited environment
      LOG_LEVEL: debug
    ports:                       # named preferred ports; 0 = ephemeral
      http: 3000                 # an http port exports PORT unless env sets PORT
    restart: on-failure          # never (default) | on-failure | always
    max_restarts: 5              # 0 = unlimited; needs restart other than never
    shutdown_timeout: 10         # seconds from graceful stop to forced kill (default 5)
    depends_on: [db, migrate]    # start after these are ready (tasks: after exit 0)
    ready_when:                  # exactly one of http | tcp | cmd | log
      http: '${urls.api.http}/health'   # ready on a 2xx response
      # tcp: localhost:5432             # ready when a TCP connect succeeds
      # cmd: ./scripts/ready-check      # ready when the command exits 0
      # log: 'listening on'             # ready when a stdout line matches this Go regex
    ready_timeout: 60            # seconds (default 60; -1 = no limit)
    health_when:                 # ongoing check after readiness: http | tcp | cmd (no log)
      http: '${urls.api.http}/health'
    health_interval: 10          # seconds between checks (default 10)
    health_action: report        # report (default) | restart (needs restart: on-failure or always)
    foreign_listener: allow      # allow (default) | reject another process's answer to an http/tcp probe
```

## Modes

| | `service` (default) | `task` | `teardown` |
|---|---|---|---|
| Runs | Until stopped | Once per `up` | Once after the stack stops |
| Ready when | Started, or `ready_when` passes | Exits 0 | Not applicable |
| Failure | Restart policy applies | Non-zero exit fails the whole stack | Stop verdict fails with a `teardown:` line |
| Not allowed | | `ready_when`, `ready_timeout`, `health_*`, `restart` other than `never`, `max_restarts` | Everything a task forbids, plus `ports`, `depends_on`; nothing may depend on it |
| `restart` command | Allowed | Refused | Not applicable |

- Use tasks for migrations, seeding, codegen, and builds. Tasks can depend on
  tasks, so `reset -> seed -> api` runs each step after the previous exited 0.
  Tasks run again on every `up`; keep destructive steps behind a guard that
  checks they point at a test database.
- A stack with only tasks cannot run with `up --silent`; it runs in the
  foreground.
- Teardown processes release what the stack held (uploads, buckets, rows). They
  run one at a time, in name order, within 15 seconds in total, after every
  managed process is verified terminated, while ports and leases are still
  held. They run after `stop`, `stop --force`, Ctrl+C, a foreground stack
  that ends, and a failed task. They are skipped if any process could not be
  verified terminated, and they do not run if the daemon crashes (`stop` then
  lists them as `teardown did not run`). They get the session variables and
  `${...}` references. They cannot be named in `up` or `verify`.

## Readiness and dependencies

- Without `ready_when`, a service is ready as soon as it starts. Add a probe to
  every service that others depend on or that a test will hit; `state:
  running` is not readiness.
- `http` and `tcp` probes try at once, then about once a second. `cmd` probes
  run through the same shell, `dir`, and environment as `cmd`, retry one
  second after each failure, and each attempt may run at most 10 seconds.
  `log` matches a Go regex against stdout.
- `depends_on` holds a process until its dependencies are ready. If a
  dependency fails before it becomes ready, its dependents fail too.
  `config plan` shows the resolved order.
- `up --wait-ready`, `restart --wait-ready`, and `verify` all wait on these
  probes.

### Who answered the probe

An `http` or `tcp` probe passes when any process answers at its address, so a
dev server from another worktree on a hardcoded port makes a new process look
ready before it has bound anything. On Linux and Windows proc-compose records
who answered:

- `owned`: a process in this process's own tree.
- `foreign`: another process run by the same user, with `pid` and `command`.
  Its `scope` is `cross_session` (another live proc-compose session, named in
  `session`) or `outside_session`.
- `unattributable`: the holder could not be named, with a `reason` (another
  user's process such as a root `docker-proxy`, a remote or container address,
  and always on macOS).

It appears as `readiness_owner` and `health_owner` in `status --json`, as
`owner` on `verify` readiness observations, and as
`readiness.foreign_listener` or `health.foreign_listener` warning events.
By default it only reports. `foreign_listener: reject` makes a `foreign`
answer count as a failed attempt, so readiness waits for the process's own
server. `verify` refuses `cross_session` answers by default (Linux fully,
Windows on leased ports only, macOS never); `verify --allow-cross-session`
accepts them. Do not use `reject` for a service that hands its socket to a
process outside its tree (PM2, a detached daemon, WSL's port relay, rootless
Podman). The real fix is named `ports`, which keeps copies apart.

## Health checks

`health_when` takes one `http`, `tcp`, or `cmd` probe and starts after the
process is ready. Each check starts `health_interval` seconds after the
previous one finished and fails if it runs longer than the interval or 10
seconds, whichever is shorter. The first pass makes the service `healthy`;
three failures in a row make it `unhealthy`; one pass recovers it. Before the
first verdict it is `unknown`. `health_action: report` only reports;
`restart` restarts the unhealthy incarnation through the restart policy (and
`max_restarts`). Health never changes readiness: `/ready`, `--wait-ready`, and
`depends_on` ignore it, while `/health`, `status --json` (`health`,
`health_error`), the monitor, and `verify` report it.

## Restart policies

| `restart` | Behavior |
|---|---|
| `never` | Runs once. With `up`, a non-zero exit makes proc-compose exit non-zero. |
| `on-failure` | Restarts after a non-zero exit. |
| `always` | Restarts after any exit. |

Backoff starts at 1 second and doubles up to 30 seconds; it resets after the
process runs longer than the current backoff. After `max_restarts` restarts
the process is `failed`.

## Environment and references

- Order: inherited environment, then `env_file`, then `env`.
- References work in `cmd`, `env` values, probe fields, and `resources`:

| Reference | Value |
|---|---|
| `${ports.api.http}` | Port assigned to `api`'s `http` port |
| `${urls.api.http}` | Loopback URL for it, such as `http://127.0.0.1:3000` |
| `${session.name}` | Session name (`default` without `--session`) |
| `${session.id}` | Stable session hash |
| `${session.dir}` | Per-session runtime directory (for databases, uploads, queues) |
| `${config.dir}` | Directory of the config file |

- Every process also gets `PROC_COMPOSE_SESSION`, `PROC_COMPOSE_SESSION_DIR`,
  and `PROC_COMPOSE_RUN_ID`.
- `resources` labels are advisory leases: a second session in the same
  runtime directory that declares the same label fails before any command
  starts. Separate ports do not isolate databases or build outputs; give each
  session its own state (for example under `${session.dir}`) or declare the
  shared thing as a resource.

## merge-port

A `merge:` section injects a `merge-port` process (install it with
`brokit install merge-port`) that serves frontend and backend on one port.

- Simple mode: `client` and `server` ports, plus `api_prefixes` routed to the
  server. Prefixes are auto-detected from top-level Express, Koa, Fastify,
  Hono, chi, gorilla, and `net/http` routes; other frameworks need explicit
  `api_prefixes`.
- Route mode: `routes: ['/api=3001', '/auth=3002', '/=3000']`.
- Each upstream process without its own `ready_when` gets a `tcp` probe, and
  `merge-port` depends on every upstream, so the proxy starts last.
- Well-known client variables found in the client's `.env.example` or `.env`
  (`VITE_API_URL`, `VITE_API_BASE_URL`, `VITE_SERVER_URL`,
  `REACT_APP_API_URL`, `REACT_APP_BASE_URL`, `REACT_APP_API_BASE_URL`,
  `NEXT_PUBLIC_API_URL`, `NEXT_PUBLIC_API_BASE_URL`, `NUXT_PUBLIC_API_BASE`,
  `PUBLIC_API_URL`) are set to the proxy URL, keeping their path. Values in
  `env:` win.
- The proxy port and every upstream port owned by a managed process (matched
  by its `env.PORT`) must be free before start.

## Platform notes

- Linux and macOS run `cmd` with `sh -c`; Windows uses `cmd /c`. On Windows
  write `%PORT%`, or call PowerShell explicitly:
  `powershell -NoProfile -Command "Write-Output $env:PORT"`. POSIX syntax
  (`$VAR`, `$(...)`, `trap`, `export`) is not portable.
- Each process runs in its own process group (Job Object on Windows), so whole
  trees such as npm -> node stop together.
- A process that daemonizes and exits at once (for example `tunnel --silent`)
  looks like a crash and restart-loops. Run such tools in the foreground.
