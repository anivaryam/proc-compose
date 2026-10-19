# Deployment

## Container entrypoint

proc-compose can be the single entrypoint of a container on Railway, Render,
Fly.io, and similar platforms:

```sh
proc-compose up --silent \
  --log-format json \
  --wait-ready --wait-timeout 90 \
  --health-port 8081
```

- `--wait-ready` blocks the entrypoint until every probe passes, so traffic
  never reaches a half-started stack.
- `--health-port` serves `GET /health`: 200 only when every service is
  `running` or `restarting` and not `unhealthy`, and every task completed;
  otherwise 503. `GET /ready` reports readiness only and ignores health. Point
  the platform's health check at `:8081/health`.
- Use `--log-format json`, `--no-color`, or `NO_COLOR=1` for log aggregators.
- When `merge.port` is unset, the merge-port proxy listens on `$PORT`, the
  port the platform assigns.
- A process with `restart: never` that exits non-zero makes proc-compose exit
  non-zero, so the platform restarts the container. `on-failure` and `always`
  keep retrying inside the container instead.

```yaml
merge:
  client: 5173
  server: 3001          # no port: line, so the proxy uses $PORT
processes:
  frontend:
    cmd: npm run start
    env: { PORT: "5173" }
  backend:
    cmd: node server.js
    env: { PORT: "3001" }
    restart: on-failure
```

## Start on boot (Linux)

```sh
proc-compose up --survive --name myapp --install    # write and enable a systemd user unit
proc-compose up --survive --name myapp > ~/.config/systemd/user/proc-compose-myapp.service   # print only
proc-compose uninstall --name myapp                 # remove it
```

The unit runs `proc-compose up --silent`. macOS and Windows have no
`--survive`; use launchd or a Windows service manager.

## Public URLs with tunnel

The `tunnel` tool (`brokit install tunnel`) can run as a process. Run it in
the foreground, never with `--silent` (it would exit at once and
restart-loop), and on Linux and macOS wrap it so stopping the stack also
stops the tunnel daemon:

```yaml
processes:
  tunnel:
    cmd: bash -c 'trap "tunnel stop --name myapp" EXIT TERM INT; tunnel http 8080 --name myapp; wait'
    depends_on: [merge-port]
    restart: on-failure
    ready_when:
      http: http://localhost:8080/health
```

## Platform differences

| Feature | Linux | macOS | Windows |
|---|---|---|---|
| Shell for `cmd` | `sh -c` | `sh -c` | `cmd /c` |
| Graceful `stop --timeout` | SIGTERM, then SIGKILL | SIGTERM, then SIGKILL | Always `taskkill /T /F` |
| `--survive`, `uninstall` | systemd | No | No |
| Probe owner attribution | Yes | No (`unattributable`) | Yes |
| `verify` refuses another session's answer | Yes | Never | Leased ports only |
| Crash recovery by `stop` | Proves and signals groups | Never signals live groups | Job Objects end with the daemon |
