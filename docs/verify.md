# Verified runs

`verify` turns "start the stack, run the tests, clean up" into one owned
operation with a receipt:

```sh
proc-compose --session checkout-fix verify --json -- npm run test:smoke
proc-compose verify api -- go test ./e2e/...           # select api + its dependencies
proc-compose verify --check-timeout 2m -- sh -c 'npm test && npm run lint'
```

It validates the config, acquires a fresh session (refusing one that is already
running instead of reusing or stopping it), starts the selected processes,
waits for readiness, runs the check, stops everything it started, and records a
receipt under the session runtime directory. The check after `--` is executed
directly, never through an implicit shell. It inherits your environment plus
`PROC_COMPOSE_SESSION`, `PROC_COMPOSE_SESSION_DIR`, `PROC_COMPOSE_RUN_ID`,
`PROC_COMPOSE_ENVIRONMENT_REPORT` (see [Environment reports](#environment-reports)),
and `PROC_COMPOSE_PORT_<PROCESS>_<PORT>` / `PROC_COMPOSE_URL_<PROCESS>_<PORT>` for
every declared named port.

| Exit | Meaning |
|---|---|
| 0 | Startup, check, and verified cleanup all passed; receipt persisted |
| 1 | Startup/readiness, check, [environment](#environment-reports), required process, cleanup, teardown, or receipt failure |
| 2 | Usage, invalid config, or occupied session (including a crashed run whose process groups may still be running; run `stop` first) — nothing was started |
| 130 | Interrupted (cleanup still ran) |

After successful flag parsing, `--json` returns one JSON result object
(`kind: verify.response`, at most 64 KiB; or `verify.compact_error` if the full
result cannot fit). Flag-parsing errors are an exception: for example,
`verify --json --bogus -- true` exits 2 with plaintext stderr and empty stdout.
Check the exit status and stderr; do not assume every failure has JSON to decode.
Process output is kept in the journal only (read it with `why --attempt`); add
`--stream-logs` to also stream it to stderr, as human mode does. Each phase (`startup`, `check`, `cleanup`) keeps its own
status, so a passing check cannot hide a crashed service or an unverified
cleanup. A required service that stops running, restarts, or becomes
`unhealthy` through its `health_when` while the check runs fails the attempt
with `required_failed`. A service without a readiness probe is reported as
`not_configured`/unprobed rather than ready. On Linux and Windows, each passing
`http` or `tcp` readiness observation also records its `owner`: who held the
socket that answered (`owned`, `foreign` with the holder's `pid` and `command`,
or `unattributable` with a `reason`; `null` for other probes). A `foreign`
owner also has a `scope`: `cross_session` when it belongs to another live
session, which `session` names (its `name`, `worktree`, `process`, `port` and
`evidence`), or `outside_session`. A `foreign` owner means the check may have
run against another server on that address; human output prints it as a
warning, naming the session of a `cross_session` one.

An answer from another live session is refused by default: the process does
not become ready on it, so the attempt fails at startup and its readiness
observation's `owner` is the refused `cross_session` answer; a `health_when`
check answered that way counts as a failed check. Pass
`--allow-cross-session` to count such answers again (the owner keeps its
label). An `outside_session` or `unattributable` owner is evidence, not a
verdict, unless the process sets `foreign_listener: reject`, which refuses
every foreign answer the same way (see
[Who answered the probe](configuration.md#who-answered-the-probe) and
[Answers from another session under verify](configuration.md#answers-from-another-session-under-verify)).
Windows refuses only on a port the other session leases, and macOS never
refuses, because it does not attribute answers. `compare` does not refuse
such answers; it only records them.

Source revision and dirty state are recorded as provenance only
(`loaded_code_proven: false`). Default budgets are startup 120s, check 10m,
and an independent 30s cleanup (unmeasured policy defaults; override with
`--startup-timeout`, `--check-timeout`, `--cleanup-timeout`). The cleanup budget starts once, at the first trigger
(check deadline, interruption, or failure), and covers check teardown, stack
teardown, and sealing together.

[Teardown processes](configuration.md#teardown-processes) run inside that same
cleanup budget, ending before the final sealing reserve. The receipt's
`cleanup.teardown` lists each one with its `status` (`passed`, `failed`,
`timed_out`, `handed_off`, `not_run`, or `skipped`), exit code, times, and
`error`; it is an empty list when none is configured. A teardown that does not
pass fails the attempt with `teardown_failed`; `cleanup.status` and
`cleanup.termination_verified` still describe only whether the processes were
terminated.

```sh
proc-compose attempts                      # receipts for this session, newest first
proc-compose attempts latest --json        # one receipt (state classified from evidence)
proc-compose why --attempt latest --json   # that attempt's own events, grouped, bounded
proc-compose sessions                      # every session: owner, URLs, latest attempt, stop command
proc-compose compare --baseline ../main --candidate . -- npm run test:smoke
proc-compose config check --json           # structured config diagnostics with line/column
proc-compose config plan --json            # resolved startup order (a plan, not a run)
proc-compose config schema                 # versioned JSON Schema for proc-compose.yml
```

Receipts and journals live in the runtime directory and may disappear at
logout or reboot. `why --attempt` reads only events stamped with that attempt
and reports missing or rotated history explicitly; it shows temporal order, not
causes. `sessions` only reads metadata (set `PROC_COMPOSE_OWNER` to label who
started a session); retained environments are the ones started with
`up --silent`, and are stopped with the command it prints. `compare` runs the
same check against two worktrees with separate sessions and reports outcome
differences; it neither isolates external data nor attributes differences to
the code change.

## Environment reports

A passing check proves only that the check passed against whatever it reached.
It can reach a dev server from another worktree, a stale build, a database
another session shares, or a fixture left over from an earlier attempt, and
still pass. An environment report lets the check say what it actually
exercised, and keeps that verdict separate from its own pass or fail.

The check receives `PROC_COMPOSE_ENVIRONMENT_REPORT`, a per-attempt file path.
To report, it writes one JSON object there:

```json
{"observations": [
  {"name": "api.build", "status": "matched", "expected": "sha256:9f…", "observed": "sha256:9f…"},
  {"name": "db.fixture_run", "status": "mismatched", "expected": "177406-1791…", "observed": "177311-5402…"}
]}
```

- `status` is `matched`, `mismatched`, or `unobserved` (the evidence could not
  be read). `name` is required and unique; `expected` and `observed` are
  optional strings or `null`. No other fields are allowed, and no field may
  appear twice in one object (names compare case-insensitively). The report
  must be a regular file (not a symlink or FIFO) of at most 8 KiB with at most
  32 observations. Write it atomically (to a temporary file, then rename).
- The result records it under `environment`: `status` is `eligible` (at least
  one observation, all `matched`), `ineligible` (any other status), `unknown`
  with a `reason` (`report_missing`, `report_invalid`, `no_observations`), or
  `not_run` when the check never started. verify removes the file after it has
  read it; the receipt and the `--json` response keep its content.
- An `ineligible` environment fails the attempt with `environment_ineligible`,
  and an invalid report fails it with `environment_unverified`, with or
  without a flag. `check.status` still reports only the check's exit, so a real
  regression reads `check: failed` with `environment: eligible`, and a check
  aimed at the wrong environment reads `environment: ineligible`.
- `--require-environment` also fails the attempt with `environment_unverified`
  when the report is missing or has no observations, so a check that never
  validated its environment cannot pass.
- `compare` reports `not_comparable` when an attempt on either side has either
  environment error, and accepts `--require-environment` for both sides. Each
  attempt shows its `environment_status`.

The observations are the check's own. proc-compose does not inspect the
application, and it does not prove isolation or the loaded code. The one
supervisor observation, the readiness `owner`, covers only the socket that
answered each readiness probe at that moment; the endpoints the check itself
connects to are not observed. Evidence is
stronger when it comes from the application's real paths instead of from the
check's own environment: an `/identity` endpoint that reports the
`PROC_COMPOSE_RUN_ID` its process received and a digest of the code it
loaded, a query that reads a fixture marker back through the application's
own database connection, a worker that stamps the jobs it processes. Echoing
an environment variable proves nothing.

A setup task can give the services and the check one shared, per-attempt
expectation without new configuration:

```yaml
processes:
  prepare:                       # build, reset + seed, write the expectation
    mode: task
    cmd: ./scripts/prepare-env   # stamps the fixture with $PROC_COMPOSE_RUN_ID and
                                 # writes $PROC_COMPOSE_SESSION_DIR/expected.json
    env: &state
      DATABASE_PATH: ${session.dir}/app.db
      QUEUE_DIR: ${session.dir}/queue
  api:
    cmd: ./bin/api
    depends_on: [prepare]
    env: *state
    ports: { http: 3000 }
    ready_when: { http: '${urls.api.http}/health' }
```

The check reads `$PROC_COMPOSE_SESSION_DIR/expected.json`, compares the
API's identity and fixture with it before and after its own assertions, and
writes the report. `PROC_COMPOSE_RUN_ID` changes on every attempt, and a reused
session keeps its directory, so a fixture stamped with an earlier run ID shows
that the reset did not run. Check that a path is inside
`PROC_COMPOSE_SESSION_DIR` before you delete or reset it, in teardown processes
too: they run even when setup refused to start.
[`examples/environment-report`](../examples/environment-report) is a runnable
version with a pilot of seeded faults and acceptance gates.

## Recipes

CI (GitHub Actions step):

```yaml
- name: Verified smoke test
  run: proc-compose verify --json -- npm run test:smoke > verify.json
- if: always()
  run: proc-compose why --attempt latest --json > why.json
- if: always()
  uses: actions/upload-artifact@v4
  with:
    name: verify-evidence
    path: |
      verify.json
      why.json
```

`verify.json` is the receipt: startup, check, environment observations with
their expected values, cleanup, and source provenance. `why.json` is the
attempt's process history, read from the journal while the runtime directory
still exists. Together they stay readable after the runner and its runtime
directory are gone. Add any expectation file your check used; the check runs
with your environment, so it can copy it to a directory you name.

Agent tool action or worktree setup script:

```sh
proc-compose -f "$PWD/proc-compose.yml" --session "task-$TASK_ID" verify --json -- npm test
# on failure: proc-compose -f "$PWD/proc-compose.yml" --session "task-$TASK_ID" why --attempt latest --json
```

For repeated checks against a deliberately retained stack, see the
[agent and browser-test workflow](sessions-and-logs.md#agent-and-browser-test-workflow).
`verify` always needs a fresh session; it does not attach to a retained one.
