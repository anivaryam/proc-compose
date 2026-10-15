# Environment report example and pilot

A small stateful application (an order API and a queue worker sharing a
SQLite database and a file queue) whose `verify` check proves which
environment it exercised, with [environment reports](../../docs/verify.md#environment-reports).
It needs `python3` (standard library only) and `proc-compose`.

| File | Role |
|---|---|
| `src/app.py` | The application. `/identity` reports, from the running process, its run ID, a digest of the code it loaded, its database and queue paths, and the fixture stamp read through its own connection. The worker stamps every order it processes. |
| `prepare.py` | Setup task: builds (copies `src/` to `build/`), refuses state outside the session directory, resets and seeds the fixture with `PROC_COMPOSE_RUN_ID`, and writes `expected.json` to the session directory. `prepare.py release` is the teardown, with the same refusal. |
| `check.py` | `plain` asserts that an order of 3 widgets totals 750. `env-bound` compares the application's identity with `expected.json` before and after that assertion, checks the worker that processed the order, and writes the report. It always reports every observation it requires; evidence it cannot read is `unobserved`. |
| `proc-compose.yml` | `prepare`, `api`, `worker`, and a `release` teardown, all with state under `${session.dir}`. |
| `pilot.py` | Runs seeded faults in two modes, checks acceptance gates, and exports evidence. |
| `pilot_sync.py` | Orders two concurrent sessions through marker files; inert unless the pilot sets its variables. |
| `read_evidence.py` | Reads an exported bundle without proc-compose or its runtime directory. |

Run the check once:

```sh
cd examples/environment-report
proc-compose --session demo verify --require-environment -- python3 check.py env-bound
```

## Pilot

```sh
python3 examples/environment-report/pilot.py --bin proc-compose [--baseline-bin OLD] [--work DIR]
```

Every scenario runs in `baseline` mode (`check.py plain`, optionally with an
older binary) and in `env-bound` mode (`check.py env-bound` with
`--require-environment`). The pilot uses its own runtime directory
(`XDG_RUNTIME_DIR`), and exits 1 if any gate fails. It writes every JSON
response to `DIR/results/<mode>/` and the gate results to `DIR/gates.json`.

| Scenario | Fault | Baseline | Env-bound gate |
|---|---|---|---|
| S1 | none | passes | passes, `eligible` |
| S2 | the build has a real regression | check fails | check fails, `eligible` |
| S3a | the check reaches another session's dev server | passes, hiding S2's regression | `environment_ineligible` (instance, db) |
| S3b | the build step is skipped; a stale build runs | passes, hiding the regression | `environment_ineligible` (`before.build`) |
| S3c | the config hardcodes a port the dev server already holds | readiness passes against the dev server; the check passes against it too, and only the new api's later crash fails the attempt | `environment_ineligible`; on Linux the api's readiness `owner` is `foreign`, naming the dev server's process |
| S3d | S3c with `foreign_listener: reject` (env-bound mode only) | — | `startup_failed`; the check never runs, and the api's readiness `owner` names the refused dev server process |
| S4a | state configured outside the session | `startup_failed` | `startup_failed`; setup and teardown both refuse, and the foreign file is untouched |
| S4b | two sessions share state; the second resets it during the first's check (both orders) | both pass | the first is `ineligible` on `after.fixture_run` only, with `check: passed`; the second, which then runs alone, passes as `eligible` |
| S4c | the same order, each session with its own state | both pass | both `eligible`, on different databases |
| S5 | one session name reused; the second attempt skips the reset; a third resets | passes | the stale reuse is `ineligible` (`before.fixture_run`); the reset reuse is `eligible` |
| S6a | preparation fails | `startup_failed` | `startup_failed`, environment `not_run` |
| S6b | the application has no identity endpoint | passes | `ineligible` (unobserved) |
| S7 | the teardown fails | `teardown_failed` | `teardown_failed`; termination verified; `eligible` |
| S8 | `compare` of a good worktree and the stale-build one | `comparable` | `not_comparable` |
| S9 | the plain check under `--require-environment` | — | `environment_unverified` |

S4b and S4c use the same ordering in both modes, enforced by marker files
rather than sleeps: the second session resets its state only after the first
check's assertion, the first check validates again only after that reset, and
the second check starts only after the first attempt ends. A gate checks the
marker times. The first session keeps its state at teardown, so the second
isolates the reset; a teardown that deletes shared state is S4a's fault.

Every run must also report verified termination. The baseline gates require
S3a, S3b, S5, and S6b to pass, S3c's readiness to pass, and S8 to be
`comparable`: if the baseline does not show the problem, the pilot proves
nothing. On Linux, a gate also requires every other passing readiness probe
in env-bound mode to be `owned`, so attribution reports no false `foreign`.

## Evidence that outlives the runtime directory

Receipts and journals live in the runtime directory. The pilot exports two
attempts (one eligible, one stale build) with existing commands only:

```sh
EVIDENCE_DIR=bundle/a proc-compose --session s verify --json --require-environment -- python3 check.py env-bound > bundle/a/verify.json
proc-compose --session s why --attempt <attempt_id> --json > bundle/a/why.json
```

`check.py` copies `expected.json` into `EVIDENCE_DIR`. The pilot then removes
its runtime directory, confirms that `why --attempt` no longer finds the
attempt, and reads the bundle with `read_evidence.py`, which prints each
attempt's expectation, observations, check result, cleanup, process history,
and evidence gaps. A gate also sets the diagnosis `gap` and `truncated` flags
on copies of the bundle and requires the reader to report each.

## Limits

The observations are the check's own: proc-compose records and enforces them
but cannot see inside the application. The pilot is one synthetic
application, and it has been run on Linux only.
