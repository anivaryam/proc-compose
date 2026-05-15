# proc-compose

A lightweight process runner that starts multiple local services from a single YAML config. Color-coded log output, restart policies, clean shutdown with one Ctrl+C — and optional merge-port integration built in.

```
proc-compose up

proc-compose starting 3 processes

  backend    configured
  frontend   configured
  merge-port configured

  Press Ctrl+C to stop all

frontend   │ ready
backend    │ ready
merge-port │ ready
frontend   │ VITE v5.4.2  ready in 237ms
backend    │ listening on :3001
merge-port │ merge-port is running on port 8080
```

## Install

**With [brokit](https://github.com/anivaryam/brokit)** (recommended — handles install, update, and uninstall across the whole tool family in one command):

```sh
brokit install proc-compose         # install latest release
brokit update proc-compose          # upgrade to latest
brokit list                         # see installed tools and versions
brokit remove proc-compose          # uninstall
```

`brokit` reads the GitHub releases for this repo, verifies the binary, and drops it into `/usr/local/bin`. It also installs the optional companions (`merge-port`, `tunnel`, `env-vault`, `proxy-relay`) the same way, which is the easiest way to unlock proc-compose's `merge:` and tunnel features.

**From release binary (Linux/macOS, single-tool install):**

```sh
curl -sSL https://raw.githubusercontent.com/anivaryam/proc-compose/main/install.sh | bash
```

The script downloads a release binary for your platform into `~/.local/bin/`
(override with `PROC_COMPOSE_INSTALL_DIR=/path/to/dir`). Pin a specific
version with `PROC_COMPOSE_VERSION=v1.2.3`. If no release matches your
platform but `go` is on `$PATH`, the script falls back to `go install`.

**With Go**:

```sh
go install github.com/anivaryam/proc-compose/cmd/proc-compose@latest
```

**From source**:

```sh
git clone https://github.com/anivaryam/proc-compose.git
cd proc-compose
make install  # copies to ~/.local/bin/
```

### Optional binary dependencies

proc-compose unlocks additional capabilities when these binaries are available on your `PATH`. The simplest way to install them is `brokit install <name>`; alternatives are listed for reference.

| Binary | Enables | Install |
|--------|---------|---------|
| [`merge-port`](https://github.com/anivaryam/merge-port) | `merge:` config section — combine frontend + backend into one port | `brokit install merge-port` |
| [`tunnel`](https://github.com/anivaryam/tunnel) | Public URL for a local port (see "Full-stack with public tunnel" example) | `brokit install tunnel` |

These are optional. proc-compose works without them; the features that depend on them simply won't be available.

## Quick Start

Generate a starter config:

```sh
proc-compose init
# edit proc-compose.yml, then:
proc-compose up
```

Or write it by hand:

```yaml
merge:
  client: 5173   # frontend port
  server: 3001   # backend port
  port: 8080     # combined proxy port (merge-port required)

processes:
  frontend:
    cmd: npm run dev
    dir: ./client
    env:
      PORT: "5173"
  backend:
    cmd: go run ./cmd/server
    dir: ./server
    env:
      PORT: "3001"
    restart: on-failure
```

```sh
proc-compose up
```

Frontend, backend, and the merge-port proxy all start together. Logs are interleaved with color-coded prefixes. Ctrl+C stops everything cleanly.

## CLI Usage

```
proc-compose init [--template T]     Generate a starter config (templates: minimal, node, go, python)
proc-compose validate                Parse and validate config without starting (alias: check)
proc-compose doctor [--write]        Scan project and diagnose proc-compose setup
proc-compose bootstrap [--write] [--verify]  Generate and optionally verify setup
proc-compose up [processes...]       Start all or named processes
proc-compose up --survive --name <n> Print or install a systemd user unit
proc-compose status [--json]         Show running daemon's process states (alias: st, ps)
proc-compose stop [--timeout N]      Stop a running daemon (graceful, escalates to SIGKILL)
proc-compose monitor                 Connect to a running daemon and display a live TUI
proc-compose list                    List processes defined in config
proc-compose restart <process>       Restart a single process in a running daemon
proc-compose reload                  Reload config and restart changed processes
proc-compose logs [-n N]             Print log file for the current config
proc-compose uninstall --name <n>    Remove a systemd unit installed via --survive
proc-compose man [--dir DIR]         Generate man pages
proc-compose --version               Print version

Flags (up):
  -f, --file string        Config file path (default "proc-compose.yml"; falls back to proc-compose.yaml)
  -s, --silent             Daemonize — run in the background and exit
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
  -v, --verbose            Enable verbose debug output
      --health-port int    HTTP port for /health endpoint (0 = disabled)

Flags (stop):
      --timeout int        Seconds to wait for graceful shutdown before SIGKILL (default 10)
      --force              SIGKILL immediately without graceful shutdown

Flags (init):
      --template string    Starter template: minimal (default), node, go, python
```

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

The process starts daemonized. Logs are written to `app.log`. Use `monitor` to watch it live or `stop` to shut it down.

### Monitor a running daemon

```sh
proc-compose monitor
```

Opens an interactive TUI showing process status, resource usage, and a scrollable log area:

```
proc-compose monitor               q=quit  ?=help  ↑↓/jk=nav  ⏎=filter  a=all  PgUp/Dn=scroll
PROCESS      STATUS        RESTARTS  CPU%    MEM       UPTIME
▶ backend    ● running     0         12.5%    45.2MB   1m32s
  frontend   ● running     0          8.1%    32.1MB   1m32s
  merge-port ● running     0          0.2%     8.4MB   1m32s
──────────────────────────────────────────────────────────────────────────────────────────────
  LOGS  [all]
backend    │ listening on :3001
frontend   │ VITE v5.4.2  ready in 237ms
merge-port │ merge-port is running on port 8080
```

Keys:

| Key             | Action                                |
|-----------------|---------------------------------------|
| `q` / `Ctrl+C`  | Quit                                  |
| `?` / `h`       | Toggle help overlay (any key dismisses) |
| `↑` / `↓` or `k` / `j` | Navigate processes             |
| `Enter`         | Filter logs to the selected process   |
| `a`             | Show all logs (clear filter)          |
| `PgUp` / `PgDn` | Scroll log area                       |
| `G`             | Jump to live tail                     |
| `g`             | Jump to top of log buffer             |

`NO_COLOR=1` (or `--no-color` on `up`) disables colour output in both the runner and the monitor.

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
`--json` output includes a `ready` flag per process that distinguishes
"started" from "started and passed its readiness probe".

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

`doctor` helps first-time setup and existing config debugging. Without a config file, it scans common Node, Go, and Python project layouts, then prints suggested processes, ports, readiness probes, and YAML. With an existing config, it checks paths, env files, port conflicts, readiness gaps, merge-port setup, and likely missing binaries.

`--write` is safe by default: it only creates `proc-compose.yml` when neither `proc-compose.yml` nor `proc-compose.yaml` exists.

### Bootstrap a working setup

```sh
proc-compose bootstrap                  # dry run: show generated config
proc-compose bootstrap --write          # create proc-compose.yml when none exists
proc-compose bootstrap --verify         # test generated config without writing it
proc-compose bootstrap --write --verify # write, then verify
```

`bootstrap` builds on `doctor`: it scans the repo, proposes a config, and can verify the config through proc-compose. The default mode is safe and read-only. Use `--write` to create `proc-compose.yml`; it refuses to overwrite existing configs.

Use `doctor` when you want diagnostics only. Use `bootstrap` when you want a first-run setup path.

### Wait for readiness when daemonising

```sh
proc-compose up --silent --log-file app.log --wait-ready --wait-timeout 60
```

Without `--wait-ready`, `up --silent` returns once the daemon has bound
its IPC socket — processes may still be starting. With `--wait-ready` the
command blocks until every started process passes its `ready_when` probe
(or `--wait-timeout` elapses). Useful in container entrypoints and CI
fixtures that need a hard guarantee before running the next step.

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

## Configuration

The config file is `proc-compose.yml` by default; if that's missing,
proc-compose falls back to `proc-compose.yaml` automatically. Pass
`-f path/to/file` to use any other path. Each process has the following fields:

```yaml
processes:
  name:
    cmd: "command to run"        # required — passed to sh -c
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

### Process Fields

| Field | Required | Default | Description |
|-------|----------|---------|-------------|
| `cmd` | Yes | — | Shell command to run (via `sh -c`) |
| `dir` | No | `.` | Working directory for the process |
| `env_file` | No | — | Path to `.env` file; values are merged before `env` block |
| `env` | No | — | Extra environment variables (merged with system env; overrides `env_file`) |
| `restart` | No | `never` | Restart policy: `never`, `on-failure`, or `always` |
| `max_restarts` | No | `0` (unlimited) | Stop restarting after N restarts; process enters "failed" state |
| `shutdown_timeout` | No | `5` | Seconds to wait for graceful shutdown before SIGKILL |
| `ready_when` | No | — | Readiness probe (see below) |
| `ready_timeout` | No | `60` | Seconds to wait for readiness before failing (`-1` = no limit) |
| `depends_on` | No | — | List of process names to wait for before starting |

### Restart Policies

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

### Readiness Probes

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

Probes poll every 3 seconds until success or process exit. The default readiness deadline is 60 seconds — set `ready_timeout` to override (`-1` disables the limit). Without `ready_when`, a process is considered ready as soon as it starts.

### Startup Dependencies

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

### Environment File

Use `env_file` to load environment variables from a `.env` file. The `env` block overrides any conflicting keys from `env_file`:

```yaml
processes:
  backend:
    cmd: npm run start
    env_file: ./server/.env
    env:
      PORT: "3000"  # overrides PORT from .env file
```

### merge-port Integration

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

#### Startup ordering and readiness

When a `merge:` section is present, proc-compose wires the upstream ports automatically so the proxy never starts before its backends are accepting connections:

- Each upstream process (`client`/`server` in simple mode; every backing process in route mode) gets a `ready_when: tcp localhost:<port>` probe — but only when you haven't supplied your own `ready_when`. User probes (`http`, `log`) always win.
- The injected `merge-port` process gets `depends_on` set to every upstream, so the runner blocks `merge-port` startup until the probes pass.

The upshot: a fresh `proc-compose up` on the simple example below boots `client` and `server`, waits for both to bind their ports, *then* starts `merge-port`. Requests that hit `:8080` after the daemon prints "ready" can never land on an unbound upstream.

Override the auto-detected client process with `merge.client_process` (also used by the env-injection step). Upstream ports that don't match any managed process are left alone — proc-compose won't fabricate dependencies on external services.

#### Client API URL auto-injection

When a `merge:` section is present, proc-compose looks at the client process's `.env.example` (or `.env`) and, for any of these well-known frontend variables present in the file, injects the merge-port URL into the client's environment:

`VITE_API_BASE_URL`, `VITE_API_URL`, `VITE_SERVER_URL`, `REACT_APP_API_URL`, `REACT_APP_BASE_URL`, `REACT_APP_API_BASE_URL`, `NEXT_PUBLIC_API_URL`, `NEXT_PUBLIC_API_BASE_URL`, `NUXT_PUBLIC_API_BASE`, `PUBLIC_API_URL`.

The path suffix from the example value is preserved (e.g. `http://1.2.3.4:3000/api` → `http://localhost:8080/api`). Anything you set explicitly under the process's `env:` block always wins. The client process is detected by matching `merge.client` against each process's `PORT` env var, or by name (`client`, `frontend`, `web`, `ui`, `app`); set `merge.client_process` to override.

## Examples

### Full-stack web app

```yaml
merge:
  client: 5173
  server: 3001

processes:
  frontend:
    cmd: npm run dev
    dir: ./frontend
    env:
      PORT: "5173"
  backend:
    cmd: go run .
    dir: ./backend
    env:
      PORT: "3001"
      DATABASE_URL: "postgres://localhost:5432/myapp"
```

### Full-stack with multiple API prefixes

```yaml
merge:
  client: 5173
  server: 5000
  api_prefixes:
    - /api
    - /health
    - /uploads
    - /welcome

processes:
  client:
    cmd: npm run dev
    dir: ./client
  server:
    cmd: npm run dev
    dir: ./server
    restart: on-failure
```

### Microservices

```yaml
processes:
  gateway:
    cmd: go run ./cmd/gateway
    env:
      PORT: "8080"
  users:
    cmd: go run ./cmd/users
    env:
      PORT: "8081"
  orders:
    cmd: go run ./cmd/orders
    env:
      PORT: "8082"
  worker:
    cmd: go run ./cmd/worker
    restart: always
```

### Full-stack with public tunnel

Tunnel runs as a regular process. The [`tunnel`](https://github.com/anivaryam/tunnel) binary spawns a background daemon (managed via a unix socket in `$TMPDIR`); the foreground command is a client attached to it. If proc-compose kills the client, the daemon keeps running and the public URL stays open. Wrap the command with a `trap` so SIGTERM stops the daemon too:

```yaml
merge:
  client: 5173
  server: 3001
  port: 8080

processes:
  frontend:
    cmd: npm run dev
    dir: ./frontend
    env:
      PORT: "5173"
  backend:
    cmd: go run .
    dir: ./backend
    env:
      PORT: "3001"
  tunnel:
    cmd: bash -c 'trap "tunnel stop --name myapp" EXIT TERM INT; tunnel http 8080 --name myapp; wait'
    depends_on:
      - merge-port
    restart: on-failure
    ready_when:
      http: http://localhost:8080/health
```

The `trap` runs `tunnel stop --name myapp` on shutdown so the daemon and its socket are cleaned up. Without it, `proc-compose stop` (or Ctrl+C) leaves the tunnel exposing your port until you kill it manually.

> **Do not pass `--silent` to `tunnel`.** That flag daemonizes and exits immediately — proc-compose treats the immediate exit as a crash and restarts in a loop. Default (foreground) is correct here.

### Multiple tunnels for multiple services

Same pattern repeated per service. Each tunnel waits on its upstream via `depends_on` + `ready_when`, so the public URL only opens once the local service serves requests:

```yaml
processes:
  api-a:
    cmd: npm run dev
    dir: ./service-a
    env:
      PORT: "7001"
    ready_when:
      http: http://localhost:7001/health
    restart: on-failure

  api-b:
    cmd: npm run dev
    dir: ./service-b
    env:
      PORT: "7002"
    ready_when:
      http: http://localhost:7002/health
    restart: on-failure

  api-a-tunnel:
    cmd: bash -c 'trap "tunnel stop --name api-a" EXIT TERM INT; tunnel http 7001 --name api-a; wait'
    depends_on:
      - api-a
    restart: on-failure
    ready_when:
      http: http://localhost:7001/health

  api-b-tunnel:
    cmd: bash -c 'trap "tunnel stop --name api-b" EXIT TERM INT; tunnel http 7002 --name api-b; wait'
    depends_on:
      - api-b
    restart: on-failure
    ready_when:
      http: http://localhost:7002/health
```

Verify a clean shutdown:

```sh
proc-compose stop
ls /tmp/tunnel-*.sock 2>/dev/null         # empty
ps aux | grep "tunnel http" | grep -v grep   # empty
```

## Cloud Deployment

proc-compose works as a single container entrypoint on Railway, Render, Fly.io, and similar platforms.

### Port binding

Cloud platforms assign a port via the `PORT` environment variable. When `merge.port` is not set in your config, proc-compose reads `$PORT` automatically and passes it to the merge proxy — no config change needed.

```yaml
merge:
  client: 5173
  server: 3001
  # no port: line — proc-compose reads $PORT at runtime

processes:
  frontend:
    cmd: npm run dev
    env:
      PORT: "5173"
  backend:
    cmd: node server.js
    env:
      PORT: "3001"
    restart: on-failure
```

Deploy this as-is. The merge proxy binds to whatever port the platform assigns.

### Exit codes

If a process with `restart: never` exits with a non-zero code, proc-compose exits with a non-zero code too. The platform sees the failure and restarts the container. Processes with `restart: on-failure` or `restart: always` never trigger this — they keep retrying until context cancellation.

### Clean logs

Cloud log aggregators don't render ANSI color codes. Disable them with the `--no-color` flag, set the `NO_COLOR` environment variable (recognized automatically), or use structured JSON logging:

```sh
proc-compose up --no-color
```

```sh
# or use structured JSON logging for easier parsing by log aggregators:
proc-compose up --log-format json
```

### Container entrypoints

Cloud platforms run a single entrypoint and gate traffic on its readiness
probe. Combine `--silent`, `--wait-ready`, and `--health-port` to make
proc-compose a complete entrypoint with a real liveness signal:

```sh
proc-compose up --silent \
  --log-format json \
  --wait-ready --wait-timeout 90 \
  --health-port 8081
```

The container's HTTP healthcheck targets `:8081/health` (returns 200 only
when every managed process is `running`). `--wait-ready` blocks the
entrypoint until probes pass so platforms don't route traffic to a
half-started stack.

```sh
# or set in the platform's environment variables dashboard:
NO_COLOR=1
```

### Example: Railway

Set these in Railway's environment variables dashboard before deploying:

| Variable | Value |
|----------|-------|
| `NO_COLOR` | `1` |
| `NODE_ENV` | `production` |

Everything else (`PORT`, database URLs, secrets) is set as normal Railway env vars and inherited by all child processes.

## How It Works

```
proc-compose.yml
      │
      ▼
  proc-compose up
      │
      ├── sh -c "npm run dev"         → frontend    │ http://localhost:5173
      ├── sh -c "go run ."            → backend     │ http://localhost:3001
      └── sh -c "merge-port ..."      → merge-port  │ http://localhost:8080  ← merge: section
      │
   Ctrl+C → context cancellation → SIGTERM → SIGKILL (5s) → all processes stop
```

The two tools compose a layered stack:

- **proc-compose** — orchestrates all processes, handles logs, restarts, and shutdown
- **merge-port** — auto-injected from the `merge:` config section; routes `/api` → backend, `/*` → frontend on a single port

Each process runs via `sh -c` in its own process group so child trees (npm → node, nodemon → ts-node, etc.) terminate cleanly. Names are sorted alphabetically for deterministic color assignment.

## Development

```sh
# Build
make build

# Run tests
make test

# Install to ~/.local/bin/
make install
```

## Windows

proc-compose has limited Windows support:

- **IPC** uses named pipes (via `go-winio`)
- **Process tree termination** uses Windows Job Objects (via `go-winjob`)
- **`merge:` sections** require `merge-port` binary on PATH
- **Shell commands** in configs use `cmd /c` — Unix shell syntax (`$VAR`, `./path`) may need adjustment for Windows
- **`stop --timeout`** has no graceful path on Windows: console processes started with `CREATE_NEW_PROCESS_GROUP` cannot receive SIGTERM, so `stop` always uses `taskkill /T /F` regardless of the timeout value
- **`--survive`** uses systemd user units and is unavailable on Windows; use a Service or scheduled task instead

### Install on Windows

```bash
# Via Go
go install github.com/anivaryam/proc-compose/cmd/proc-compose@latest

# Or download a release binary from:
# https://github.com/anivaryam/proc-compose/releases
```

Note: `merge-port` must be installed separately if you use the `merge:` feature.

## License

MIT
