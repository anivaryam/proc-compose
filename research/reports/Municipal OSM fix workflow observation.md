# Municipal OSM fix workflow observation

**A real application fix passed its focused checks and was published for review; this did not establish a proc-compose defect or speedup.** The existing Playwright-owned workflow was retained. Observed friction involved provider authentication, dependency availability, delegated test correctness and fixture environment. No retained-stack arm or new supervisor interface was introduced.

## Source and authority

- Observation date: **2026-10-19**. The user approved municipal issue [#40](https://github.com/Fake-Taxi-Org/municipality-central-system/issues/40), an isolated contribution worktree, one task-local evidence note and subsequently commit/push/PR with baseline lint debt disclosed separately.
- Application baseline: **master@013342744b22ae8e24d48c256750b8600f690ca5**. Published fix: **[7b2f637a8805e2195dd8470b02e0bfb888d7fb3a](https://github.com/Fake-Taxi-Org/municipality-central-system/commit/7b2f637a8805e2195dd8470b02e0bfb888d7fb3a)**; **[PR #42](https://github.com/Fake-Taxi-Org/municipality-central-system/pull/42)**, targeting `master`, open and not merged at publication inspection. Auto-merge was off.
- Worktree/branch: `/tmp/opencode/mcs-osm-tile-referrer`, `contrib/fix-osm-tile-referrer`; published source has **10 files, 168 insertions, 2 deletions**. The primary application checkout remained at `f3492dca` with its existing untracked work preserved; no pull or integration occurred.
- Implementation used user-selected **MiniMax M3**, then direct `minimax/MiniMax-M3` after the user approved a provider change. The coordinator reviewed and verified it. This provider change and the task-specific prompting are confounds, not controlled treatments.
- Toolchain: Node **22.23.2**, pnpm **10.15.0**, Playwright **1.62.1**, Next.js **16.4.0**, Leaflet **1.9.4**, React Leaflet **5.0.0**. Research source remains proc-compose **main@7a9e93d3361278e72d72672f6253773c96e73ad7** (`v1.10.0`); installed proc-compose was **v1.9.0**, not upgraded or used to own this fix's application services.

The primary source of application acceptance details is the [commit-pinned verification note](https://github.com/Fake-Taxi-Org/municipality-central-system/blob/7b2f637a8805e2195dd8470b02e0bfb888d7fb3a/docs/runbooks/evidence/2026-10-19-osm-tile-referrer.md). This report summarizes workflow observations; it does not duplicate or replace the [canonical pilot](Agent%20workflow%20opportunities%20and%20validation.md#canonical-pilot).

## Real work and observed corrections

| Observed boundary | Evidence and disposition |
| --- | --- |
| Model-provider authentication | OpenCode Go returned **401 `api_key_expired`** before any source edit. The user approved the same model through the direct MiniMax provider; no credentials/configuration were changed. This is not a supervisor failure. |
| Dependency preparation | Offline frozen install lacked the cached Railway tarball; ordinary frozen-lockfile installation succeeded. Manifests/lockfile were unchanged. Preparation is separate from browser invocation timings. |
| Delegated test design | Review caught duplicate standalone tests, interception installed after tiles had loaded, and incorrect referrer descriptions. These were corrected before accepting a runtime reproduction: extend two existing recovery journeys, capture before navigation, preserve their original assertions and compare exact origin-only headers. A passing agent exit was not treated as proof of a correct handoff. |
| Fixture setup | The first official invocation reset/migrated/provisioned, then stopped during registry seeding because `UPLOAD_STORAGE_ROOT` was missing. Supplying a dedicated temporary upload/artifact root through process environment fixed setup without editing configuration or weakening the runner. Map-only tests still share this fixture prerequisite. |
| Application reproduction | The two recovery journeys reached the intended assertion: private page headers passed, nonempty intercepted OSM requests existed, but their `Referer` was **undefined**. This is the reported missing-header mechanism, not a live provider `403` test. |
| Application correction | Reused Leaflet's per-image `referrerPolicy` option and one shared literal in five OSM layers. Broader server `mapPaths`/document policy reuse was rejected to avoid an unrelated cross-origin exception. Existing unit/test infrastructure was extended; no new map abstraction, API or dependency was added. |
| Local verification | **8/8 Node tests, 3/3 Chromium journeys, Core/BIMS type checks and both production builds passed**. Focused lint remained failed with **two errors**, independently reproduced from the unchanged baseline polygon source. User authorized publication with these errors disclosed and remediation separate; no checks were waived or weakened. |

These are observed corrections and outcomes, not quantified causal bottlenecks. They do not constitute three independently selected product edits. No extra product change was invented to reach an iteration count.

## Bounded timing sample

The sample is **three official browser invocations for this one task** plus separately recorded preparation/static/build commands. Raw timing files were reread from `/tmp/opencode/mcs-osm-task-Rt47UU/`; no raw browser artifact, credential or DOM content is copied here.

| Invocation | Full external elapsed | Exit/outcome |
| --- | ---: | --- |
| Initial setup attempt | **27.45s** | **1**; registry seed prerequisite failed before browser execution |
| Tests-only RED | **44.60s** | **1**; two intended missing-referrer failures |
| Post-fix GREEN | **47.49s** | **0**; three selected journeys passed |

For GREEN, the task launcher measured **46.49s** around the unchanged official runner; Playwright reported **45.3s**. Shared-package preparation was **5.72s**, separate. Core/BIMS type checks took **9.40s / 6.73s** and overlapped; serial production builds took **20.61s / 14.71s**.

These are unlike attempts: setup success, assertion reach and selected test count changed. The few seconds between the RED and GREEN totals are **not** a treatment effect or edit cost. The duration difference between nested timers is not established avoidable overhead. Concurrent durations must not be added as elapsed savings, and preparation/build figures are not complete task wall-clock time.

The [earlier card baseline](Municipal%20card%20workflow%20baseline.md) measured a different journey, source baseline and Plan 7 lifecycle owner; its **89.58s** cannot be compared causally with **47.49s**. No cold-cache controls, matched warm arm, randomized ordering, phase critical-path instrumentation, complete agent-interaction total or monetary cost were collected. This sample cannot rank general workflow optimizations.

## Existing lifecycle and safety boundaries

The approved fix used the existing standard Playwright configuration and official `run-e2e.mjs`: six test-owned services, fresh guarded reset/migrate/provision/seed, no retained attachment and no `--keep-data`. It did **not** substitute an unguarded warm check for the incompatible Plan 7 comparison described in the [compatibility gate](Municipal%20system%20compatibility%20gate.md).

The previously absent loopback `municipal_osm_referrer_test`, ports **4205–4210** and a dedicated temporary upload/artifact root isolated this work. Before resets, the coordinator checked other municipal test connections and database ownership. After invocations, socket checks and inherited task-tag process scans found no test listeners/survivors; final DB inspection found no other municipal test connections. The existing guarded cleanup removed the task's synthetic upload root. Synthetic DB state remains for a later guarded reset. No application configuration, seed, reset contract or production/development database was changed.

These observations cover normal completion and the exercised setup failure only. They are not universal detached-descendant cleanup proof, wrong-build attestation, interruption coverage or retained per-check cleanup evidence. There was **no eligible proc-compose-owned service-mutation or retained-check exposure** in this fix; zero observed supervisor defects cannot be interpreted as a tested absence of them.

## Publication gate is not remote acceptance

At PR inspection GitHub reported the branch conflict-free, but CI unavailable. Both the [push run](https://github.com/Fake-Taxi-Org/municipality-central-system/actions/runs/39160387095) and [PR run](https://github.com/Fake-Taxi-Org/municipality-central-system/actions/runs/39160419447) annotated every primary job: **not started because account payments failed or a spending limit needs increasing**. Across the two runs, **10 jobs were marked failed and 2 evidence jobs skipped**. No remote application test result can be inferred. The existing two local baseline lint errors are a separate limitation, not the explanation for jobs that never started.

Publishing the CI disclosure exposed separate client/API friction: `gh pr edit` failed on GitHub's deprecated GraphQL `projectCards` field. Updating the same PR body through the existing authenticated REST endpoint (`gh api --method PATCH`) succeeded; no client upgrade or configuration change was needed. This was a metadata-update workaround, not a proc-compose API requirement or measured performance effect.

The user authorized opening the PR, not merging, altering billing/workflows or repairing unrelated effects. No auto-merge or CI rerun was enabled. Research updates in this report/index remain local and uncommitted alongside the preserved earlier documentation work.

## Decision and smallest next validation

**Keep the existing test-owned workflow; select no new proc-compose feature from this observation.** Fixing an application tile policy does not justify MCP, a second launcher or retained managed checks. The Plan 7 compatibility stop remains in force and the canonical three-edit matched pilot remains unexecuted. This is a scoped evidence limit, not a finding that retained development or MCP has no value for other users.

An optional, separately approved **single-task preflight feasibility check** could test a simpler hypothesis before another genuine fix: verify the selected provider can answer and validate the existing seed's guarded storage inputs before delegating edits or launching the official suite. Record credential/storage setup failures, exact unchanged test selection, preparation time and end-to-end elapsed/cleanup. Success is zero such failures reaching the full runner; any late authentication/storage failure or weakened guard falsifies that local preflight hypothesis. One pass would establish feasibility only—not saved time, a priority ranking or demand—and would not authorize configuration changes or a retained-stack comparison.
