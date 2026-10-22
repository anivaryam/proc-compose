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
| `ready_timeout` | No | `60` | Seconds to wait for readiness before failing (`-1` = no limit; a value below `-1` is refused) |
| `health_when` | No | — | Ongoing health check after readiness: `http`, `tcp`, or `cmd` (see [Health Checks](#health-checks)) |
| `health_interval` | No | `10` | Seconds between health checks; `0` uses the default; requires `health_when` |
| `health_action` | No | `report` | `report` marks the service unhealthy; `restart` restarts it through `restart`; requires `health_when` |
| `foreign_listener` | No | `allow` | What an `http` or `tcp` probe does with an answer from a process outside this process's tree: `allow` passes and reports it, except that `verify` refuses an answer from another live session unless given `--allow-cross-session`; `reject` fails the attempt (see [Who answered the probe](#who-answered-the-probe)); requires an `http` or `tcp` `ready_when` or `health_when` |
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

A `foreign` answer is also placed against the other sessions in the same
runtime directory (the ones `proc-compose sessions` lists), read when the
answer is judged, and gets a `scope`:

| `scope` | Meaning |
|---|---|
| `cross_session` | It belongs to another live session. `session` names it (`name`, `run_id`, `config_path`, `worktree`) and gives the `evidence`: `holder` when the holder is a member of a process group that session recorded (Linux only; the group is proven by its leader's start time, as crash recovery proves it), with `process` naming the group; `port_lease` when the answered port is one that session was assigned and leases, with `process` and `port` naming the assignment. |
| `outside_session` | No live session is known to hold it: a program proc-compose did not start, a session in another runtime directory, or a session whose supervisor is no longer running. |

A session counts as live only while its supervisor (the `up` process or
daemon, or the `verify` process) is still running with the start time its PID
file recorded, so a finished or crashed session never makes an answer
`cross_session`, even when processes it started still run, and neither does
one whose supervisor has exited but has not been reaped yet (a zombie on
Linux, or on Windows a process some handle still holds open). `scope` is absent
for `owned` and `unattributable` answers, for a `foreign` answer that the
same session's own record places (another of its processes, through its
recorded process group on Linux or a port it leases), and in records written
by earlier versions. Another process of the same session that its record does
not place, such as one on a hardcoded port it does not lease on Windows, is
`outside_session`; neither is ever refused.

The result appears as `readiness_owner` and `health_owner` in `status --json`,
as `owner` on each `verify` readiness observation, and, when it is `foreign`,
as a `readiness.foreign_listener` or `health.foreign_listener` warning event,
whose message names the other session of a `cross_session` answer. The
journal's state events carry the same owner, with its `scope` and `session`.
The health warning is logged when the answer becomes foreign or changes to
another foreign holder, not on every check. Like `last_error`,
`readiness_owner` describes the latest incarnation until the next one starts;
`health_owner` is shown only while the process runs.

By default (`foreign_listener: allow`) attribution only reports. It does not
change readiness, health, `depends_on` or `--wait-ready`, nor the `verify`
verdict except for a `cross_session` answer (see
[Answers from another session under verify](#answers-from-another-session-under-verify)).
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

#### Answers from another session under verify

`verify` refuses a `cross_session` answer by default, the way
`foreign_listener: reject` refuses a `foreign` one: the probe attempt counts
as failed, readiness keeps waiting for the process's own server, and at
`ready_timeout` the error names the other session, for example
`readiness probe (http) timed out (last attempt: answered at 127.0.0.1:3000
by PID 4242 (node) on a port leased by session "feature-x" (web.http, in
/home/me/wt/feature-x), outside api's process tree)`. The first refusal is
logged as a `readiness.foreign_listener` warning, and the refused answer is
kept as the readiness observation's `owner`. A `health_when` check answered
that way counts as a failed check. `outside_session` and `unattributable`
answers are never refused this way, so a shared database or a detached daemon
outside every session behaves as before, and so does every answer under
`up`, `compare` and every other command, which only label them.

To count such answers again, pass `verify --allow-cross-session`; the owner
is still recorded with its `scope` and `session`.

On Linux both kinds of evidence refuse. Windows records no process groups, so
only `port_lease` evidence exists there: a holder on a hardcoded port that
the other session does not lease is `outside_session` and is taken. macOS
attributes nothing, so nothing is refused.

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

If a dependency fails before becoming ready and is not restarted, dependent
processes also fail. A dependency that the restart policy retries does not fail
its dependents: they keep waiting for the replacement incarnation's readiness
probe, and they fail only when the dependency's lifecycle ends for good.

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

A single line of the env file may be up to 1 MiB long: 1,048,576 bytes, not
counting its line terminator (`\n` or `\r\n`). A longer line is an error
(`env_file_unreadable`) that names the file and the line, not a silently
truncated value.

## Shared Resources

A top-level `resources` list declares the exclusive labels a stack holds while
it runs, so two stacks that share a database or a build directory serialise
instead of colliding:

```yaml
resources:
  - 'postgresql:local:role-ddl'
  - 'next-build:${config.dir}/apps/web'
```

A label is normalised **once**, when the config is loaded: surrounding
whitespace is trimmed, and the trimmed label is the identity the lease, the
duplicate check and `status --json` all use. `db` and `" db "` therefore name
one resource — the first occurrence wins, the session holds one lease, and no
error is raised. A label that is **exactly** repeated is still a configuration
error (`invalid_resource`), as is an empty label, a label longer than 256
**bytes** and a label containing a line break or a NUL byte; those checks run
on the trimmed label, so a label padded with whitespace is accepted when its
trimmed form is valid.

The lease identity is taken after the label's references are expanded. A label
that is then an absolute path is cleaned lexically, like Go's `filepath.Clean`
(symlinks are not resolved): `${config.dir}/db`, the literal path, and a
spelling with a doubled or trailing separator — or, on Windows, `/` where the
path has `\` — are one resource, and `status --json` lists the cleaned path.
Every other label is compared exactly as written: `db`, `tcp:127.0.0.1:5432`,
a URL, a relative path such as `./db`, and a path behind a prefix such as
`next-build:${config.dir}/apps/web`, so spell such a label the same way in
every config that shares it.

## JSON Schema

`proc-compose config schema` prints the published JSON Schema (draft
2020-12). It states every loader rule that JSON Schema can express: a
non-empty `processes` map or a `merge` section, the field types, ranges and
enums, the `task` and `teardown` restrictions, the fields that need a probe or
a restarting policy, exactly one non-empty kind per probe, the `merge` modes
and port ranges, the reserved `merge-port` name next to a `merge` section, and
the resource label rule. A resource label is checked on its trimmed form, as
the loader checks it: `" db "` is valid, `" "` is not. `uniqueItems` compares
the labels as written, so `db` next to `" db "` is valid there too, as it is
for the loader. The patterns use a character class that matches exactly the
whitespace the loader trims.

The loader is the authority for what runs. These loader rules cannot be
expressed in JSON Schema, so a file can be schema-valid and still refused at
load time:

- a `depends_on` entry that names an unknown process, the process itself or a
  teardown process, or that closes a cycle;
- a merge upstream started with a `PORT` other than the merge port (see
  [Startup ordering and readiness](#startup-ordering-and-readiness));
- a `merge.client_process` that names a task or teardown process;
- an unset `merge.port` while the `PORT` environment variable is out of range;
- a `ready_when.log` that is not a valid Go regular expression;
- an `env_file` that cannot be read or has a line longer than 1 MiB;
- a resource label of at most 256 characters but more than 256 bytes (the
  schema counts characters, the loader bytes);
- an integer too large for the platform's `int`.

The YAML decoder is more lenient than the schema's types, so these values are
refused by the schema and accepted by the loader:

- a `null` value, which the loader reads as unset;
- an empty `mode`, `restart`, `health_action` or `foreign_listener`, which the
  loader reads as the default;
- a number or boolean where a string is expected, which the loader reads as
  its text;
- a fractional number where an integer is expected, which the loader
  truncates.

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
| `client` | — | Client/frontend port (simple mode) — must be 1-65535 |
| `server` | — | Server/backend port (simple mode) — must be 1-65535 |
| `port` | `$PORT` → `8080` | Proxy listen port — reads `$PORT` env var if not set, then falls back to `8080`; must be 1-65535 |
| `api_prefixes` | `[/api]` | Path prefixes routed to server (simple mode) |
| `routes` | — | Explicit `prefix=target` routes (route mode) |
| `client_process` | auto | Override the process treated as the client (upstream wiring and env auto-injection); must name a service |

`routes` cannot be combined with `client`, `server`, or `api_prefixes`.

`client`, `server` and `port` must be a port number between 1 and 65535. A
value outside that range is refused when the config is loaded
(`invalid_merge`) — including a `PORT` environment variable outside the range,
which the `port` fallback would otherwise pick up. `port: 0` still means
"unset", so the `$PORT` value and then `8080` are used. A route whose target
is a port number (`/api=3001`) must name a port between 1 and 65535 as well,
and is refused at `merge.routes[<index>]` otherwise; a target written as a URL
is passed to merge-port as written.

Auto-detection of `api_prefixes` only matches top-level Express/Koa/Fastify/Hono handlers (`app.use`, `server.get`, etc.) and Go chi/gorilla/`net/http` handlers (`Route`, `Mount`, `Handle`, `HandleFunc`). Sub-router files and other frameworks (Django, Rails, Flask, etc.) need explicit `api_prefixes`.

### Startup ordering and readiness

When a `merge:` section is present, proc-compose wires the upstream ports automatically so the proxy never starts before its backends are accepting connections:

- Each upstream process (`client`/`server` in simple mode; every backing process in route mode) gets a `ready_when: tcp localhost:<port>` probe — but only when you haven't supplied your own `ready_when`. A `ready_when` you set yourself, of any kind, always wins.
- The injected `merge-port` process gets `depends_on` set to every upstream, so the runner blocks `merge-port` startup until the probes pass.

Only **service** processes can be upstreams. A `task` or `teardown` process is
never selected as one, so it gets neither the readiness probe nor the
dependency edge — the merge wiring never writes a field the process's mode
forbids. The same candidates feed every merge selection: the upstream wiring,
the `api_prefixes` scan and the client env injection, so a task named `server`
is not scanned and a task named `web` gets no injected API URL. A
`merge.client_process` that names a task or teardown process is refused
(`invalid_merge` at `merge.client_process`).

The upshot: a fresh `proc-compose up` on the simple example below boots `client` and `server`, waits for both to bind their ports, *then* starts `merge-port`. Requests that hit `:8080` after the daemon prints "ready" can never land on an unbound upstream.

Override the auto-detected client process with `merge.client_process` (also used by the env-injection step). Upstream ports that don't match any managed process are left alone — proc-compose won't fabricate dependencies on external services.

An upstream is matched by the `PORT` it is started with, and a process selected
by name (or by `merge.client_process`) must be started on the merge port it was
selected for. That `PORT` is the one the session gives the process: its
`env.PORT`, else the `PORT` in its `env_file`, else its `ports.http` port; a
`${ports.<process>.<port>}` reference counts as the referenced port's preferred
value. If that `PORT` differs from `merge.client` or `merge.server`, the config
is refused (`invalid_merge` at `merge.client` / `merge.server`) instead of being
probed on a port the process does not serve. A process whose `PORT` cannot be
known when the config is loaded — none is declared, `ports.http` is `0`
(ephemeral), or a reference only the session resolves — is still probed at the
merge port: the documented guess.

The proxy is injected under the reserved process name `merge-port`. A config
that defines a process with that name **and** a `merge:` section is refused
(`reserved_process_name`); without a `merge:` section nothing generates a
proxy, so the name is free.

Before starting, proc-compose always checks that the merge-port proxy output port (`merge.port`, or `$PORT`/`8080`) is free. It also checks upstream ports owned by managed processes — ownership is determined by a process `env.PORT` matching `merge.client`, `merge.server`, or a `merge.routes` target port. Managed upstream ports must be free because proc-compose is about to start those processes. Upstream ports with no matching managed process are treated as external services and may already be listening; proc-compose does not reject those occupied external targets.

The merge port is **leased** for the session's lifetime, like a named port: a
second proc-compose session that declares the same merge port is refused at
startup (`resource "tcp:127.0.0.1:8080" is owned by another session`) instead
of failing later, when the proxy cannot bind. The lease is taken only when the
proxy is part of the selected process set: a filtered start that does not
select `merge-port` never binds the port, so it does not hold the lease and
does not refuse another session that would bind it. Unlike a named port, which
the session keeps bound until its process is launched, the merge port is only
leased: the lease excludes **other proc-compose sessions only** — a third-party
program can still take the port between the check and the proxy's bind, and
`up --silent` checks the port in the parent process before the daemon child
takes the lease. `verify` runs no port check of its own, so the lease is what
keeps it from colliding with another session; a collision there is reported as
`resource_unavailable`. The merge port is never reallocated and never reported
as a named port: a managed process that prefers the same port is moved to the
next free port. A reload that changes `merge.port` is refused (`merge port
changed; restart the session`), as a change to the resources or named ports
is; a reload that removes the `merge:` section leaves the running proxy and its
lease alone.

### Client API URL auto-injection

When a `merge:` section is present, proc-compose looks at the client process's `.env.example` (or `.env`) and, for any of these well-known frontend variables present in the file, injects the merge-port URL into the client's environment:

`VITE_API_BASE_URL`, `VITE_API_URL`, `VITE_SERVER_URL`, `REACT_APP_API_URL`, `REACT_APP_BASE_URL`, `REACT_APP_API_BASE_URL`, `NEXT_PUBLIC_API_URL`, `NEXT_PUBLIC_API_BASE_URL`, `NUXT_PUBLIC_API_BASE`, `PUBLIC_API_URL`.

The path suffix from the example value is preserved (e.g. `http://1.2.3.4:3000/api` → `http://localhost:8080/api`). Anything you set explicitly under the process's `env:` block always wins. The client process is the service started on `merge.client` (see [Startup ordering and readiness](#startup-ordering-and-readiness)), else the first service named `client`, `frontend`, `web`, `ui` or `app`; set `merge.client_process` to override. A task or teardown process never gets the injection.
