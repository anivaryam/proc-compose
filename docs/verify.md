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
causes. It never scans the whole journal: a receipt always names its attempt
(see the receipt contract below), and a record without a run id (`null` or
empty) has no journal scope, so it reports `history_missing` with the
limitation `journal_unavailable`.
`sessions` only reads metadata (set `PROC_COMPOSE_OWNER` to label who
started a session); retained environments are the ones started with
`up --silent`, and are stopped with the command it prints. `compare` runs the
same check against two worktrees with separate sessions and reports outcome
differences; it neither isolates external data nor attributes differences to
the code change.

A receipt is a complete attempt record, stored as
`attempts/<attempt_id>.json`: its `attempt_id` is the ULID that names the
file, and its `state`, `status`, `exit_code` and `precedence` are one of the
combinations below. `precedence` names the rule that decided `status` and
`exit_code`.

| `state` | `status` | `exit_code` | `precedence` | Recorded when |
|---|---|---|---|---|
| `active` | `not_run` | 0 | `no_verdict` | Before the attempt starts any process. It has no verdict yet, and the exit code means nothing until the terminal receipt replaces it. |
| `completed` | `passed` | 0 | `all_required_phases_passed` | Startup, check, environment, cleanup, teardown, and the receipt all passed. |
| `completed` | `failed` | 1 | `failure_without_interruption` | At least one error code was recorded, without an interruption. |
| `completed` | `interrupted` | 130 | `interruption` | The attempt was interrupted; this wins over any failure. Cleanup still ran. |

A refusal before the session is acquired (exit 2: usage, invalid config,
occupied session) creates no receipt; its `--json` response has
`status: failed` and `precedence: pre_acquisition_failure`. Receipts written
by earlier releases while an attempt ran say `status: failed` with
`exit_code: 0` and an empty `precedence`; they are still read. A receipt
never records `abandoned` or `indeterminate`: `attempts` derives those, and
`active`, from an active receipt's owner evidence, and lists the recorded
state as `recorded_state`. `run_id` is not part of the contract; it only
scopes the journal evidence.

A file that does not satisfy the contract is refused like an unreadable one:
`attempts` skips it, `attempts latest` and `why --attempt latest` take the
newest receipt that does, and the file is preserved rather than pruned.
Naming it explicitly (`attempts <id>`) reports why it was refused.

## Comparisons

`compare` runs the same check against two worktrees, one attempt per side per
repetition, alternating which side goes first. The verdict is
`outcomes_equal`:

- `true` — every repetition pair agreed: repetition *i* of the baseline
  against repetition *i* of the candidate, on complete output, with each
  side's own repetitions agreeing too.
- `false` — a difference was proven: a compared field differs between every
  baseline repetition and every candidate repetition (with one repetition per
  side, between the two attempts). A proven difference is `false` whatever
  limitations also apply.
- `null` — no difference was proven, and the data cannot show that there is
  none. The reasons are named in `limitations`: `nondeterministic_within_side`
  when a side's own repetitions disagree, and `output_truncated` when an
  output was truncated; both are listed when both apply.

`status` stays `comparable` and the exit status stays 0 for all three; only
the verdict is withheld. `differences` and `sides[*].attempts[]` remain the
observations either way: `differences` lists every field that differs in some
repetition pair, proven or not. `nondeterministic_within_side` and
`output_truncated` are listed whenever they apply, beside `false` too.

When a side's repetitions disagree, which repetitions ran together is
incidental, so a field proves a difference only if it differs in every
pairing. A baseline that always passes against a candidate that fails with
varying exit codes is `false`; repetitions that differ in some pairings but
agree in others are `null`.

Each stream of the check's output is captured up to 4 KiB, so `stdout_sha256`
and `stderr_sha256` digest the captured prefix only, and `output_truncated`
marks an attempt with a stream that was cut off. The overall status, check
status, and exit code are recorded in full, so a difference in them is proven
however much output was cut. A truncated output proves a difference only where
the two captures disagree at a byte both hold: equal captures cannot show what
followed the cap, and neither can a capture that is a prefix of the other
(including one the 64 KiB result limit emptied).

`sides[*].median_check_ms` is the median of that side's check durations: the
middle value for an odd number of samples, and the average of the two middle
values for an even number, rounded half up.

The check receives `PROC_COMPOSE_COMPARE_SIDE` (`baseline` or `candidate`)
and `PROC_COMPOSE_COMPARE_REPETITION` (1-based) so it can select explicitly
isolated data. The tool sets both, and they win over a variable of the same
name supplied by the caller.

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
