# proc-compose

[![CI](https://github.com/anivaryam/proc-compose/actions/workflows/ci.yml/badge.svg)](https://github.com/anivaryam/proc-compose/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/anivaryam/proc-compose)](https://github.com/anivaryam/proc-compose/releases/latest)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

Run every process in your stack from one YAML file — one log stream, ordered startup, readiness checks, restarts, and a clean shutdown with one Ctrl+C.

![proc-compose monitor TUI](docs/monitor.gif)

`proc-compose` is a local process orchestrator for development and small deployments. It is intentionally smaller than Docker Compose: no containers, images, networks, or volumes. Commands run on your machine exactly as they would in a terminal, but share one lifecycle, one config, and one control surface.

Use it when a project needs more than one command to run:

- a frontend plus an API server
- several microservices plus workers
- a local stack that needs ordered startup and one-time setup tasks
- a single-container deployment that still runs multiple child processes
- isolated stacks per branch or worktree, driven by humans or coding agents

## Install

```sh
# Linux / macOS
curl -sSL https://raw.githubusercontent.com/anivaryam/proc-compose/main/install.sh | bash

# Any platform with Go
go install github.com/anivaryam/proc-compose/cmd/proc-compose@latest

# With brokit
brokit install proc-compose
```

Prebuilt binaries for Linux, macOS, and Windows are on the [releases page](https://github.com/anivaryam/proc-compose/releases). See [Installation](docs/installation.md) for all options and update notifications.

## Quick start

Let proc-compose scan your project and propose a config:

```sh
proc-compose bootstrap          # print a generated config
proc-compose bootstrap --write  # write proc-compose.yml
proc-compose up                 # start the stack
```

Or write `proc-compose.yml` yourself:

```yaml
processes:
  migrate:
    mode: task                  # runs once; dependents wait for exit 0
    cmd: npm run migrate
  api:
    cmd: npm run dev
    dir: ./server
    depends_on: [migrate]
    restart: on-failure
    ready_when:
      http: http://localhost:3000/health
  web:
    cmd: npm run dev
    dir: ./client
    depends_on: [api]
```

```
$ proc-compose up

proc-compose starting 3 processes

  api     configured
  migrate configured
  web     configured

  Press Ctrl+C to stop all

migrate │ applied 3 migrations
api     │ listening on :3000
api     │ ready
web     │ ready
web     │ VITE v5.4.2  ready in 237ms
```

Run it in the background and control it from any terminal:

```sh
proc-compose up --silent --wait-ready   # daemonize, return once everything is ready
proc-compose monitor                    # live TUI
proc-compose status                     # one-shot status table (--json for scripts)
proc-compose logs api --follow          # follow one process
proc-compose restart api                # restart one process
proc-compose stop                       # stop everything
```

## Features

- **One config, one log stream** with stable, color-coded process names
- **Services and tasks**: long-running processes and one-shot setup steps
- **Ordered startup** via `depends_on` and `http`, `tcp`, `cmd`, or `log` readiness probes, plus optional ongoing health checks
- **Restart policies** with exponential backoff and restart limits
- **Verified shutdown** of whole process trees on Linux, macOS, and Windows
- **Background daemon** with a TUI monitor, status, restart, reload, and stop
- **Named sessions** for isolated stacks per branch, with assigned ports
- **Structured logs** with cursors, filters, and failure evidence (`why`)
- **Verified runs**: start a stack, run a check, clean up, and keep a receipt
- **Project scanning** with `doctor` and `bootstrap`
- **Single-port proxying** for frontend + backend via [merge-port](https://github.com/anivaryam/merge-port)

See [FEATURES.md](FEATURES.md) for a short tour of each with examples.

## Documentation

| Guide | Covers |
|---|---|
| [Installation](docs/installation.md) | Install methods, optional companions, update notifications |
| [Configuration](docs/configuration.md) | Every config field, modes, restarts, readiness, dependencies, merge-port |
| [CLI reference](docs/cli.md) | Commands, flags, environment variables, daemon files, monitor keys |
| [Sessions and logs](docs/sessions-and-logs.md) | Named sessions, named ports, resource leases, structured logs, `why` |
| [Verified runs](docs/verify.md) | `verify`, receipts, `attempts`, `compare`, CI recipes |
| [Examples](docs/examples.md) | Full-stack, microservices, public tunnels |
| [Deployment](docs/deployment.md) | Containers and platforms such as Railway, Render, and Fly.io |
| [Platform support](docs/platforms.md) | Linux, macOS, and Windows differences; how it works |
| [Agent guide](internal/agentguide/GUIDE.md) | Operating guide for coding agents (also `proc-compose agent-guide`) |

## Contributing

Bug reports and pull requests are welcome. See [CONTRIBUTING.md](CONTRIBUTING.md) for the development setup and checks, and [CHANGELOG.md](CHANGELOG.md) for release history.

## License

[MIT](LICENSE)
