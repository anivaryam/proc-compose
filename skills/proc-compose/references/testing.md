# Testing with proc-compose

Three ways to run tests, from the fastest feedback to the strongest verdict.

## Local development loop

### Test watcher as a process

Unit tests rerun on every save, in the same log stream as the servers:

```yaml
processes:
  api:
    cmd: npm run dev
    dir: ./server
    ready_when:
      http: http://localhost:3000/health
  web:
    cmd: npm run dev
    dir: ./client
    depends_on: [api]
  test:
    cmd: npx vitest              # any watch-mode runner: jest --watch, pytest-watch, gotestsum --watch
    dir: ./client
```

```sh
proc-compose up               # servers and watcher
proc-compose up api web       # without the watcher
proc-compose logs test -n 50  # latest test output
```

Keep the watcher a service (the default mode). A `mode: task` that exits
non-zero fails the whole stack, which would stop the dev servers on the first
failing test.

### Tests against a running stack

For end-to-end tests (Playwright, Cypress, API suites) that you run many times
while editing:

```sh
proc-compose up --silent --wait-ready --json   # read project.urls from the result
npx playwright test                            # baseURL = the web URL
proc-compose restart api --wait-ready          # after a change the server does not hot-reload
proc-compose why api --json                    # when a test fails because a server did
proc-compose stop                              # when done
```

- Point the test runner at the URLs from `up --json` or `status --json`.
  Tests launched from your shell do not get the daemon's port variables, so
  pass the URL through the project's own input (Playwright `use.baseURL`, an
  env var the suite already reads).
- In Playwright, either keep its `webServer` and let it launch a foreground
  `proc-compose up` (check that it stops cleanly), or use the retained stack
  with `reuseExistingServer: true`. Playwright never stops a reused server;
  you do, with `proc-compose stop`.
- A failed `up --wait-ready` leaves the daemon running. Stop it on every exit
  path (a shell `trap`, a `finally` block).
- Agents: use a named `--session` and an absolute `-f` for every command, and
  never stop a session you did not start.

## verify

One owned operation: validate the config, acquire a fresh session, start the
selected processes, wait for readiness, run the check, stop everything it
started, and write a receipt.

```sh
proc-compose verify -- npm test
proc-compose verify api -- go test ./e2e/...                  # api plus its dependencies
proc-compose --session task-1 verify --json -- npm run test:smoke
proc-compose verify --check-timeout 2m --dir web -- npx playwright test
proc-compose verify -- sh -c 'npm test && npm run lint'      # the check runs without a shell
```

| Flag | Default | Meaning |
|---|---|---|
| `--startup-timeout` | 2m (1s-10m) | Startup and readiness budget |
| `--check-timeout` | 10m (1s-1h) | Check budget |
| `--cleanup-timeout` | 30s (30s-2m) | Independent cleanup budget, started once at the first trigger |
| `--dir` | config directory | Check working directory |
| `--json` | | One result object on stdout (at most 64 KiB) |
| `--stream-logs` | | With `--json`, also stream process output to stderr |
| `--require-environment` | | Fail unless the check reports an eligible environment |
| `--allow-cross-session` | | Accept probe answers from another live session |

Exit codes: 0 startup, check, and cleanup all passed and the receipt was
saved; 1 something failed (startup, readiness, check, required process,
environment, cleanup, teardown, receipt); 2 nothing was started (usage,
invalid config, session already running); 130 interrupted, cleanup still ran.

The check inherits your environment plus:

| Variable | Value |
|---|---|
| `PROC_COMPOSE_URL_<PROCESS>_<PORT>` | URL of each named port, for example `PROC_COMPOSE_URL_API_HTTP` |
| `PROC_COMPOSE_PORT_<PROCESS>_<PORT>` | Port number |
| `PROC_COMPOSE_SESSION`, `PROC_COMPOSE_SESSION_DIR` | Session name and runtime directory |
| `PROC_COMPOSE_RUN_ID` | Changes on every attempt |
| `PROC_COMPOSE_ENVIRONMENT_REPORT` | Where the check may write an environment report |

Names are uppercased and other characters become `_`.

Reading the `--json` result:

- `status`: `passed`, `failed`, or `interrupted`. Only `passed` means the
  check passed against a ready stack with verified cleanup.
  `kind: verify.compact_error` (result too large) is never a pass.
- `startup.status`, `check.status` and `check.exit_code`, `cleanup.status`,
  `cleanup.teardown`, and `errors` as `{code, count}`. Stable codes include
  `usage_error`, `invalid_configuration`, `session_occupied`,
  `resource_unavailable`, `startup_failed`, `startup_timeout`,
  `check_start_failed`, `check_exit_nonzero`, `check_timeout`,
  `required_process_failed`, `interrupted`, `cleanup_unverified`,
  `teardown_failed`, `receipt_persistence_failed`, `result_size_limit`,
  `evidence_unavailable`, `environment_ineligible`, and
  `environment_unverified`.
- `check.stdout` and `check.stderr` hold up to 4 KiB each. Process output
  stays in the journal; read it with `why --attempt`.
- A required service that exits, restarts, or turns unhealthy during the check
  fails the attempt. Readiness `not_configured` means the process had no probe.
- A readiness `owner` of `foreign` means another server may have answered;
  treat the result like a wrong environment.
- `source` records the git revision and dirty state as provenance only
  (`loaded_code_proven: false`).

### Environment reports

A passing check proves only that it passed against whatever it reached. The
check can say what it actually exercised by writing JSON to
`PROC_COMPOSE_ENVIRONMENT_REPORT` (atomically: temp file, then rename):

```json
{"observations": [
  {"name": "api.build", "status": "matched", "expected": "sha256:9f...", "observed": "sha256:9f..."},
  {"name": "db.fixture_run", "status": "mismatched", "expected": "run-a", "observed": "run-b"}
]}
```

- `status` per observation: `matched`, `mismatched`, or `unobserved`. `name`
  is required and unique. At most 32 observations and 8 KiB, regular file
  only, no extra fields.
- The result's `environment.status` is `eligible` (all matched),
  `ineligible` (fails with `environment_ineligible`), `unknown` with a
  `reason`, or `not_run`.
- `check: failed` with `environment: eligible` is a real regression.
  `environment: ineligible` means the check hit the wrong build, endpoint, or
  data; fix that before reading the check result.
- Good evidence comes from the application's own paths (an identity endpoint
  that returns the `PROC_COMPOSE_RUN_ID` it received, a fixture marker read
  through the app's database). Echoing an environment variable proves
  nothing. A setup task can stamp fixtures with `PROC_COMPOSE_RUN_ID` and
  write the expectation to `$PROC_COMPOSE_SESSION_DIR/expected.json`.

## Receipts and attempt evidence

```sh
proc-compose attempts                      # receipts for this session, newest first
proc-compose attempts latest --json        # one receipt
proc-compose why --attempt latest --json   # that attempt's own events, grouped and bounded
proc-compose why --attempt latest api --json
```

- A receipt that never finished is `active` only while its owner still runs
  the session, `abandoned` only with proof the owner is gone and the lease is
  free, and otherwise `indeterminate`. None of these is a pass.
- `why --attempt` reports per-process state transitions and output (identical
  consecutive lines grouped with a `count`), `first_id` and `last_id` for
  `logs --after`, and `history_missing`, `gap`, and `truncated` when evidence
  is incomplete. It shows order, not causes.
- Receipts live in the runtime directory and can disappear at logout or
  reboot. Save the `--json` result if you need it later.

## compare

Runs the same check against two worktrees in separate sessions and reports
outcome differences:

```sh
proc-compose compare --baseline ../main --candidate . --json -- npm run test:smoke
proc-compose compare --baseline /tmp/base --candidate . --repeat 3 -- go test ./e2e
```

- Takes the same budget, `--dir`, `--stream-logs`, and
  `--require-environment` flags as `verify`, plus `--repeat` (1-5 attempts per
  side, alternating order).
- The check also gets `PROC_COMPOSE_COMPARE_SIDE` (`baseline` or
  `candidate`) and `PROC_COMPOSE_COMPARE_REPETITION`.
- `status: comparable` (exit 0) means both sides started, ran the check, and
  cleaned up; read `outcomes_equal` and `differences` for the answer.
  `not_comparable` (exit 1) includes any side with an environment error.
- Differences are observations, not attributions: compare does not isolate
  external data such as a shared database.

## CI recipe

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

`verify.json` is the receipt and `why.json` the attempt's process history;
together they stay readable after the runner's runtime directory is gone.
