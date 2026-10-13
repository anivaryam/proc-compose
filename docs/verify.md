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
`PROC_COMPOSE_SESSION`, `PROC_COMPOSE_SESSION_DIR`, `PROC_COMPOSE_RUN_ID`, and
`PROC_COMPOSE_PORT_<PROCESS>_<PORT>` / `PROC_COMPOSE_URL_<PROCESS>_<PORT>` for
every declared named port.

| Exit | Meaning |
|---|---|
| 0 | Startup, check, and verified cleanup all passed; receipt persisted |
| 1 | Startup/readiness, check, required process, cleanup, teardown, or receipt failure |
| 2 | Usage, invalid config, or occupied session (including a crashed run whose process groups may still be running; run `stop` first) — nothing was started |
| 130 | Interrupted (cleanup still ran) |

With `--json`, stdout is exactly one JSON object (`kind: verify.response`, at
most 64 KiB; or `verify.compact_error` if the full result cannot fit). Process
output is kept in the journal only (read it with `why --attempt`); add
`--stream-logs` to also stream it to stderr, as human mode does. Each phase (`startup`, `check`, `cleanup`) keeps its own
status, so a passing check cannot hide a crashed service or an unverified
cleanup. A required service that stops running, restarts, or becomes
`unhealthy` through its `health_when` while the check runs fails the attempt
with `required_failed`. A service without a readiness probe is reported as
`not_configured`/unprobed rather than ready. Source revision and dirty state are
recorded as provenance only (`loaded_code_proven: false`). Default budgets are
startup 120s, check 10m, and an independent 30s cleanup (unmeasured policy
defaults; override with `--startup-timeout`, `--check-timeout`,
`--cleanup-timeout`). The cleanup budget starts once, at the first trigger
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

## Recipes

CI (GitHub Actions step):

```yaml
- name: Verified smoke test
  run: proc-compose verify --json -- npm run test:smoke > verify.json
- if: always()
  uses: actions/upload-artifact@v4
  with: { name: verify-receipt, path: verify.json }
```

Agent tool action or worktree setup script:

```sh
proc-compose -f "$PWD/proc-compose.yml" --session "task-$TASK_ID" verify --json -- npm test
# on failure: proc-compose -f "$PWD/proc-compose.yml" --session "task-$TASK_ID" why --attempt latest --json
```

