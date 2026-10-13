# Deployment

proc-compose works as a single container entrypoint on Railway, Render, Fly.io, and similar platforms.

## Port binding

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

## Exit codes

If a process with `restart: never` exits with a non-zero code, proc-compose exits with a non-zero code too. The platform sees the failure and restarts the container. Processes with `restart: on-failure` or `restart: always` never trigger this — they keep retrying until context cancellation.

## Clean logs

Cloud log aggregators don't render ANSI color codes. Disable them with the `--no-color` flag, set the `NO_COLOR` environment variable (recognized automatically), or use structured JSON logging:

```sh
proc-compose up --no-color
```

```sh
# or use structured JSON logging for easier parsing by log aggregators:
proc-compose up --log-format json
```

## Container entrypoints

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

## Example: Railway

Set these in Railway's environment variables dashboard before deploying:

| Variable | Value |
|----------|-------|
| `NO_COLOR` | `1` |
| `NODE_ENV` | `production` |

Everything else (`PORT`, database URLs, secrets) is set as normal Railway env vars and inherited by all child processes.
