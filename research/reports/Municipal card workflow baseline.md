# Municipal card workflow baseline

**One existing Chromium card journey passed through the official guarded Plan 7 workflow.** The complete invocation took **89.58 seconds**; Playwright reported **1 passed (39.8s)**. Shared-package preparation was measured separately at **7.14 seconds**. Cleanup checks found no live session, test listeners, recorded process/group survivors or other municipal test DB connections. This is one baseline observation, **not a three-edit comparison or evidence of a speedup**.

## Scope and source state

Study date: **2026-10-19**. Following the [compatibility preflight](Municipal%20system%20compatibility%20gate.md), the user authorized continuing. The existing resident-card journey was selected as a bounded baseline; no feature implementation or replay edits were invented.

- Application: `/home/anivaryam/projects/municipality-central-system`, **master@f3492dca6496579aa0d192cb7a0c8f399de75c14**. The locally recorded tracking ref was 13 commits ahead; no fetch, pull or checkout occurred.
- Toolchain: Node **22.23.2**, pnpm **10.15.0**, installed Playwright **1.62.1**, Next.js **16.4.0**, proc-compose **v1.9.0**. Do not credit this binary with v1.10.0-only behavior.
- Test: `apps/e2e/tests/resident-portal.spec.ts:453–885`, **“a released staff card shows the approved photo, flips to its QR, and stays private and accessible”**, project **chromium**. Collection-only selection confirmed exactly one test in one file before execution.
- No tracked application source, configuration or tests changed. Pre-existing untracked `.vscode/` and `apps/worker/worker-artifacts/` entries remained. Normal generated build/cache outputs were produced by the existing commands.

## Safety and preparation

The existing `apps/e2e/scripts/plan7-env.mjs` parser validated the rendered environment without printing secrets. It accepted only the loopback **municipal_plan7_test** target, separate Plan 7 API/worker roles, deterministic scanning and the guarded temporary upload/artifact roots. No setup script or configuration rewrite was needed.

Before any reset/start:

- `proc-compose sessions --live --json` returned no live sessions.
- Independent `ss` inspection found ports **4105–4110** free.
- A guarded owner connection queried `pg_stat_activity` read-only: **zero other municipal test DB connections**, also checking the Plan 7 runtime roles across databases. The inspection connection was closed afterward.
- The installed Chromium executable existed and `ldd` reported no missing libraries. Actual browser usability was subsequently exercised by the test.
- The repo's existing **build:workspace-deps** compiled only `@municipal/contracts`, `@municipal/db` and `@municipal/artifacts`, avoiding stale shared outputs before the application run. Its exit was **0**, elapsed **7.14s**. This preparation is not included in the 89.58s workflow total.

Source boundaries: `apps/e2e/scripts/plan7-env.mjs:84–119,122–398`, `packages/db/src/test-database.ts:3–20`, root `package.json:10–11`, and `docs/product/testing-standards.md:62–80,309–329,626–640` in the application checkout.

## Exact baseline invocation

From the application root, the existing runner—not raw Playwright—owned reset, seed, launch and cleanup:

```sh
env -u E2E_STAGE PLAN7_TOPOLOGY=1 COREPACK_ENABLE_NETWORK=0 \
  node apps/e2e/scripts/run-e2e.mjs resident-portal.spec.ts \
  --project=chromium --grep 'a released staff card shows the approved photo'
```

The invocation was wrapped with `/usr/bin/time` and an external `timeout --signal=TERM --kill-after=45s 15m` ceiling; neither the ceiling nor its escalation was reached. Existing readiness/test budgets and assertions were unchanged. Raw stdout/stderr stayed in a private directory under `/tmp/opencode/mcs-baseline-20261019-LtQbjf/`; no DOM, trace, screenshot, credentials or raw journal output was copied into research.

The runner performed the normal guarded reset/migration/provisioning/seed, started the existing seven-service topology, executed the selected test, then stopped it. `PLAN7_TOPOLOGY=1` suppresses the test's separate artifact-worker launch, so the supervisor-owned worker performed rendering (`apps/e2e/tests/registration-helpers.ts:65–73`). There was no duplicate test-owned worker.

## Observed outcome and timing limits

| Observation | Result |
|---|---|
| Official invocation | **Exit 0; 89.58s** elapsed wall-clock, measured externally |
| Playwright result | **1 passed (39.8s)**; no additional project or full file run |
| Browser flow | Browser-created applicant, real upload/scan, staff approval and issuance, worker release, resident login, card photo, QR flip/keyboard, privacy, reduced motion, print and fallback assertions passed |
| Download coverage | **PDF-link visibility/print guidance only**; this test does not download and validate PDF bytes |
| Photo observation | Synthetic upload **312,250 B**, browser photo **21,484 B**; this is an application byte observation, not supervisor performance evidence |

The outer total includes reset/seed and provisioning, API/worker builds, topology startup/readiness, browser execution, wrapper overhead, stop and teardown. Playwright's 39.8s is its own runner-reported duration, not an independently instrumented phase. The difference between those numbers is **not established avoidable startup cost**. No per-phase monotonic spans, matched warm arm, command-count treatment effect or cold-cache controls were collected. Service concurrency must not be summed into elapsed savings. Do not compare this observation causally with the unlike historical runs in the desk study.

## Cleanup evidence

Task-owned daemon **42258**, session **municipal-plan7-test**, run **42258-1792349914309884520**. Session metadata records startup at **2026-10-18T18:58:34.310Z** (October 19 in Asia/Manila).

- The official runner exited 0 after reporting daemon stop. The journal contains **one `shutdown.verified`** and **one `teardown.completed` for `upload-cleanup`** for this exact run.
- Run-filtered state evidence contained **31 events in one bounded page**, with `gap=false`, `truncated=false`, `has_more=false`. The seven services ended `exited`; upload cleanup ended `completed`.
- Independent OS checks found all **nine recorded PID identities and their corresponding group IDs absent**: `42258`, `42268`, `42269`, `42408`, `42417`, `42419`, `42420`, `42572`, `43633`. This inventory covers the daemon and recorded service/teardown leaders, not every possible detached descendant.
- Final live-session listing was empty; ports **4105–4110** were free. The guarded upload root was absent after the existing teardown. The postflight read-only DB check found **zero other municipal test connections**, then closed its own connection.
- The test database retains this run's synthetic state until the next guarded reset, as the repository policy requires. No final reset was added. Development/production data was not reset.
- `git diff --exit-code --` passed, and application Git status matched the pre-run status. No task-owned stack or background service was retained.

Finite `logs --run ... --kind state -n 0 --max-bytes 32768` pages and code-filtered shutdown/teardown queries supplied the supervisor evidence; raw application output was not used as instructions. These are host-run browser results plus supervisor journal/cleanup evidence, **not a `verify` receipt** (`latest_attempt` was null).

## What this does and does not justify

The existing proc-compose integration successfully supported this representative browser path without a new API, MCP adapter or parallel launcher. Ordinary successful cleanup is supported at the observed boundaries. Wrong-identity, interruption, incomplete-reap, unrelated-process and parallel-worktree fault exposures were **not exercised**; group/PID absence cannot certify all possible escaped descendants. Source revision alone is not loaded-code attestation.

The [canonical pilot](Agent%20workflow%20opportunities%20and%20validation.md#canonical-pilot) remains unexecuted: there were no three real edits and no retained comparison. Its compatibility stop condition still applies to the official reset/start/test/stop gate. This passing baseline does not authorize bypassing that contract to manufacture a warm result, establish a general productivity gain or prioritize a new proc-compose feature. Observe a concrete future feature/fix and its eligible failure opportunities before choosing an implementation.
