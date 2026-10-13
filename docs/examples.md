# Examples

## Full-stack web app

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

## Full-stack with multiple API prefixes

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

## Microservices

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

## Full-stack with public tunnel

Tunnel runs as a regular process. The [`tunnel`](https://github.com/anivaryam/tunnel) binary spawns a background daemon (managed via a unix socket in `$TMPDIR`); the foreground command is a client attached to it. If proc-compose kills the client, the daemon keeps running and the public URL stays open. On Linux/macOS, wrap the command with `bash` and `trap` so SIGTERM stops the daemon too:

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

The `trap` runs `tunnel stop --name myapp` on shutdown so the daemon and its socket are cleaned up. Without it, `proc-compose stop` (or Ctrl+C) leaves the tunnel exposing your port until you kill it manually. This `bash`/`trap` wrapper is a Linux/macOS example. On Windows, use an explicit Windows process wrapper that stops the tunnel on exit, or stop the tunnel manually.

> **Do not pass `--silent` to `tunnel`.** That flag daemonizes and exits immediately — proc-compose treats the immediate exit as a crash and restarts in a loop. Default (foreground) is correct here.

## Multiple tunnels for multiple services

Same Linux/macOS pattern repeated per service. Each tunnel waits on its upstream via `depends_on` + `ready_when`, so the public URL only opens once the local service serves requests:

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
