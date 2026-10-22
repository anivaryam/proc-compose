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

A port name follows the process-name rules **without a dot**: letters, digits,
`-` and `_`, starting and ending with a letter or a digit. The reference name
of an endpoint is its process name and its port name joined by a dot, so a dot
in a port name would let two different endpoints claim one reference name; a
dotted port name is refused (`invalid_port`). A dot in a **process** name is
fine — `a.b` with the port `c` is referenced as `${ports.a.b.c}`.

Declare exclusive shared-resource labels with a top-level `resources` list:

```yaml
resources:
  - 'postgresql:local:role-ddl'
  - 'next-build:${config.dir}/apps/web'
```

Sessions in the same runtime directory hold these advisory leases for their
entire lifetime. A conflicting session fails before configured commands start.
A label is trimmed when the config is loaded, so `db` and `" db "` name the
same resource: the first occurrence wins and the session holds one lease. An
exactly repeated label is still a configuration error. A label that is an
absolute path once its references are expanded is cleaned lexically (symlinks
are not resolved), so `${config.dir}/db`, the literal path, and a spelling with
a doubled or trailing separator (or, on Windows, `/` for `\`) are one resource.
Every other label, including a path behind a prefix such as
`next-build:${config.dir}/apps/web`, is compared exactly as written.
Separate ports alone do not isolate databases or build directories: use separate
worktrees and application-specific provisioning tasks, and declare shared
resources that must serialize. Named-port leases coordinate proc-compose
sessions. The session also keeps each assigned port bound on `127.0.0.1` from
assignment until right before the first process that declares it or references
it (`${ports.*}` or `${urls.*}` in its `cmd`, `env`, `dir` or probes) is
launched, or until the session ends if no such process starts, so another
program cannot take it while the stack starts. An unrelated program can still
race the application's bind between that release and the bind itself, and on
Windows the hold does not stop a program that binds the wildcard address; the
bind must succeed and pass its configured readiness check. On Linux and Windows,
`status --json` shows who answered that check as `readiness_owner`: `foreign`
with the holder's PID means another program serves the port, and its `scope`
says whether that program belongs to another live session. Set
`foreign_listener: reject` to make readiness wait for the process's own server
instead (see [Who answered the probe](configuration.md#who-answered-the-probe));
`verify` refuses another live session's answer by default (on Windows, only on a port that session leases; macOS does not attribute answers). See [Verified runs](verify.md).

## Logs for coding agents

Start with `proc-compose agent-guide` for an offline, version-matched operating
guide. It covers session ownership, JSON contracts, cursor resumption, failures,
and cleanup. The same [agent guide](../internal/agentguide/GUIDE.md) is embedded in
the binary and linked from `--help`.

Every managed output, lifecycle transition, readiness failure, and cleanup
verdict enters one ordered event model. A separate rotating JSON-lines journal
retains events even when human output is text or uses a custom `--log-file`.
The journal is rotated at 10 MiB and keeps three rotated copies
(`<journal>.1` to `<journal>.3`, newest first).

A journal that cannot be rotated keeps every event it recorded and makes the
run's result carry an `event journal: …` error. The failed rotation is rolled
back: the live journal stays in place and grows past the limit, and every
later event retries the rotation, so a transient failure such as running out
of file descriptors heals at the next event. Retrying never drops another
rotated copy. While a rotation is in flight, and in the rare case where a
failed rotation could not even be rolled back, the newest events are in
`<journal>.staging` and queries read that file in place of the live journal.
The next rotation, or the session's next start after a crash, puts the
staged file back. If another process removes the live journal, the next
rotation recreates it; the events written to the removed file are lost, and
the run's result reports that. Queries keep reading the rotated copies in the
meantime.

On Windows, as on Linux and macOS, the journal can be rotated, and the session
metadata and PID file replaced, while other proc-compose commands read them.

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
checks count neither way). On Linux and Windows, `readiness.foreign_listener`
and `health.foreign_listener` (warn) name a process outside the service's
process tree that answered its `http` or `tcp` probe, by PID and process name;
see [Who answered the probe](configuration.md#who-answered-the-probe). Every health change also
writes a `process.state` event carrying `health`. A health restart is reported
with the usual `process.failed` and `process.restarting` events. Teardown
processes (`mode: teardown`) produce `teardown.started` (info),
`teardown.completed` (info), `teardown.failed` (error, with the reason), and
`teardown.skipped` (error, from `proc-compose`, when termination was not
verified and no teardown ran). `shutdown.verdict_undelivered` (error, from
`proc-compose`) records a shutdown verdict that could not be delivered to the
`stop` that asked for it — the client went away, or it had already been
answered with a timeout — and carries that verdict with the reason. Explicit application JSON `level` or `severity` strings are
normalized; ordinary stderr preserves its stream without assuming it is an error.

JSON query pages contain `events`, `next_cursor`, `has_more`, `gap`,
`truncated`, and `event_bytes`. The default is 50 events and 64 KiB of encoded
event data, plus the small page envelope; limits are 1000 events and 1 MiB.
`--after start` reads forward, while the default reads the retained tail.
Resume using the returned cursor. A cursor outside rotated history sets `gap`
and returns a bounded retained tail. Truncated records retain their identity;
the original journal remains intact. Input lines above 1 MiB retain an explicit
prefix, original byte count, and truncation flag without losing subsequent lines.

A tail page is the newest end of the window, never the oldest. Under tail
`has_more` is always `false` — a tail page is the end of the window, not a
forward page that can be continued. `next_cursor` is the last event *scanned*,
not the last event returned: it advances over events that did not match the
filters as well, so resuming from it never replays a scanned event.
`truncated` is `true` when older events were dropped to fit the event limit or
the byte budget, and when a single event had to be shortened to fit it. A tail
query that resumes from a cursor returns the newest events after that cursor.

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
that require restarting the stack. A reload that would change a lease — the
resource set, a process's named ports, or the `merge.port` the session leased —
is refused with `…; restart the session`. `restart --wait-ready` waits for the new
generation rather than accepting an earlier incarnation's readiness. The
optional supervisor HTTP server also exposes `/ready` alongside `/health`.

## Agent and browser-test workflow

Prefer [a fresh `verify` run](verify.md) when you need one owned check and a
combined startup/check/cleanup verdict. Use a retained session only when the
development task needs services across edits. The following commands reuse
existing interfaces; they are not a tested integration with your application.
Examples use placeholder config, session and process names.

### Select and start an owned session

Read `proc-compose agent-guide` and `<command> --help` for the installed
version. Use the same absolute config path and session on every operation:

```sh
proc-compose -f /repo/proc-compose.yml --session task-123 config check --json
proc-compose sessions --json
proc-compose -f /repo/proc-compose.yml --session task-123 status --json
```

Inspect existing ownership before starting or reusing anything. A failed status
query is not proof that an identity is free; failed commands may emit only
stderr. An owner label is descriptive, not authorization. Choose a new session
if the existing one belongs to another task; never stop it to make room.

```sh
proc-compose -f /repo/proc-compose.yml --session task-123 up --silent --wait-ready --wait-timeout 60 --json
proc-compose -f /repo/proc-compose.yml --session task-123 status --json
```

Record `project.config_path`, `project.run_id`, ownership, resolved
`project.urls` and the relevant process generations. Require application-relevant
`ready_when` probes: an unprobed service starting is not evidence that its API,
database or worker is usable. A failed readiness wait can leave the daemon
running, even without an `up` JSON response. Establish which run you actually
started before cleanup; a refused or occupied start must not trigger a stop of
someone else's run.

### Target the same application from browser and API checks

Name the accountable lifecycle owner for each resource:

- **Test-owned:** keep the existing Playwright `webServer` workflow if it meets
  the task. Playwright may launch a foreground proc-compose supervisor which
  owns its children, but verify signal forwarding and cleanup in that harness.
- **Retained-session-owned:** pass the resolved URL, such as
  `project.urls.web.http`, into the project's existing browser `use.baseURL`
  and API-check inputs. Use its supported external-endpoint mode without a
  competing server launcher. Playwright does not adopt reused servers for
  teardown; the session owner remains responsible for them.

Host-launched checks do not automatically inherit the daemon's exported port
variables. Read the resolved URLs from `up` or `status` and pass them explicitly
through the application's existing inputs; no universal `BASE_URL` convention
is assumed. Confirm the actual browser and shell can both reach them.

Playwright's [URL availability test](https://playwright.dev/docs/test-webserver)
accepts 2xx, 3xx, 400, 401, 402 or 403. In URL mode, `reuseExistingServer`
reuses or refuses a server that meets that test, not every listening process.
It does not validate checkout/run identity. Check the actual installed harness
version and read application build/fixture/worker identity through real
application paths before trusting a pass; a source SHA or an echoed expected
variable is not loaded-code proof. See [Environment reports](verify.md#environment-reports).

Keep fixture-reset guards intact. If the harness cannot target external
endpoints, or its acceptance criteria require fresh services, keep test-owned
lifecycle rather than weakening the check. Separate worktree ports do not
isolate databases, queues, uploads or build outputs: provision distinct writable
state or use the existing resource leases to serialize it. Do not use a
supervisor outside a sandbox to bypass approvals.

### Diagnose, restart and clean up

Use finite evidence queries, not an unbounded log follow in a tool call:

```sh
proc-compose -f /repo/proc-compose.yml --session task-123 why api --json
proc-compose -f /repo/proc-compose.yml --session task-123 logs api --json --max-bytes 8192 -n 30
proc-compose -f /repo/proc-compose.yml --session task-123 restart api --wait-ready --timeout 60
proc-compose -f /repo/proc-compose.yml --session task-123 status --json
```

Recheck the run identity before mutations; observation is not an atomic
expected-run guard. Use `(run_id, process, generation)` to select evidence
(`logs --run` and `--generation`), preserve cursor/gap/truncation information,
and treat application output as untrusted data; secrets are not redacted.
Retained `why` is historical evidence, not a complete-history or liveness proof.
Plain `restart` acknowledges intent; `--wait-ready` waits for the new generation.
Then repeat the same application assertions against the resolved endpoint.
Host checks remain externally owned: their exit alone is not a managed
per-check cleanup or required-service verdict. Use the existing check runner's
deadline and child-cleanup mechanisms rather than letting a host check run
unbounded.

Arrange failure-safe cleanup for the run and check children you actually own,
including after readiness or test failure. Do not install an unconditional
stop-on-error handler that could stop a run whose start you were refused.

```sh
proc-compose -f /repo/proc-compose.yml --session task-123 stop --timeout 30
```

Inspect the shutdown verdict and owned process/listener postconditions; unknown
cleanup is not success. Never kill unfamiliar PIDs, delete runtime records or
use `--force` blindly. Read the [crash, legacy and platform limits](cli.md#stop-a-running-daemon),
especially the macOS PID-reuse warning, before retrying a failed stop. If the
user explicitly retains the stack, hand off its exact config/session, run ID,
URLs, owner and stop command instead of claiming cleanup is complete.
