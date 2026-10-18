# Municipal system compatibility gate

**Result: external proc-compose integration exists, but the canonical retained-stack comparison is blocked by the current official browser runner's lifecycle.** Nine focused unit/configuration tests passed. No application stack, browser journey, database reset or three-edit comparison ran. This is a compatibility preflight result, not a reproduced proc-compose defect or a performance finding.

## Source state and scope

Inspected on **2026-10-19**, after the user selected `municipality-central-system` for the [canonical pilot](Agent%20workflow%20opportunities%20and%20validation.md#canonical-pilot).

| Item | Observed state |
|---|---|
| Application checkout | `/home/anivaryam/projects/municipality-central-system` |
| Branch / revision | `master@f3492dca6496579aa0d192cb7a0c8f399de75c14` |
| Working tree | No tracked modifications; pre-existing untracked `.vscode/` and `apps/worker/worker-artifacts/` preserved |
| Tracking state | 13 commits behind the locally recorded `origin/master`; no fetch, pull or checkout performed |
| Node / pnpm | `v22.23.2` / `10.15.0`, matching the declared toolchain |
| Installed Playwright | `1.62.1`, matching `pnpm-lock.yaml:282–296` |
| Installed proc-compose | **v1.9.0**, not the research source checkout's v1.10.0 |

Source locations below are relative to the application checkout at that revision. Repository rules, testing standards, delivery workflow and relevant design/planning context were read. No application feature/fix or three exact edit revisions have yet been selected. Existing worktrees and local data were not changed.

## What the existing harness already supports

1. **Normal Playwright owns six servers.** `apps/e2e/playwright.config.ts:45–106` declares API, API proxy, Core, Portal, BIMS and ingress launchers, all with `reuseExistingServer: false`. Origins/ports can be supplied through the existing inputs (`:6–12`). This is not a drop-in check against an already-retained copy.
2. **Plan 7 already delegates launch responsibility to proc-compose.** `apps/e2e/playwright.plan7.config.ts:45–56` disables Playwright `webServer`, global setup and teardown, while retaining the projects and external endpoint selection. The installed configuration tests confirm this configuration shape. This corrects an uncertainty in the earlier desk study: a new proc-compose/Playwright integration is not needed merely to express external ownership.
3. **The official runner owns the entire invocation, not just the browser.** With `PLAN7_TOPOLOGY=1`, `apps/e2e/scripts/run-e2e.mjs:22–75` validates the rendered environment, ensures the topology is stopped, prepares the upload root, resets/seeds the guarded database, then starts the topology. `:315–374` wraps each Playwright invocation and calls topology stop in `finally`.
4. **This stop/reset/start boundary is deliberate.** `docs/product/testing-standards.md:62–80,309–329` requires fresh guarded lifecycle state and post-termination upload teardown. `proc-compose.plan7-test.yml:3–14,98–100` puts reset/migration before service startup and cleanup in the supervisor teardown. `apps/e2e/scripts/plan7-test-topology.mjs:517–565` implements stopped-state checks and explicit start/stop.
5. **`--keep-data` is not a general retained-mode escape hatch.** `apps/e2e/scripts/run-e2e.mjs:199–227` restricts it to one exact registry-lifecycle debugging invocation. Moreover, the Plan 7 preparation at `:22–75` is not conditional on that flag. It does not establish an approved retained resident-card workflow.

A scoped read-only diff against the local `origin/master` showed no changes to those four harness/topology implementation files. Testing standards changed through additional feature coverage, not a replacement of the inspected reset boundary. This is not a remote freshness check or acceptance of another baseline.

## Focused checks executed

Both commands ran from the application root, without installing dependencies or starting services:

```sh
TMPDIR=/tmp/opencode node --test --test-name-pattern='global setup owns one reset|keep-data is accepted only|Plan 7 setup failures|Plan 7 reset and seed' apps/e2e/scripts/run-e2e.test.mjs

TMPDIR=/tmp/opencode COREPACK_ENABLE_NETWORK=0 corepack pnpm --filter @municipal/db exec tsx --test ../../apps/e2e/playwright.plan7.config.test.ts
```

| Check | Result | What it proves |
|---|---|---|
| Four selected runner tests | **4/4 passed**, exit 0 | Existing source assertions for reset/seed ownership, restricted keep-data, setup-failure cleanup and validated reset/seed environment remain satisfied. These are source-pattern checks, not live subprocess/DB proof. |
| Existing Plan 7 configuration tests | **5/5 passed**, exit 0 | Loading the installed configuration disables competing Playwright launch/setup and preserves projects, use settings and resolver arguments. No browser/server was launched. |

Read-only version checks were `node --version`, `proc-compose --version`, `COREPACK_ENABLE_NETWORK=0 corepack pnpm --version`, and reading the installed Playwright package version. Git status, HEAD, worktrees and the scoped tracking-ref diff were inspected. Context7's current Playwright documentation was supplemental; it was not treated as an exact 1.62.1 snapshot or runtime compatibility evidence.

## Decision and limits

**Stop before the retained-versus-current benchmark.** The [canonical pilot](Agent%20workflow%20opportunities%20and%20validation.md#canonical-pilot) explicitly rejects a warm substitute when the existing gate requires stopping services around reset. The existing official Plan 7 invocation does exactly that. Bypassing the runner, skipping reset, weakening guards, or creating another launcher solely to produce a warm comparison would change what the test proves.

- Preserve the existing test-owned Plan 7 workflow for these browser/lifecycle gates. It already uses proc-compose and needs neither a new supervisor API nor another transport merely to launch its stack.
- No new feature is selected **for this gate on this evidence**. This does not disprove retained development's value elsewhere, establish no user demand, or measure the cost of the present workflow.
- Installed v1.9.0 must not be credited with v1.10.0's default cross-session refusal. No upgrade or configuration change was performed.
- Browser/shell reachability, actual application/build identity, database target/client safety, live process cleanup and per-check service monitoring remain **unverified**. No secret environment values or municipal data were read. Unit-test runtimes are not development-loop or startup measurements.
- All task checks were foreground commands that exited. No task-owned background server or retained session was created. Application Git status was unchanged afterward.

## Next allowed action

Select an exact application feature/fix and approved baseline before any three-edit work. Observe its existing development/check workflow without weakening the official lifecycle gate. If a different existing check legitimately permits retained services, assess that check's compatibility separately; do not silently substitute it for this browser gate. Otherwise record the retained arm as incompatible and retain the existing workflow. Any change to configuration, reset policy, fixtures or shared harness needs its own proposed diff and approval; no such change is proposed here.
