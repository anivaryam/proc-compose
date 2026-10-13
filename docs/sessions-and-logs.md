# Sessions and logs

Named sessions have independent daemon identities, event journals, runtime
directories, and declared endpoint assignments. These features require v1.5.2
or later.

```sh
proc-compose --session main up --silent --wait-ready --json
proc-compose --session feature up --silent --wait-ready --json
proc-compose --session feature status --json
proc-compose --session feature restart worker --wait-ready
proc-compose --session feature stop
```

Declare preferred named ports and use references wherever their resolved values
are needed. A preferred value of `0` requests an ephemeral port; an occupied
preferred port advances to the next available candidate. `http` ports export
`PORT` when no explicit `PORT` environment entry exists. Endpoint references are
resolved consistently in commands, environment values, and readiness URLs:

```yaml
processes:
  api:
    cmd: node server.js
    ports:
      http: 3000
    ready_when:
      http: '${urls.api.http}/health'
  web:
    cmd: npm run dev
    depends_on: [api]
    ports:
      http: 5173
    env:
      API_URL: '${urls.api.http}'
      UPLOAD_ROOT: '${session.dir}/uploads'
    ready_when:
      http: '${urls.web.http}'
```

`${ports.api.http}` expands to the assigned number; `${urls.api.http}` is its
HTTP loopback URL. `${session.name}`, `${session.id}`, `${session.dir}`, and
`${config.dir}` are also available. Commands receive `PROC_COMPOSE_SESSION`,
`PROC_COMPOSE_SESSION_DIR`, and `PROC_COMPOSE_RUN_ID`. Named-session commands
without `dir` run from the config directory. Applications create their own
declared runtime subdirectories or use a setup task to provision them.

Declare exclusive shared-resource labels with a top-level `resources` list:

```yaml
resources:
  - 'postgresql:local:role-ddl'
  - 'next-build:${config.dir}/apps/web'
```

Sessions in the same runtime directory hold these advisory leases for their
entire lifetime. A conflicting session fails before configured commands start.
Separate ports alone do not isolate databases or build directories: use separate
worktrees and application-specific provisioning tasks, and declare shared
resources that must serialize. Named-port leases coordinate proc-compose
sessions; an unrelated program can still race the application's bind, which
must succeed and pass its configured readiness check.

## Logs for coding agents

Start with `proc-compose agent-guide` for an offline, version-matched operating
guide. It covers session ownership, JSON contracts, cursor resumption, failures,
and cleanup. The same [agent guide](../internal/agentguide/GUIDE.md) is embedded in
the binary and linked from `--help`.

Every managed output, lifecycle transition, readiness failure, and cleanup
verdict enters one ordered event model. A separate rotating JSON-lines journal
retains events even when human output is text or uses a custom `--log-file`.

```sh
proc-compose --session feature logs worker --json --level error --max-bytes 4096
proc-compose --session feature logs --json --after start -n 100
proc-compose --session feature logs --json --after '<next_cursor>'
proc-compose --session feature logs worker --generation 2 --stream stderr --json
proc-compose --session feature logs --json --follow
proc-compose --session feature why worker --json
```

An event contains `schema_version`, `id`, `run_id`, `sequence`, `session`,
`time`, `process`, `generation`, `pid`, `kind`, `level`, `code`, `stream`, and
`message`. State events include readiness, dependency names, last errors, and
exit status. Application output is data (`kind: output`); supervisor codes such
as `readiness.failed`, `process.failed`, and `shutdown.verified` are generated
by proc-compose. Health checks (`health_when`) produce `kind: probe` events:
`health.check_failed` (debug, one per failed check, with its detail),
`health.unhealthy` (error, once when three checks in a row have failed),
`health.recovered` (info, the first pass after that), and
`health.check_skipped` (warn, when proc-compose itself cannot launch a `cmd`
check, for example because the crash-recovery record cannot be written; such
checks count neither way). Every health change also
writes a `process.state` event carrying `health`. A health restart is reported
with the usual `process.failed` and `process.restarting` events. Teardown
processes (`mode: teardown`) produce `teardown.started` (info),
`teardown.completed` (info), `teardown.failed` (error, with the reason), and
`teardown.skipped` (error, from `proc-compose`, when termination was not
verified and no teardown ran). Explicit application JSON `level` or `severity` strings are
normalized; ordinary stderr preserves its stream without assuming it is an error.

JSON query pages contain `events`, `next_cursor`, `has_more`, `gap`,
`truncated`, and `event_bytes`. The default is 50 events and 64 KiB of encoded
event data, plus the small page envelope; limits are 1000 events and 1 MiB.
`--after start` reads forward, while the default reads the retained tail.
Resume using the returned cursor. A cursor outside rotated history sets `gap`
and returns a bounded retained tail. Truncated records retain their identity;
the original journal remains intact. Input lines above 1 MiB retain an explicit
prefix, original byte count, and truncation flag without losing subsequent lines.

`--follow --json` and `-n 0 --json` produce newline-delimited pages. Filters
include process, severity, kind, stable code, stream, substring, time, run, and
generation. Follow can be interrupted without stopping the managed session.
Human `logs --follow` uses timestamped, readable rows from those same events.
Historical unstructured logs do not acquire fictitious cursors; querying a
session started by an older binary reports the legacy log path.

`why` produces a bounded evidence packet containing observed state, exit code,
last error, unresolved dependencies, and recent relevant events. Its `source`
distinguishes live state from retained post-stop evidence. It reports observed
failures rather than inferring business-level causes from arbitrary log text.

Reload preserves existing endpoint leases and rejects mode/dependency changes
that require restarting the stack. `restart --wait-ready` waits for the new
generation rather than accepting an earlier incarnation's readiness. The
optional supervisor HTTP server also exposes `/ready` alongside `/health`.

