# Agent runtime: decisions and progress

[Strategy](AGENT_RUNTIME_STRATEGY.md) · [Implementation plan](AGENT_RUNTIME_PLAN.md)

## Current state

- Product direction agreed with the maintainer: 2026-10-12.
- Initial inspected source: `main`, `3e7b389a6e6fb848b48b2e8a895f54ab79d9c4af`.
Reconciled 2026-10-13. Acceptance, platform verification, and measured benefit
are separate axes; none implies another.

- **Reviewed implementation acceptance (Linux scope): accepted.** M0 was
  accepted on 2026-10-12. For M1 and the M2–M6 code, the originating agent
  independently closed all five review findings and every YAML follow-up
  (merge precedence, recursive merges, inherited quoted `<<`) on 2026-10-13.
  The progress entries below keep their original pending labels, annotated
  as closed.
- **Platform verification: Linux native only.** Windows/macOS are
  cross-compiled and vetted, not executed. Lint and vulncheck have not run on
  the changed code. The last full Linux race suite predates the finding-4
  corrections, which were verified with targeted packages. The work is
  uncommitted, so no CI run covers it. Windows regressions for the blocked-reap
  handoff, check quiescence, and the single cleanup budget now compile but have
  not run natively. Windows admission-race containment has a pre-existing gap
  and stays unverified. See the plan's
  [Native CI readiness](AGENT_RUNTIME_PLAN.md#native-ci-readiness).
- **Measured product benefit: not established.** Recorded on 2026-10-12: a
  tool-level pilot (18 trials, no agent), an agent run (6 trials, n=1 per
  cell), and an agent rerun after the guide revision (3 candidate trials, n=1).
  The rerun showed one fewer tool call and 11–17% lower wall time per cell.
  With one repetition, no retained transcripts, and zero baseline
  interventions, this does not establish causality or meet the 30% target.
- **M2–M6** are first implementations and candidates, not measured
  improvements. M3 is partial, and M5 is recipes only (MCP deferred).
- **Next action:** the maintainer publishes a branch with the reviewed changes
  and opens a pull request against `main` to trigger CI. In parallel, the
  maintainer reviews and approves (or edits) the
  [Pilot 2 protocol](AGENT_RUNTIME_PLAN.md#pilot-2-protocol) and its approval
  list. No execution until both are done.

Agreement on direction does not freeze example syntax, JSON fields, retention
policy, or exit codes. Resolve the decisions below at their implementation gate.

## Milestone tracker

| Milestone | State | Owner | Completion evidence |
|---|---|---|---|
| M0: Truthful verification vocabulary | Accepted for reviewed scope | Originating agent | `go test -race -count=1 -timeout 180s ./internal/bootstrap ./cmd/proc-compose` passed; native macOS/Windows deferred to CI |
| M1: Owned verification operation and pilot | Implementation accepted (Linux review scope); native Windows/macOS unrun; pilot partial | Implementation pass 2; review fixes 2026-10-13 | Review findings 1–5 closed 2026-10-13; `go test ./... -race -count=1` passed on Linux before the finding-4 corrections; Windows/darwin cross-compiled only; tool-level pilot (n=3) and agent runs (n=1) recorded, Pilot 2 proposed |
| M2: Attempt-aware diagnosis | Implemented (candidate) | Implementation pass 2 | `TestDiagnose_*`, `TestQuery_AttemptFilter` |
| M3: Explainable configuration | Implemented, partial (candidate); YAML follow-ups closed 2026-10-13 | Implementation pass 2 (delegated); review fixes 2026-10-13 | `internal/config/explain_test.go`, `config_cmd_test.go`; doctor evidence items not done |
| M4: Session handoff | Implemented (candidate) | Implementation pass 2 | `TestDiscoverSessions_ReadOnlyHandoffView`, manual `up -s`/`verify` occupancy check |
| M5: Agent integrations | Recipes only; MCP deferred | Implementation pass 2 | README/guide recipes; guide parity test |
| M6: Baseline/candidate comparison | Implemented (candidate) | Implementation pass 2 | `TestCompare_ReportsDifferencesWithoutAttribution`, manual two-worktree run |

Use Planned → In progress → Verified when implementation and required checks
are complete. Record Blocked with its cause or Deferred with its rationale.
Candidate milestones become Planned when their scope is selected from evidence.

## Direction decisions

| ID | Decision | Rationale and alternative considered |
|---|---|---|
| A1 | Build around verified development runs | Connect existing runtime capabilities to a complete user outcome; an isolated collection of additional commands would leave agents assembling the workflow |
| A2 | Reuse session, runner, event, query, and task-execution foundations | A second supervisor or agent log store would duplicate lifecycle and evidence contracts |
| A3 | Execute existing project checks | A new test DSL adds migration and maintenance cost without establishing the core workflow's value |
| A4 | Use one result model for humans and agents | Separate verdict implementations could disagree about success or cleanup |
| A5 | Start M1 with fresh owned sessions | Shared-session reuse adds authority and freshness questions; defer it to an explicit later contract |
| A6 | Treat MCP as a thin integration | The adjacent Process Compose project already provides MCP controls and log search; validate the workflow before expanding transports |
| A7 | Measure before broadening the roadmap | Published agent-tool examples and repo acceptance tests do not measure proc-compose user time savings |
| A8 | Preserve the distinction between evidence and inference | Readiness, test results, source metadata, and temporal log association each establish different facts |

These direction decisions originate in the 2026-10-12 discussion. Revise them
explicitly if implementation evidence or user feedback changes the approach.

## Open design decisions

| ID | Decision needed | Due | Evidence needed |
|---|---|---|---|
| D1 | Review proposed `verify [processes...] -- PROGRAM [ARG...]` syntax and whether `--dir` is needed | Before M1 implementation | Cross-platform direct argv execution, CLI conventions, literal spaces/quotes and shell-metacharacter tests |
| D2 | Review proposed inherited env plus endpoint/session variables; decide names and whether any allowlisted config env may reach the check | Before M1 implementation | Existing endpoint resolver is clear; actual representative app consumers and secret exposure expectations are missing |
| D3 | Review session lease as exclusive acquisition only, exact owned-run cleanup authority/stale metadata behavior, and platform-aware signaling-seal gap | Before M1 implementation | Unix withdrawal serializes signals; Windows `Withdraw` is a no-op and Job Object `Close` kills remaining members; require delayed-signal/blocked-reap coverage and prove no signal after release |
| D4 | Approve/refine 120s startup, 10m check, 30s independent cleanup defaults, bounded override ranges, phase boundaries, escalation, verdict precedence and exit mapping | Before M1 implementation | Defaults are policy proposals, explicitly unmeasured; timeout/interruption/dual-failure and blocked-reap fixtures plus caller expectations required |
| D5 | Approve versioned response/receipt schema, compact-error variant, runtime-dir retention, exact serialized-size policy and attempt-state semantics | Before M1 implementation | Validate stable error codes/counts, readiness observations, unavailable-provenance codes, compact identity limitations, persistence failure and pruning order |
| D6 | Final pilot thresholds, fixtures, agent/model/version, and independent outcome verifier | Before comparative pilot runs | Proposed 2026-10-13 as the plan's [Pilot 2 protocol](AGENT_RUNTIME_PLAN.md#pilot-2-protocol); open until the maintainer approves and freezes it |
| D7 | Attempt diagnosis interface and event-schema evolution | Before M2 | Pilot transcripts, bounded-query and old-journal compatibility |
| D8 | Config schema generation/parity and structured diagnostic format | Before M3 | Parser contract, existing doctor findings, editor/agent consumption |
| D9 | Retention/handoff/reuse authority and discovery scope | Before M4 | Actual human/agent handoffs, session ownership boundaries |
| D10 | MCP operations, transport, cancellation, and integration maintenance | Before M5 | Proven CLI workflow, client requirements, current version-matched protocol docs |
| D11 | Baseline/candidate fixture isolation and comparability requirements | Before M6 | Two real worktrees, controlled test data, reproducibility limits |

D1–D5 and D7–D11 were resolved on 2026-10-12 in implementation pass 2 (see
that progress entry); D6 remains open until pilot inputs exist.

### Resolved decision and M1 proposals

| ID | Decision | Rationale and compatibility impact |
|---|---|---|
| D0 (2026-10-12) | Preserve JSON `started`, `ready`, and `stopped` fields as false for compatibility; add `validation_status` with `passed`/`failed` as the authoritative dry-run outcome. Human output names configuration validation. No release/migration note is needed beyond the CLI/README/agent-guide contract update. | The fields have no external in-repository consumers beyond bootstrap rendering/tests, but may be consumed externally. Keeping their keys avoids breaking JSON decoders; false avoids claiming an application lifecycle occurred. A status string communicates failed validation distinctly from an unattempted/unknown boolean. |
| D1 (2026-10-12, proposed) | `proc-compose [global flags] verify [--dir PATH] [--startup-timeout DURATION] [--check-timeout DURATION] [--cleanup-timeout DURATION] [--json] [processes...] -- PROGRAM [ARG...]`; process selectors use existing process names/dependency selection; command is direct argv with no implicit shell; default check cwd is absolute config directory, relative `--dir` resolves there. Empty check command is usage error. | Matches Cobra positional conventions and existing process filters while preserving exact argv across OSes; avoids inventing shell quoting. Explicit shell behavior remains available by naming a shell in argv. Compatibility: adds a new command only; does not change current `up` shell-string semantics. Rejected alternative: parse one shell command string, which is ambiguous across shells/platforms. |
| D2 (2026-10-12, proposed) | Check inherits caller environment and receives resolved session/endpoint metadata as `PROC_COMPOSE_SESSION`, `PROC_COMPOSE_SESSION_DIR`, `PROC_COMPOSE_RUN_ID`, plus `PROC_COMPOSE_PORT_<PROCESS>_<PORT>` and `PROC_COMPOSE_URL_<PROCESS>_<PORT>` variables. Proposed normalization uppercases ASCII alphanumerics and maps every other name byte to `_`; detect/reject key collisions. Generated values override same-key inherited values. Do not inject process-specific service env or serialize any env values into receipt. | Endpoint map from `session.Plan.Project` remains the sole allocation source and is also recorded in receipt. Explicit allowlisting/config-env forwarding remains an open review item. Rejected alternative: second endpoint config or copying every service's secrets into the check environment. Compatibility: names are new, operation-local environment additions only. |
| D3 (2026-10-12, revised target approved) | For the first feasibility slice, shared process groups expose a synchronized seal. Unix withdraws signaling authority; Windows snapshots Job Object state and closes the handle under its operation lock, preserving pre-close state and errors. Launch/restart admission now shares the runner lifecycle lock with shutdown: `cmd.Start`, `Track`, and registry insertion are one admission transition; IPC restart and reload replacement use that lock. Full ownership/receipt/lease ordering remains a proposed M1 contract, not implemented or accepted. | `session.Acquire` leases are exclusive claims, not liveness proofs. Per-group locking existed on Unix; Windows `Withdraw` was a no-op. Rejected alternatives: timeout-as-termination, CLI-side group signaling, generic stop by mutable session name, or claiming this feasibility seam completes M1. The admission gate prevents launch escape, but `Runner.Run` still joins lifecycle goroutines before sealing, so blocked observer/readers/`cmd.Wait()` can prevent bounded cleanup. |
| D4 (2026-10-12, cleanup policy approved as a design target) | Defaults remain 120s startup/readiness, 10m check, 30s independent cleanup, explicitly unmeasured; cleanup override target remains 30s–2m. Cleanup target: graceful up to 5s, force by T+5, observe to five seconds before deadline, then reserve five seconds for synchronized sealing. Termination/reaping unresolved at the boundary remains unverified. This policy is approved for feasibility work only; deadline enforcement and bounded safe exit are not implemented. | Rejected alternatives: caller timeout as termination, releasing before all signaling paths are sealed, or waiting indefinitely for reaping. Actual lifecycle currently waits for `cmd.Wait()` before the sweep, so a runner lifecycle change is required before this policy can be claimed. |
| D5 (2026-10-12, revised proposal) | Full v1 has explicit readiness observations, provenance limitation codes, and error records exactly `{code,count}`. Full response/receipt uses 64 KiB exact-byte limit. If the full object still cannot fit, emit only the distinct response-only `verify.compact_error` schema: failed/interrupted verdict, phase statuses, stable error codes/counts, fixed identity hashes, `identity.complete:false`, and an explicit omitted-field list; never persist this form as a receipt or claim full identity. | Compact form is an explicit bounded failure contract rather than a partial full schema; hashes preserve correlation but cannot reconstruct readable identities. Rejected alternatives: silently truncating identity, adding an undefined mini-receipt, or allowing compact overflow to report success. Receipt-write failure keeps the old active receipt, skips pruning, releases lease after safe sealing/evidence finalization, and reports `receipt_persisted:false`. |

When resolving a decision, add a dated entry with the chosen behavior, rejected
alternative, rationale, compatibility impact, and evidence. Update the plan's
contract rather than allowing a conflicting specification to accumulate here.
The full D1–D5 contracts remain proposals, not resolved implementation
authority. The originating agent approved only the D3/D4 cleanup mechanics as
the design target for this feasibility slice. That approval is not M1
acceptance and does not resolve D1, D2, or D5. (Historical. Superseded by
implementation pass 2, which resolved D1–D5 as recorded in that entry. The
review closures on 2026-10-13 accepted the resulting implementation for the
reviewed Linux scope.)

## Progress log

### 2026-10-12 — Strategy and planning baseline

- Inspected current capabilities and relevant implementation/test seams at the
  source revision above; consulted the primary sources linked in the strategy.
- Recorded verified-run positioning, staged delivery, acceptance scenarios,
  evaluation design, and outstanding interface decisions.
- Recorded the bootstrap dry-run reporting discrepancy as the first concrete
  implementation target. This is source-inspection evidence, not a new runtime
  test result.
- Reused the existing session/log milestone as historical foundation; the new
  plan owns future work rather than rewriting that completed milestone.
- Planning documents live at the repository root alongside that milestone:
  the existing `.gitignore` excludes `docs/*` except the demo animation.
- No runtime implementation or performance claim is established by this entry.

### 2026-10-12 — Documentation verification

- Source state: the initial revision above plus an uncommitted README update
  and three new planning documents; Linux. No runtime/toolchain checks needed
  for these Markdown-only changes.
- Reviewed proposal labels, milestone dependencies, source references, and
  relative links against the inspected files. Confirmed all three documents
  appear as untracked additions in `git status --short --untracked-files=all`.
- Whitespace checks passed:
  - `git diff --check`
  - `git diff --no-index --check /dev/null AGENT_RUNTIME_STRATEGY.md`
  - `git diff --no-index --check /dev/null AGENT_RUNTIME_PLAN.md`
  - `git diff --no-index --check /dev/null AGENT_RUNTIME_RECORD.md`
- Runtime tests, builds, and the pilot were not run: this change documents the
  plan. No background processes were started. Next implementation action is M0.

### 2026-10-12 — M0 implementation attempt

- Source state: `main` at
  `3e7b389a6e6fb848b48b2e8a895f54ab79d9c4af`, plus the originating agent's
  README/planning changes and this M0 work; Linux/WSL.
- Inspected bootstrap verification/report/render code, all in-repository report
  consumers, CLI wiring, existing bootstrap tests, and the embedded-guide
  parity mechanism. The real verifier invokes `up --dry-run`; it did not start
  applications, but incorrectly reported `started` and `ready` on success.
- Resolved D0 as recorded above. Added an explicit validation outcome while
  retaining legacy lifecycle keys as false. Updated CLI help, README, and the
  embedded guide. Added actual command-verifier success/failure coverage and
  CLI JSON/no-final-config-write coverage.
- Verification is blocked: `go` and `gofmt` are not available on `PATH`, and
  `/usr/local/go/bin` is absent. The attempted targeted test command stopped
  before formatting or executing tests. `git diff --check` passed.
- No runtime tests/build, race tests, help/guide parity checks, or process
  fixtures have run. No background processes were started. Next: run gofmt and
  targeted bootstrap/CLI tests in a Go-enabled environment, then affected
  package race tests and guide parity before marking M0 verified.

### 2026-10-12 — M0 review fixes and local verification

- Rechecked the current checkout and preserved the existing README/planning and
  M0 worktree changes. Source remains `main` at
  `3e7b389a6e6fb848b48b2e8a895f54ab79d9c4af`; no project configuration was
  changed.
- Updated the command-verifier test binary path to append `.exe` on Windows.
  `commandVerifier` now initializes its outcome to `failed`, so temporary-log
  creation failures return a defined D0 status and leave lifecycle flags false.
  Added a deterministic regression by setting all supported temp-directory
  environment variables to a regular file.
- Toolchain: cached Go 1.26.8 at
  `/home/anivaryam/go/pkg/mod/golang.org/toolchain@v0.0.1-go1.26.8.linux-amd64/bin`;
  Linux/WSL. Every Go command used a command-scoped PATH prefix and
  `GOTOOLCHAIN=local GOPROXY=off GOSUMDB=off TMPDIR=/tmp/opencode`.
- Checks and outcomes:
  - `go test ./internal/bootstrap -run '^TestCommandVerifierReportsTemporaryFileFailure$' -count=1` — passed first, before other checks.
  - `gofmt -w internal/bootstrap/verify.go internal/bootstrap/bootstrap_test.go` — completed.
  - `go test -race -count=1 ./internal/bootstrap` — passed.
  - `go test -race -count=1 ./cmd/proc-compose -run '^(TestBootstrap|TestAgentGuide_)'` — passed, including embedded-guide parity.
  - `gofmt -d internal/bootstrap/verify.go internal/bootstrap/bootstrap_test.go` — clean.
  - `GOOS=windows GOARCH=amd64 go test -c -o /tmp/opencode/proc-compose-bootstrap.test.exe ./internal/bootstrap` — cross-compilation passed.
  - `GOOS=windows GOARCH=amd64 go test -c -o /tmp/opencode/proc-compose-cli.test.exe ./cmd/proc-compose` — cross-compilation passed.
  - `git diff --check` — passed.
- No native Windows test execution was performed; cross-compilation does not
  establish Windows runtime behavior. No pilot, broad suite, or M1 work was
  performed. The originating agent later independently accepted M0 as recorded
  in the following entry.

### 2026-10-12 — M0 acceptance and M1 contract/baseline preparation

- Checkout verified before this pass: branch `main`, HEAD
  `3e7b389a6e6fb848b48b2e8a895f54ab79d9c4af`; Linux. Existing M0 edits and
  untracked strategy/plan/record were preserved. No project instructions file
  was found in the checkout or its parent project directories.
- M0 acceptance recorded from the originating agent: reviewed both fixes and
  successfully ran
  `go test -race -count=1 -timeout 180s ./internal/bootstrap ./cmd/proc-compose`;
  checked formatting on all six changed Go files and ran `git diff --check`.
  M0 is accepted for this reviewed scope. Native macOS/Windows execution remains
  deferred to CI; Linux and cached Go 1.26.8 were the reviewed environment.
- Re-inspected current session acquisition, endpoint allocation/resolution,
  runner lifecycle and process-tree cleanup, IPC run/generation fields, bounded
  journal queries/rotation, CLI selection and directory conventions, and
  existing tests. Concrete limitations and future file ownership are recorded
  in the plan. D1–D5 have proposed contracts but are awaiting originating-agent
  review; no command/API/schema was implemented.
- Baseline evidence: **no actual workflow observations or timing measurements
  collected**. The repository root contains proc-compose itself and Go module
  metadata but no representative app manifests/configs or task transcript
  dataset. No synthetic Go test timings are substituted. Existing strategy
  mentions frontend+API, API+database/migration, and API+worker as pilot types;
  these are targets, not currently available fixtures.
- Baseline inputs needed before prototype: identify 2–3 representative projects
  (frontend/API, database/migration, worker); arrange 5–10 real existing-tool
  agent attempts with source revision and installed proc-compose version; for
  each attempt, collect task/scenario, elapsed wall-clock start/end, overlapping
  command intervals/cumulative command time, setup/startup/check/diagnosis/
  cleanup phases, tool calls and token/byte units, independent outcome, false
  success, human interventions, and leaked task-owned processes. Also name the
  agent/model/version, initial context/tool budget, reset procedure, and an
  independent result verifier. Mark unavailable fields missing rather than
  estimating. Keep raw transcripts private and link only approved evidence.
- Proposed small falsifiable pilot remains as the strategy states: healthy
  startup, readiness timeout, missing migration, and check failure despite
  readiness; counterbalanced current/prototype conditions, three repetitions
  per condition/project if feasible. The 72-attempt full matrix is conditional,
  not a baseline observation or a prerequisite to contract review.
- Documentation-only changes: runtime tests/builds not run. No dependencies,
  project configuration, application processes, or background processes were
  started. Next: originating agent reviews D1–D5 plus proposed defaults and
  retention; separately provide baseline project/agent-task inputs before
  prototype implementation. Native platform M0 runtime checks remain for CI.

### 2026-10-12 — M1 cleanup and schema review findings addressed

- Rechecked checkout/instructions: `main` at
  `3e7b389a6e6fb848b48b2e8a895f54ab79d9c4af`; applicable global instructions
  read, no repository `AGENTS.md`. Existing M0/planning changes were preserved.
  Only the plan and record were edited; no runtime/config changes.
- Revised the plan in place with one cleanup policy: startup/check expiry
  immediately starts an independent 30s cleanup window; graceful termination is
  capped at 5s, forced signaling begins by T+5, observation runs to five seconds
  before the configured cleanup deadline (T+25 default), and the last 5s are
  reserved for serialized platform-specific authority sealing. CLI does not wait
  past the cleanup deadline (T+30 default) for `cmd.Wait()`; after all signaling
  capability is sealed it may exit failed/unverified without asserting
  termination/reaping. Finalize evidence and attempt terminal persistence before
  releasing leases; on persistence failure skip pruning, release after safe
  sealing/evidence finalization, and report `receipt_persisted:false`.
- Feasibility inspection: `processgroup.go` requires withdrawal before reaping;
  Unix `Withdraw` serializes signals, but Windows `Withdraw` is currently a
  no-op and Job Object `Close` is kill-on-close. `lifecycle.go` can remain in
  joins or `cmd.Wait()`. The shared runner needs a platform-aware seal (Unix
  withdraw; Windows serialized close); this is a gap, not current behavior. If
  signal/withdraw/close cannot meet the reserved boundary, M1/platform acceptance
  remains blocked. No in-process coordinator is assumed to outlive CLI exit.
- Corrected full JSON examples with readiness observations, unavailable-source
  limitation codes, structured `{code,count}` errors, cleanup-after-finish
  timestamps, and terminal cleanup states. Replaced the underspecified compact
  form with response-only `verify.compact_error`, fixed fields, phase statuses,
  stable error codes/counts, identity hashes, explicit omitted-field list,
  incomplete-identity disclosure, exit precedence, and failed/interrupted-only
  verdicts. It is never persisted as a receipt.
- Narrow regression required before runtime acceptance: delay a signal near the
  withdrawal reserve, block reaping beyond cleanup deadline, prove signals are
  refused after sealing, prove lease release follows sealing/evidence finalization,
  and verify failed/unverified output. Include Windows Job Object close/seal and
  non-empty group behavior. Force terminal receipt-write failure and compact
  overflow separately. Runtime tests were not run.
- Rejected alternatives: beginning graceful/forced escalation at the cleanup
  deadline; treating caller timeout as termination; retaining an in-process
  lease owner beyond CLI exit; CLI-side group signaling; truncating identity in
  the full schema; persisting an undefined mini-receipt.
- Baseline remains **not collected**. Minimum app/task/agent inputs from the
  preceding record remain missing; no synthetic timing or unrelated workflow
  was used.
- Remaining blockers/next action: originating agent reviews revised D3–D5 and
  confirms the 30s–2m cleanup range and 5s/grace + observation + 5s seal
  allocation, Windows close-as-seal policy, and
  compact identity limitations. Before implementation, prove the shared runner
  meets the signal-seal boundary with the delayed-signal/blocked-reap regression.
  Keep M1 acceptance pending; baseline inputs also remain outstanding.
- Documentation checks (Linux):
  - `git diff --check && for file in AGENT_RUNTIME_PLAN.md AGENT_RUNTIME_RECORD.md; do git diff --no-index --check /dev/null "$file"; status=$?; if [ "$status" -gt 1 ]; then exit "$status"; fi; done` — passed with no whitespace diagnostics. The two `--no-index` comparisons return the expected “different from `/dev/null`” status for untracked planning files; the wrapper accepts that status and rejects diagnostics/errors.
  - `python3 -c 'import pathlib,re,sys; fs=[pathlib.Path("AGENT_RUNTIME_PLAN.md"),pathlib.Path("AGENT_RUNTIME_RECORD.md")]; bad=[f"{f}: {u}" for f in fs for u in re.findall(r"(?<!!)\[[^\]]*\]\(([^)]+)\)",f.read_text()) if u and "://" not in u and not u.startswith("mailto:") and not (f.parent/u.split("#",1)[0]).exists()]; print("relative links: " + ("passed" if not bad else "failed")); [print(x) for x in bad]; sys.exit(bool(bad))'` — passed.
  - `python3 -c 'import json,pathlib,re; blocks=re.findall(r"```json\s*(.*?)\s*```",pathlib.Path("AGENT_RUNTIME_PLAN.md").read_text(),re.S); [json.loads(block) for block in blocks]; print(f"parsed {len(blocks)} JSON examples")'` — parsed 4 examples.
  - Runtime tests/builds and application workflows — not run; this is a
    documentation/design-only pass. No dependency installation or application
  runtime processes were started.

### 2026-10-12 — M1 shared-runner seal feasibility slice

- Checkout and instructions: branch `main`, HEAD
  `3e7b389a6e6fb848b48b2e8a895f54ab79d9c4af`; no repository `AGENTS.md` was
  present. Preserved the pre-existing M0 and planning worktree changes. The
  originating agent's approved cleanup contract was treated as a design target,
  not M1 completion.
- Added `ProcessGroup.Seal()`. Unix withdraws under the existing signal mutex,
  observes the group, then releases its identifier. Windows queries membership
  and closes the kill-on-close Job Object under the handle mutex, returning the
  pre-close observation and preserving query/close errors. The runner's terminal
  group sweep now uses this boundary and treats non-empty/unavailable state or
  seal errors as a failed shutdown verdict.
- Added a controlled delayed-signal/reap gate regression at the runner sweep,
  plus a real Unix process-group seal regression and a Windows Job Object
  pre-close observation regression. The shared controlled test released both
  gates; the Unix child was force-killed and `cmd.Wait()` reaped it. The existing
  Unix fixture cleanup also kills/waits the leader and closes the group. The
  Windows fixture's test cleanup closes the Job Object (kill-on-close), cancels
  context, and kills/waits the leader; that test was cross-compiled but not run
  natively here.
- **Feasibility blocker remains:** `Runner.Run` joins lifecycle goroutines before
  its group sweep, and `exec` synchronously waits for `cmd.Wait()` before those
  goroutines return. Therefore a blocked reap can prevent the production runner
  from reaching the new sweep/seal. This patch does not prove deadline-bounded
  sealing, lease release after sealing, or a complete startup-versus-seal gate.
  No bounded-safe-exit or M1 acceptance claim is made. The next feasibility step
  must restructure the runner cleanup/reap boundary narrowly enough to seal
  signaling authority and truthfully return unverified when reaping remains
  blocked; if that cannot meet the five-second seal reserve, record the platform
  as blocked rather than relaxing the contract.
- Go toolchain: cached Go 1.26.8; Linux/WSL. Go commands used the required
  command-scoped PATH with `GOTOOLCHAIN=local GOPROXY=off GOSUMDB=off
  TMPDIR=/tmp/opencode`.
- Checks:
  - `go test ./internal/runner -run '^(TestRunnerSweep_SealWaitsForSignalAndPreservesUnverifiedResult|TestProcessGroup_SealClosesSignalAuthority)$' -count=1` — passed before broader checks.
  - `go test -race -count=1 ./internal/runner` — passed.
  - Re-ran the two focused regressions after adding guaranteed gate release:
    `go test -race -count=1 ./internal/runner -run '^(TestRunnerSweep_SealWaitsForSignalAndPreservesUnverifiedResult|TestProcessGroup_SealClosesSignalAuthority)$'` — passed.
  - `go test -race -count=1 ./internal/session -run '^(TestAcquire_ConcurrentStartsHaveOneOwner|TestAcquire_ResourceContentionAndRelease)$'` — passed; session code itself was unchanged.
  - `GOOS=windows GOARCH=amd64 go test -c -o /tmp/opencode/proc-compose-runner.test.exe ./internal/runner` — cross-compilation passed; native Windows execution remains deferred.
  - `gofmt -d` on all changed runner Go files — clean.
  - `git diff --check` and trailing-whitespace scan of the modified plan/record — passed.
- Independent review of this slice remains pending. M1 acceptance, verify CLI,
  receipts, baseline collection, and native Windows runtime verification remain
  pending; no dependencies/configuration were changed and no background
  processes were started.

### 2026-10-12 — M1 seal identity review finding

- Checkout remained `main` at
  `3e7b389a6e6fb848b48b2e8a895f54ab79d9c4af`, with the pre-existing M0/planning
  edits and earlier M1 seal implementation intact. No repository `AGENTS.md`
  was found; applicable global instructions were followed.
- Changed `ProcessGroup.Seal` to return a `SealObservation` containing state and
  immutable platform identity captured before resource release. The Unix
  implementation captures its PGID before clearing it; Windows captures the Job
  Object label before closing the handle. The runner formats survivor reports
  from this observation rather than querying closed resources. Added a real
  Unix survivor regression that checks both the retained identifier and the
  resulting diagnostic.
- Focused regression run passed first:
  `go test -race -count=1 ./internal/runner -run '^(TestProcessGroup_SealPreservesSurvivorIdentity|TestRunnerSweep_SealWaitsForSignalAndPreservesUnverifiedResult|TestProcessGroup_SealClosesSignalAuthority)$'`.
- Production lifecycle remains concretely blocked. `Runner.Run` blocks in
  `wg.Wait`; `runProcess` reaches synchronous `cmd.Wait` before returning, and
  only then can Run take groups and seal them. Returning Run after sealing would
  leave its lifecycle goroutine able to mutate runner state and signal/report
  afterward; sealing concurrently also races startup admission because the
  current start-to-track interval is not serialized with terminal cleanup. A
  safe change needs a shared admission/seal state transition and a lifecycle
  handoff that makes the post-seal worker inert, then an actual `Runner.Run`
  regression with controlled reap blocking. The disconnected sweep/reap-gate
  test is not production-path evidence. This pass does not claim blocked-reap
  handling, startup/restart sealing, session lease ordering, or bounded cleanup.
- The cleanup statement above was incorrect: killing and waiting the helper
  leader did not establish descendant cleanup after group signaling was sealed.
  The independent reviewer confirmed and stopped that leaked helper. A later
  full-suite process check found another 120-second helper from the observer
  fixture; its shell-based command had the same leader/child ownership problem.
  Both fixtures are corrected in the following entry.
- Verification on Linux with cached Go 1.26.8 (each Go command used the
  command-scoped cached-toolchain PATH and
  `GOTOOLCHAIN=local GOPROXY=off GOSUMDB=off TMPDIR=/tmp/opencode`):
  - New focused regressions first:
    `go test -race -count=1 ./internal/runner -run '^(TestProcessGroup_SealPreservesSurvivorIdentity|TestRunnerSweep_SealWaitsForSignalAndPreservesUnverifiedResult|TestProcessGroup_SealClosesSignalAuthority|TestWindowsJob_UnavailableContainmentIsReported|TestWindowsJob_SealPreservesPreCloseObservation)$'` — passed on Linux (Windows-only selectors are excluded by build tags).
  - `go test -race -count=1 -timeout 240s ./internal/runner ./internal/session` — passed (`runner` 44.061s; `session` 2.041s).
  - `GOOS=windows GOARCH=amd64 go test -c -o /tmp/opencode/proc-compose-runner.test.exe ./internal/runner` — cross-compilation passed; no native Windows execution.
  - `gofmt -d` on all changed runner Go files — clean.
  - `git diff --check` — passed. Planning documents are untracked, so their
    whitespace checks are recorded separately below.
- M1 acceptance remains pending independent review. No verify workflow,
  receipts, configuration, dependencies, or commits were added; baseline
  application measurements remain separately pending.

### 2026-10-12 — M1 process admission boundary and fixture cleanup

- Checkout was `main` at
  `3e7b389a6e6fb848b48b2e8a895f54ab79d9c4af`, with the existing M0 and planning
  edits preserved. No project `AGENTS.md` was present. The earlier independent
  review's confirmed fixture leak was re-read and corrected in this pass.
- Changed the Unix survivor-identity fixture and long-running observer fixture
  to `exec` the helper in place of the shell. Cleanup now targets the observed
  process through its owned `os.Process` handle and calls `Wait`; the identity
  fixture asserts that process is no longer running. Admission regressions also
  use direct-exec helpers and assert process termination on cancellation and
  failed tracking.
- Added a shared launch transition protected by `lifecycleMu`: check admission,
  `cmd.Start`, `ProcessGroup.Track`, and registry insertion occur while holding
  the lock. Shutdown/cancellation closes admission through the same lock before
  taking a seal snapshot. IPC restart requests already use this lock; reload now
  atomically checks admission, publishes replacement config, and queues its
  restart under it. Parent-context cancellation is checked while holding the
  lock, closing the interval before the cancellation watcher acquires it.
  Automatic and initial starts pass through the common launch transition. A
  tracking failure kills and waits the started process via its owned process
  handle before releasing admission.
- Production-path regression `TestRunnerAdmission_CancelBetweenStartAndRegistration`
  gates `Track` after actual `cmd.Start`, cancels `Runner.Run`, proves Run cannot
  finish before registration, observes the group in the production registry
  during teardown, and queues restart/reload attempts while cancellation is
  racing the admission watcher; both are refused without config mutation.
  It releases every gate and checks leader exit. A second production test forces
  `Track` failure and verifies direct kill/reap and no registry residue. Existing
  real IPC restart, reload replacement, and automatic restart tests remain in
  the focused regression set.
- Process cleanup evidence: a full-suite `/proc` scan found PID 41346 running the
  test helper with the 120-second sleep payload. Its identity was checked against
  its command line and it was stopped using a pidfd; it exited. After correcting
  both shell-based long-running fixtures, focused regressions and the full
  race-enabled runner package each had an immediate post-test process scan
  reporting `test helper processes: none`.
- Verification on Linux with cached Go 1.26.8; Go commands used the
  command-scoped cached-toolchain PATH and
  `GOTOOLCHAIN=local GOPROXY=off GOSUMDB=off TMPDIR=/tmp/opencode`:
  - Focused fixture/admission regressions first:
    `go test -race -count=1 ./internal/runner -run '^(TestProcessGroup_SealPreservesSurvivorIdentity|TestObserveLeaderExit_SetupDoesNotBlockOnARunningCommand|TestRunnerAdmission_CancelBetweenStartAndRegistration|TestRunnerAdmission_TrackFailureKillsAndReapsStartedProcess)$'` — passed (3.054s).
  - Focused real IPC restart, reload replacement, automatic restart, and admission checks:
    `go test -race -count=1 ./internal/runner -run '^(TestRestart_RecoversFailedServiceInLiveDaemon|TestReload_RebuildsReadinessForReplacement|TestRun_MaxRestarts_OnFailure|TestRun_MaxRestarts_Always|TestRun_OnFailureRestartCancelledReturnsNil|TestRunnerAdmission_CancelBetweenStartAndRegistration|TestRunnerAdmission_TrackFailureKillsAndReapsStartedProcess)$'` — passed (10.412s).
  - `go test -race -count=1 -timeout 240s ./internal/runner` — passed (43.832s).
  - `GOOS=windows GOARCH=amd64 go test -c -o /tmp/opencode/proc-compose-runner.test.exe ./internal/runner` — cross-compilation passed; native Windows execution remains deferred.
  - `gofmt -d` on changed runner Go files — clean; `git diff --check` — passed.
- Remaining work is the bounded blocked-reap handoff, lifecycle-worker
  quiescence, and session-lease release ordering. This pass establishes admission
  closure, not deadline-bounded cleanup or termination after seal. M1 acceptance
  remains pending independent review; verify workflow, receipts, and baseline
  remain pending. No configuration/dependency changes or commits were made.

### 2026-10-12 — Implementation pass 2: M1 completion and M2–M6

- Checkout: `main` at `3e7b389a6e6fb848b48b2e8a895f54ab79d9c4af` with the earlier
  uncommitted M0/M1 work preserved. The previous agent had already written the
  bounded cleanup handoff (`CleanupHandoffTimeout`, handoff channel, tracked
  reapers, polling reap-free observer) and its tests without recording them;
  this pass verified and adopted that work. The maintainer asked to finish
  M0–M6; that request is treated as approval of the D1–D5 proposals with the
  refinements below.
- **M1 runner:** `runner.RunCheck` runs a direct-argv check (new `procInfo.argv`)
  through the shared `exec` lifecycle: admission under `lifecycleMu`, a tracked
  group, graceful/forced group teardown, a bounded handoff to the stack seal,
  and per-stream capped capture. While a check runs, the stack does not end
  itself (`activeChecks`); a regression found this, because the last service
  exiting during a check had sealed the still-running check group. Unresolved
  processes are now recorded structurally (`Runner.Unresolved`).
- **M1 operation (`internal/verify`):** validation → `session.Acquire` (new
  `LeaseError` separates `session_occupied` from `resource_unavailable`) →
  active receipt → start with IPC/PID/metadata so `status`/`logs` work → readiness
  poll → check with required-service monitor → cleanup (cancel, handoff at
  deadline minus 5s seal reserve, wait no longer than the cleanup deadline) →
  evidence (first/last cursor, error excerpts) → terminal receipt (atomic
  rename) → prune (only after a successful write) → lease release (only when
  signalling authority was withdrawn) → response. Configured `shutdown_timeout`
  is capped at 5s during verify. Source provenance uses `git rev-parse`/`status`
  before and after; a change adds `source_changed_during_attempt`.
- **D1 resolved:** syntax as proposed; `--dir` kept. Task-only selections are a
  usage error (the stack must stay alive to run the check).
- **D2 resolved:** inherited environment plus the proposed generated variables,
  which win; colliding normalized names are a usage error. No config env is
  forwarded to the check. Rejected: forwarding service env (secret exposure).
- **D3/D4 resolved:** as approved; cleanup range 30s–2m. If `Run` has not
  returned by the cleanup deadline, cleanup is `unverified`, authority
  `unknown`, and leases are kept until process exit rather than released.
- **D5 resolved with additions (D5a):** schema as proposed plus `owner`
  (abandonment evidence), `endpoints_not_available`, `evidence.events` (bounded
  error excerpts), and codes `usage_error`, `resource_unavailable`,
  `check_start_failed`, `evidence_unavailable`. A check handed off at the cleanup
  boundary has `check.status: unverified`. Argv is retained verbatim in receipts
  (D5 retention question): receipts are 0600 files in the per-user runtime
  directory; redaction is not implemented. Environment values are never stored.
- **D7 resolved:** optional `attempt_id` on journal events (backward compatible,
  `omitempty`); diagnosis falls back to the attempt's exclusive run ID for older
  journals and discloses it. **D8:** reflection-generated schema plus parity
  test (delegated implementation). **D9:** no new retain flag; `up --silent` is
  intentional retention; `sessions` is read-only and never probes leases.
  `attempts` probes the instance lease only when the recorded owner is
  positively gone; that brief probe can momentarily contend with a concurrent
  acquisition (documented limitation). **D10:** MCP deferred. **D11:** sides run
  sequentially with alternating order; isolation is caller-asserted via
  `PROC_COMPOSE_COMPARE_SIDE`; exit 0 means a valid comparison, with the verdict
  in `outcomes_equal`. D6 remains open (no pilot).
- Checks (Linux/WSL, cached Go 1.26.8, `GOTOOLCHAIN=local GOPROXY=off GOSUMDB=off`):
  - Focused: `go test -race -count=1 ./internal/runner -run TestRunCheck`,
    `./internal/verify`, `./cmd/proc-compose -run 'VerifyCLI|Config|DiscoverSessions'`,
    `./internal/logs -run AttemptFilter` — passed.
  - Milestone boundary (trigger: cross-cutting runner/CLI change):
    `go test ./... -race -count=1 -timeout 600s` — all packages passed
    (runner 47.6s, verify 38.5s, cmd 22.7s).
  - `go vet ./...` for linux, `GOOS=windows`, `GOOS=darwin` — clean.
    `GOOS=windows go test -c` for verify, runner, cmd — compiled; not executed.
  - gofmt clean on every changed/new Go file (pre-existing unformatted
    `metrics.go`, `probe.go`, `systemd/generator.go` untouched).
  - Manual: real `python3 -m http.server` stack with HTTP probe, endpoint env
    check, failing check, parser exception, human output, `up -s` occupancy
    refusal, `sessions`, `attempts`, `why --attempt`, and a two-worktree
    `compare`. A long scratchpad socket path made the first `up -s` attempt fail
    to bind (pre-existing Unix socket length limit); rerun under a short runtime
    directory, removed afterwards.
- Process cleanup: post-suite scan for test helpers, fixture servers, and sleeps
  found none.
- Not done / limitations: pilot and baseline; native Windows/macOS execution;
  independent review; doctor-evidence part of M3; receipt argv redaction;
  comparison of assertions/artifacts beyond check exit/output; MCP.

### 2026-10-12 — Tool-level pilot on municipality-central-system

- Authorization: the maintainer asked to run the work against their project
  `~/projects/municipality-central-system` (HEAD `a6b7356b`, uncommitted docs
  change and `.vscode/` untouched). Config `proc-compose.test.yml`, selection
  `api` (closure `migrate` → `api`), local PostgreSQL test database
  `municipal_test` (migrated by the project's own task; not reset). Node
  22.23.2, Linux/WSL. Pre-run scan: port 4003 free, no project processes, no
  proc-compose sessions.
- Conditions: **baseline** = installed v1.5.2 scripted the way the guide told
  agents (`up --silent --wait-ready --json api`, then the check, then `stop`
  in every case, plus `status` after a failed `up`); **candidate** = this
  checkout's build (`v1.5.2-dirty`) `verify --json api -- CHECK`.
- Scenarios: healthy (`curl -sf /health`), check fails after readiness
  (`curl -sf` of a missing route), readiness timeout (5s startup budget).
  3 repetitions, 18 trials, order alternated. The harness scanned for
  listeners on 4003 and for project processes after every trial and would
  stop at the first leak.
- Results (medians, n=3 per cell):

  | Scenario | Condition | Correct | False success | Cleanup verified | Leaks | Wall s | Commands | stdout B | stderr B |
  |---|---|---|---|---|---|---|---|---|---|
  | healthy | baseline | 3/3 | 0 | 3/3 | 0 | 20.43 | 3 | 420 | 0 |
  | healthy | candidate | 3/3 | 0 | 3/3 | 0 | 21.32 | 1 | 2706 | 98091 |
  | check fails | baseline | 3/3 | 0 | 3/3 | 0 | 21.29 | 3 | 421 | 0 |
  | check fails | candidate | 3/3 | 0 | 3/3 | 0 | 21.38 | 1 | 2755 | 98058 |
  | readiness timeout | baseline | 3/3 | 0 | 3/3 | 0 | 10.33 | 3 | 1043 | 54 |
  | readiness timeout | candidate | 3/3 | 0 | 3/3 | 0 | 10.19 | 1 | 2668 | 868 |

- Observations: both conditions were correct and leak-free when the baseline
  is scripted with an unconditional `stop`. Wall-clock time is unchanged
  (startup dominates). In 3/3 baseline readiness timeouts the daemon was still
  running after `up` failed and needed the separate `stop`; `verify` cleaned up
  itself. `verify` replaces three or four commands with one, but its JSON is
  about 2.7 KB versus 0.4 KB, and it streams about 98 KB of application logs to
  stderr per healthy run. An agent harness that captures stderr would get that
  volume.
- Against the strategy targets: lifecycle correctness held (zero false
  success, zero leaks). Neither benefit target was met: there was no
  wall-clock reduction, and the baseline needed no human interventions
  because it was scripted. **Decision: revise, do not expand.** Next, quiet
  application log streaming in `verify --json` (the journal and receipt
  already keep the evidence), then repeat with a real agent in the loop, where
  forgotten cleanup and output volume would show up.
- Limitations: no agent or model in the loop (tool-level, not the planned agent
  pilot), so tool-call, token, and intervention measurements are missing. One
  project, one selection, a curl smoke check rather than the project's test
  suite, and n=3. The missing-migration scenario was not run because it needs
  database manipulation not covered by the authorization. Raw trial JSON lines
  were kept only in the session scratchpad (`pilot.jsonl`) and will not
  persist. After the run: no project processes and no listener on 4003.

### 2026-10-12 — Revision: quiet JSON output; agent-in-the-loop pilot

- Change: `verify --json` and `compare --json` no longer stream process and
  check output to stderr. The output is still written to the journal and the
  receipt. The new `--stream-logs` flag restores streaming; human mode still
  streams. `verify.Options.ProcessOutput` keeps this separate from verify's own
  diagnostics in `Log`. Regression: `TestVerifyCLI_ExitCodesAndJSONContract`
  now asserts that `--json` stderr carries no application output and that
  `--stream-logs` does. Checks: `go test -race -count=1 ./internal/verify`,
  `./cmd/proc-compose -run 'VerifyCLI|AgentGuide'` — passed; gofmt and vet
  clean. One run against municipality-central-system: stdout 2693 B, stderr
  0 B (previously about 98 KB), exit 0, no leftovers.
- Agent pilot: subagents (Claude Sonnet, general-purpose agent type), each
  told only the binary path, config, session, and task. Each was told to read
  `agent-guide` first and to leave the project unchanged. The same project,
  config, and scenarios as the tool-level pilot; one repetition per cell; run
  sequentially because they share port 4003. After every run I checked for a
  listener on 4003 and for project processes.

  | Scenario | Condition | Reported | Correct | Cleanup | Leaks | Tokens | Tool calls | Wall s |
  |---|---|---|---|---|---|---|---|---|
  | healthy | v1.5.2 | PASS | yes | verified | 0 | 36,696 | 4 | 28.4 |
  | healthy | candidate | PASS | yes | verified | 0 | 40,680 | 5 | 33.0 |
  | check fails | v1.5.2 | FAIL_CHECK | yes | verified | 0 | 36,731 | 4 | 29.2 |
  | check fails | candidate | FAIL_CHECK | yes | verified | 0 | 40,222 | 4 | 29.4 |
  | readiness timeout | v1.5.2 | FAIL_STARTUP | yes | verified | 0 | 37,021 | 4 | 22.1 |
  | readiness timeout | candidate | FAIL_STARTUP | yes | verified | 0 | 38,182 | 4 | 22.1 |

- Observations: 6/6 correct, zero false success, zero leaks, zero human
  interventions. Each v1.5.2 agent cleaned up on its own, including after a
  failed `up`. The candidate used about 4–11% more tokens; the longer agent
  guide is the likely cause, but this is not established. Wall time was
  unchanged. In the readiness-timeout cell, the candidate agent used
  `up --silent --wait-ready` and `stop` rather than `verify`: the guide still
  presents the `up` workflow first, so `verify` is not the obvious default.
- Decision: **revise again, do not expand.** With a capable agent and a simple
  check, the current tool already achieves correct, clean outcomes, and
  `verify` shows no measurable benefit. Candidate revisions: put `verify` first
  in the guide and shorten the guide; then test where the plan expects benefit
  (agents that forget cleanup, interrupted runs, multi-service failures,
  diagnosing a failure). The strategy's 30% targets were not met.
- Limitations: n=1 per cell, one model, one project, curl checks, and
  subagents that share this session's environment. Turn-level transcripts
  were not retained. The token counts are the subagent totals reported by the
  harness.

### 2026-10-12 — Revision: verify-first, shorter agent guide; agent rerun

- Change: `internal/agentguide/GUIDE.md` was rewritten to open with `verify`
  ("prefer it over composing up/check/stop yourself, including when the
  requirement is about startup") and to cut its length from 1724 to 900 words.
  Up/stop, logs, and config guidance were condensed but kept, and the
  failed-`up` cleanup warning is now in bold. Removed: the note on legacy
  text-only sessions and some reload detail (still in `--help` and the README).
  Check: `go test -count=1 ./cmd/proc-compose` passed, including the guide
  parity test.
- Rerun of the candidate cells: same prompts, model, project, and scenarios as
  the previous entry, n=1. Baseline cells are those from the previous entry
  (v1.5.2 and its guide are unchanged).

  | Scenario | v1.5.2 tokens / calls / s | candidate (old guide) | candidate (new guide) | New-guide outcome |
  |---|---|---|---|---|
  | healthy | 36,696 / 4 / 28.4 | 40,680 / 5 / 33.0 | 36,990 / 3 / 23.3 | PASS, correct, clean |
  | check fails | 36,731 / 4 / 29.2 | 40,222 / 4 / 29.4 | 37,054 / 3 / 26.1 | FAIL_CHECK, correct, clean |
  | readiness timeout | 37,021 / 4 / 22.1 | 38,182 / 4 / 22.1 (used `up`) | 36,966 / 3 / 18.3 | FAIL_STARTUP, correct, clean, used `verify` |

- Observations: with the new guide, all three agents chose `verify`, including
  the startup-requirement case that had previously fallen back to `up`. Tokens
  are back to parity with v1.5.2 (within ±1%), there is one fewer tool call
  per task, and agent wall time is 11–17% lower in each cell. No leaks; all
  answers correct.
- Decision: **keep the revision. Benefit is still below the strategy
  threshold.** The gains are small, n=1, and could be noise. Wall-clock
  reduction is under 30%, and there were no interventions to reduce. Next:
  scenarios where the plan predicts real benefit (interrupted runs, a service
  that crashes during the check, multi-service stacks, diagnosis after
  failure, and weaker or cheaper agents), with at least 3 repetitions per cell.

### 2026-10-13 — Independent-review findings: five correctness fixes

- State: `main` at `3e7b389`, uncommitted M0–M6 work plus this pass. Not
  committed. At the time, acceptance was pending originating-agent re-review,
  and passing tests are not acceptance. (Closed on 2026-10-13: see the next two
  entries and the reconciliation entry.)
- Finding 1 (active checks escape shutdown): `Runner.Run` now owns check
  quiescence. Each run publishes a check admission context, a WaitGroup, and
  its cleanup handoff under `lifecycleMu`. `RunCheck` registers under that lock
  (refused once admission closes or the run's workers are done), derives its
  context from the run so stopping the stack cancels it, and is handed off by
  the stack's handoff as well as its own deadline. After workers finish, Run
  closes check admission, cancels checks, and waits for every `RunCheck` to
  publish its final state before sweeping and returning, so no event is
  emitted after Run returns. Regressions: `TestRunCheck_StackShutdownQuiescesActiveCheck`
  (the reviewer's overlay scenario, made permanent and extended with a
  post-Run admission refusal) and `TestRunCheck_StackHandoffBoundsBlockedCheckReap`
  (blocked check reap; the stack's 300ms handoff ends the wait, the check is
  reported unresolved). Both failed against the pre-fix code.
- Finding 2 (two cleanup budgets): `verify` starts one absolute cleanup
  deadline at the first trigger. A check cancellation sets it through the new
  `CheckSpec.CleanupDeadline`, called once when cancellation is observed. The
  check's teardown may run to `deadline − 2×reserve`, the stack's handoff gets
  what remains minus the seal reserve (minimum 1ms, no longer a 1s floor that
  could overrun), and verify waits only until the same deadline. The cleanup
  context stays independent of the caller's context; the 30s–2m range is
  unchanged. Regression: `TestRun_BlockedCheckReapSpendsOneCleanupBudget` holds
  the check's reap through `Runner.SetProcessWaiter` (new test seam) with a
  6s budget via the unexported `cleanupBudget` test seam, and asserts that
  cleanup starts at the check deadline and verify returns by the single
  deadline. With the old per-check split it failed ("cleanup started 5.0s
  after the check deadline").
- Finding 3 (silent invalid `--session`): the root pre-run hook no longer
  returns the session error for commands annotated
  `proc-compose/structured-usage` (`verify`, `compare`). Those commands
  validate the session themselves and render a structured refusal:
  `verify.response` with `usage_error`, or `verify.comparison` with
  `not_comparable`, exit 2, plus `Error: invalid --session ...` on stderr. Other
  commands keep the plain error and exit 1. Flag-parse errors remain plain
  text, as documented. Regression: `TestVerifyCLI_InvalidSessionIsStructured`
  runs the built binary. The reviewer's exact command now exits 2 with one
  JSON object.
- Finding 4 (`<<` rejected by `config check`): structured diagnostics follow
  YAML merge keys the way yaml.v3 decodes them. A mapping, alias, or sequence
  of aliases is merged into the merging path, explicit keys win, and a quoted
  `"<<"` stays an ordinary key. Merged nodes are indexed, so a validation error
  on a merged field is located at its anchor's line. Regressions: two new
  `TestDiagnose_Codes` cases and `TestDiagnose_MergeKeysAndAliasesMatchLoad`
  (five valid forms plus two invalid ones, each checked for agreement with
  `Load`). The reviewer's alias file now gives `validate` exit 0 and
  `config check --json` valid with exit 0.
- Finding 5 (discovery can take a lease): `Classify` consults `LeaseHeld` only
  after positive owner loss. `sessions` passes no probe, so a gone owner is
  `indeterminate` there. `attempts` and `why --attempt` probe only for a gone
  owner. Re-running the full suite showed that "owner alive ⇒ active" was too
  weak, because an owner process can outlive its attempt
  (`TestRun_TerminalReceiptFailureIsReportedAndSkipsPrune`). `active` now also
  needs read-only `OwnerRunning` evidence: the session PID file, checked by
  start time, still names the owner. Regressions: an extended `TestClassify_...`
  table that also asserts the probe is called only for a gone owner, and
  `TestDiscoverSessions_NeverProbesLease`, which counts probes through an
  injectable `instanceHeld`: zero during discovery, one during explicit
  inspection. The old `Classify` called the probe unconditionally, which was
  established by reading the code; the new tests cannot compile against the
  old signatures.
- Docs: `GUIDE.md` (active/indeterminate wording; `sessions` never probes),
  README (one cleanup budget from the first trigger), `attempts --help`.
- Checks, Linux/WSL2 native, Go 1.26.8 cached toolchain,
  `GOTOOLCHAIN=local GOPROXY=off GOSUMDB=off TMPDIR=/tmp/opencode`:
  - The new regressions were run first, then each was confirmed failing on
    temporarily reverted code, and the code was restored.
  - `go test ./... -race -count=1`: all packages ok (runner 47.8s, verify
    41.7s, cmd 22.6s). `cmd/proc-compose` was re-run after the last edit: ok.
  - `go vet ./...` passed for GOOS linux, windows, and darwin.
  - `go test -c` cross-compiled for windows and darwin (runner, verify,
    config, cmd), and the CLI cross-built for darwin/arm64. These were
    **cross-compiled only, not run**.
  - `gofmt -l` is clean on changed and new files. `metrics.go`, `probe.go`,
    and `systemd/generator.go` were already unformatted at HEAD and were left
    alone.
  - `git diff --check` is clean.
- Fixture cleanup: after each run, `pgrep` for test binaries, helper
  sentinels, and `sleep 30` found nothing. One exception: the deliberate
  old-code run of the finding-1 regression leaked its helper (PID 93553,
  `runner.test ... __PROC_COMPOSE_TEST_HELPER__ sleep 60s`), which is exactly
  the reviewed bug. It was identified by PID and command line, stopped with
  SIGTERM, and confirmed gone. The repro workspace under `/tmp/opencode` was
  removed. The reviewer's fixture files in `/tmp/opencode` were left as found.
- Limitations:
  - Native Windows and macOS runs are still deferred to CI.
  - `SetProcessWaiter` and `cleanupBudget` are test seams on internal APIs.
  - Check teardown's `2×reserve` split is unmeasured policy.
  - When a check is cancelled because the stack stopped (for example, a
    fail-fast process), the check is reported failed with a signal, not with
    a distinct cause.
  - An owner whose PID file predates start-time tracking falls back to a
    liveness-only check.
- Next (at the time): originating-agent re-review of these five fixes against
  this state. Done: see the following entry.

### 2026-10-13 — Re-review: findings 1, 2, 3, 5 closed; finding 4 corrections

- State: `main` at `3e7b389`, uncommitted work as above. Not committed.
  At the time, acceptance was pending re-review of finding 4. (Closed on
  2026-10-13: see the reconciliation entry.)
- The originating agent independently closed findings 1 (check quiescence),
  2 (single cleanup deadline), 3 (structured invalid-session refusal), and
  5 (read-only discovery). Their implementations were not touched in this pass.
- Finding 4 had two remaining defects in `internal/config/explain.go`:
  - Merge precedence: `walkNode` inspected every merged mapping, so a merged
    value that an explicit key replaces still produced diagnostics
    (`/tmp/opencode/proc-compose-review-merge-override.yml` passed `validate`
    and failed `config check`). The new `effectiveEntries` returns a mapping's
    entries the way yaml.v3 v3.0.1 decodes them (`mappingStruct`/`mapping`/
    `merge` in `decode.go`): the mapping's own keys first, then the mappings
    named by its last `<<` value, depth-first in sequence order. A merged key
    is skipped once any earlier entry has set it. Only these effective entries
    are walked, so overridden nested values are never inspected. Diagnostics
    keep the merging mapping's path and the winning node's source line.
  - Recursive merges: `<<: *self` made the walk recurse without bound
    (`/tmp/opencode/proc-compose-review-merge-cycle.yml`). `Diagnose` now
    decodes before walking. A non-type decode error, such as yaml.v3's
    `anchor 'self' value contains itself` or its excessive-aliasing refusal,
    is returned as one structured diagnostic and the walk is skipped. Type
    errors are still collected and reported after the walk, so diagnostic
    order is unchanged. As defence in depth, `effectiveEntries` also skips a
    mapping that is already being expanded.
- Regressions, in `internal/config/explain_test.go`:
  - `TestDiagnose_Codes`: an earlier merge-sequence source wins and is located
    at its own line; an effective nested merged unknown field; a quoted `"<<"`
    is an ordinary (unknown) key.
  - `TestDiagnose_MergeKeysAndAliasesMatchLoad` (`Load` versus `Diagnose`
    parity): explicit keys replace merged nested values (the reviewer's
    case); an earlier sequence entry replaces a later one; a nested merge
    chain; an alias reused many times. Its invalid list gained the three new
    invalid fixtures.
  - `TestDiagnose_RecursiveMergesRejectedPromptly`: a direct self merge, a
    self merge inside a sequence, an indirect cycle through an inline mapping,
    and an indirect cycle through a second anchor. Each fixture must be
    rejected by `Load`. `Diagnose` runs in a child test process
    (`TestDiagnoseHelper`) under a 5s context, so a broken implementation
    fails that test rather than crashing the runner. The child must return one
    `invalid_type` diagnostic containing "contains itself".
  - Before the fix, the prior implementation was restored temporarily. The
    precedence cases and all four recursion cases failed, with each recursion
    child crashing or timing out after 1–5s and leaving no child process
    behind. The effective-unknown-field and quoted-key cases already passed;
    they pin behaviour that must not change. The fix was then reapplied.
- Checks, all on Linux/WSL2 with cached Go 1.26.8, run as
  `PATH=<go1.26.8>/bin:$PATH GOTOOLCHAIN=local GOPROXY=off GOSUMDB=off TMPDIR=/tmp/opencode`:
  - `go test ./internal/config -count=1 -run 'TestDiagnose_(Codes|MergeKeysAndAliasesMatchLoad|RecursiveMergesRejectedPromptly)$'`: ok.
  - `go test ./internal/config -race -count=1`: ok.
  - `go test ./cmd/proc-compose -race -count=1 -run '^(TestConfigCommand_|TestVerifyCLI_InvalidConfigJSON$|TestVerifyCLI_InvalidSessionIsStructured$)'`: all 5 passed.
  - `GOOS={linux,windows,darwin} go vet ./internal/config ./cmd/proc-compose`:
    clean. This is static analysis only; nothing ran natively on Windows or
    macOS.
  - `gofmt -l` on both changed files: clean. `git diff --check` and a
    trailing-whitespace grep: clean.
  - Fresh binary against the reviewer files:
    - Override file: `validate` exit 0, and `config check --json` exit 0 with
      `valid: true` (114ms).
    - Cycle file: `validate` exit 1, and `config check --json` exit 1 in 22ms
      with one `invalid_type` diagnostic.
  - No `config.test` helper or built binary remained afterwards. The scratch
    binary was deleted; the reviewer's `/tmp/opencode` files were left as
    found.
- Limitations:
  - Parity is with yaml.v3 v3.0.1 merge semantics, pinned by tests rather
    than shared code: the decoder does not expose which entries it used.
  - A recursion or aliasing refusal is reported as `invalid_type` with
    `path: ""` and no line, because yaml.v3 does not give a location; its hint
    is the generic type hint.
  - Merge keys are matched by their scalar text, as the decoder does for
    string keys. Non-string mapping keys are outside the schema.
- Next (at the time): originating-agent re-review of the finding-4
  corrections. Done: see the following entries.

### 2026-10-13 — Finding 4 follow-up: inherited quoted merge key

- State: `main` at `3e7b389`, uncommitted work as above. Not committed.
  At the time, acceptance was pending re-review. (Closed on 2026-10-13: see the
  reconciliation entry.) Runtime fixes and the two earlier YAML defects were
  not touched.
- Defect: `/tmp/opencode/proc-compose-review-quoted-merge-source.yml` (a merged
  mapping containing a quoted `"<<": ignored`) passed `validate` but `config
  check --json` reported `unknown_field` at `processes.api.<<`.
- Cause, checked against yaml.v3 v3.0.1 `decode.go`: `merge` seeds
  `d.mergedFields` from every key of the parent mapping, including the plain
  `<<` directive (it decodes to the string `"<<"`). `mappingStruct` and
  `mapping` then skip any merged key already in that set. `effectiveEntries`
  seeded `seen` from the emitted entries, which exclude the directive, so it
  walked an inherited quoted `<<` that the decoder drops.
- Fix (`internal/config/explain.go`, `effectiveEntries`): seed `seen` from all
  keys of the merging mapping, directive included. Seeding still happens once,
  at the outermost merge, as in the decoder. Quoted `<<` keys are not ignored
  globally: one declared directly on a struct is still walked and reported.
- Regressions, in `TestDiagnose_MergeKeysAndAliasesMatchLoad`'s valid table:
  the exact reproduction, and a quoted `"<<"` as an ordinary `env` map key.
  Before the fix, the reproduction failed with the reviewer's diagnostic. The
  map case already passed; it pins behaviour that must not change. The
  existing direct quoted-key rejection (`TestDiagnose_Codes` and the parity
  invalid list), override, sequence-precedence, location, alias and recursion
  cases are unchanged and still pass.
- Checks, Linux/WSL2, cached Go 1.26.8, same environment as the previous entry:
  - `go test ./internal/config -run 'TestDiagnose_MergeKeysAndAliasesMatchLoad' -count=1`:
    FAIL before the fix (`inherited_quoted_merge_key_suppressed`), ok after.
  - `go test ./internal/config -count=1 -run 'TestDiagnose_(Codes|MergeKeysAndAliasesMatchLoad|RecursiveMergesRejectedPromptly)$'`: ok.
  - `go test -race ./internal/config -count=1`: ok.
  - `go test ./cmd/proc-compose -race -count=1 -run '^(TestConfigCommand_|TestVerifyCLI_InvalidConfigJSON$|TestVerifyCLI_InvalidSessionIsStructured$)'`: all 5 passed.
  - `GOOS={linux,windows,darwin} go vet ./internal/config ./cmd/proc-compose`:
    clean (static only).
  - `gofmt -l internal/config cmd/proc-compose`, `git diff --check` and a
    trailing-whitespace grep of both changed files: clean.
  - Fresh binary against the reproduction: `validate` exit 0 ("parses
    cleanly"); `config check --json` exit 0 with `valid: true` and no
    diagnostics.
  - The scratch binary was deleted. No new files appeared in `/tmp/opencode`
    and no `*.test` binary remained. The reviewer's file was left as found.
- Limitations: as before, parity is pinned by tests, not shared code. Keys are
  compared by scalar text. The decoder compares decoded values, so non-string
  keys could differ, but they are outside the schema.
- Next (at the time): originating-agent re-review. Done: closed on
  2026-10-13; see the next entry.

### 2026-10-13 — Reconciliation, native CI readiness, Pilot 2 design

- Checkout: `main` at `3e7b389a6e6fb848b48b2e8a895f54ab79d9c4af`, with 25
  modified and 21 untracked files, matching the previous entries. Global
  instructions were read; there is no repository `CLAUDE.md` or `AGENTS.md`.
  This pass is documentation only: only this record, the plan, and the
  strategy were edited. No commit, push, CI trigger, dependency install,
  application start, trial agent, or configuration change.
- Review closure (reported by the maintainer): the originating agent
  independently closed all five findings and every YAML follow-up for the
  reviewed Linux scope. The pending labels in the three preceding entries are
  annotated as closed, with their original wording kept. The current-state
  summaries in this record and the plan now track three axes separately:
  reviewed implementation acceptance (accepted, Linux scope), platform
  verification (Linux native only), and measured benefit (not established).
- Pilot reconciliation: summaries that said the baseline or pilot was
  "not collected" predate the 2026-10-12 trials, so the current summaries now
  cite them: the 18-trial tool-level pilot, the 6-trial n=1 agent run, and the
  3-trial n=1 rerun with the revised guide. The rerun's one fewer tool call and
  11–17% lower wall time come from a single repetition with no retained
  transcripts. They do not establish causality and are below the 30% target.
  The historical baseline gate in the plan is marked as partly superseded.
- Native CI readiness (read-only `gh run list`/`gh run view`, inspection of
  `ci.yml`, test build tags, and skips):
  - The latest green CI (run 38055522163) covers `3e7b389` only.
  - No ref contains the reviewed changes.
  - On Windows, the blocked-reap handoff, check-quiescence, admission-race,
    and single-cleanup-budget regressions are `!windows` and never compile.
  - Lint and vulncheck have never run on the changed code.
  - The last full Linux race suite predates the finding-4 corrections.

  Details, user actions, and an optional workflow diff (not applied) are in the
  plan's [Native CI readiness](AGENT_RUNTIME_PLAN.md#native-ci-readiness).
- Pilot 2 designed and **not run**: 4 scenarios × 2 conditions × 3
  repetitions = 24 trials on municipality-central-system. The design covers
  frozen success criteria, definitions, a per-trial JSONL format, and the
  approval list; see the plan's
  [Pilot 2 protocol](AGENT_RUNTIME_PLAN.md#pilot-2-protocol). The application's
  test config was read (read-only) to design the fixtures. That earlier
  access is not authorization for new runs.
- Missing inputs:
  - A pushed ref and its CI result.
  - Approval of the Pilot 2 protocol and its access list.
  - `ANTHROPIC_API_KEY` for `--bare` trial agents, and a budget.
  - The read-only S1 API route, chosen at calibration.
  - Calibration of the S2 interrupter target and of the S4 fault on this app.
- Documentation checks (Linux):
  - `git diff --check`: clean.
  - `git diff --no-index --check /dev/null <file>` for the three untracked
    planning documents: no diagnostics.
  - A relative-link and heading-anchor check, run as a scratchpad script with
    `python3 -I` over the three documents (GitHub-style slugs): passed.
  - JSON parse of the plan's ```` ```json ```` blocks: 5 parsed.
  - Runtime tests were not run, because the changes are Markdown only.

### 2026-10-13 — Review response: Pilot 2 revision and Windows regressions

- Checkout unchanged: `main` at `3e7b389`, uncommitted work as above. Not
  committed or pushed.
- An independent reviewer accepted the reconciliation and raised five points.
  All five were adopted.
- Pilot 2 protocol revised in the plan:
  - Agent switched to opencode 1.18.35 + `minimax/MiniMax-M3`, at the
    maintainer's request. Later the same day the maintainer chose M3 and
    consented to sending the app's config and logs to MiniMax.
  - Tool exposure restricted through a pilot-only config, and verified at
    calibration. This replaces the reviewer's Claude Code `--tools` fix, which
    no longer applies.
  - Per-trial independent evidence and outcome classes, including
    `not_executed`, `evidence_missing`, and `tampered`. The prompt forbids
    modifying fixtures, and their hashes are checked after each trial.
  - An exact S2 interrupt procedure: target identity, exclusions, recheck,
    delivery and acknowledgement recorded separately from the agent's verdict.
  - The time criterion uses only completed, clean trials and excludes S2.
    "Command time" is renamed "observed tool-call interval time".
  - B1 marked as a proposed addition that needs approval.
  - The proposed CI change is now `-v` on the existing test step.
- Windows regressions:
  - Moved, unchanged, from `!windows` files to the untagged
    `internal/runner/runner_test.go` and `internal/verify/verify_test.go`:
    `TestRunnerCleanupHandoff_SealsBeforeReturningAndQuiescesWorker`,
    `TestRunCheck_StackShutdownQuiescesActiveCheck`,
    `TestRunCheck_StackHandoffBoundsBlockedCheckReap`, and
    `TestRun_BlockedCheckReapSpendsOneCleanupBudget`, with their gate helper
    types.
  - New helper `serviceHelperCmd`: `exec` on Unix, plain `cmd /c` on Windows,
    where the helper stays inside the Job Object.
  - Rejected alternative: Windows-specific copies of these tests. They would
    duplicate the contract and drift.
- Windows finding (pre-existing at `3e7b389`, not fixed):
  - `windowsProcessGroup.Setup` does not start the command suspended. `Track`
    assigns it to the Job Object after `cmd.Start`, so a child that `cmd /c`
    spawns in that window escapes containment.
  - For that reason, `TestRunnerAdmission_*` and
    `TestRunnerCleanupHandoff_FailedTrackDoesNotHoldAdmissionOrRun` stay
    Unix-only, with a comment saying why.
  - Fixing it is a product change that needs its own approval.
- Checks, Linux/WSL2, cached Go 1.26.8,
  `GOTOOLCHAIN=local GOPROXY=off GOSUMDB=off TMPDIR=/tmp/opencode`:
  - `go test -race -count=1 ./internal/runner -run '^(TestRunnerCleanupHandoff_|TestRunCheck_|TestRunnerAdmission_)' -v`:
    10 passed.
  - `go test -race -count=1 ./internal/verify -run '^TestRun_BlockedCheckReapSpendsOneCleanupBudget$'`: passed.
  - `go test -race -count=1 -timeout 300s ./internal/runner ./internal/verify`:
    ok (runner 48.5s, verify 44.6s).
  - `GOOS={linux,windows,darwin} go vet ./internal/runner ./internal/verify`:
    clean.
  - `GOOS={windows,darwin} GOARCH=amd64 go test -c` for runner and verify:
    compiled, **not executed**.
  - `gofmt -l` on the four changed test files: clean.
  - Process scan after the runs: no helper or test binaries remained.
  - Docs: `git diff --check`, `--no-index` whitespace checks, the link/anchor
    check, and the JSON parse (5 examples) all passed.
- Not run: native Windows and macOS (no ref yet), lint, vulncheck, and the full
  `./...` suite. The change touched only test files in two packages, so the
  affected packages were run instead.
- Next:
  1. Done: the maintainer chose M3 and consented to data sharing.
- Pre-publish gate, same day, after the maintainer authorized publishing:
  - `go build ./cmd/proc-compose`: ok.
  - `GOOS={linux,windows,darwin} go vet ./...`: clean.
  - `gofmt -l` on all changed and new Go files: clean.
  - `go test ./... -race -count=1 -timeout 600s`: all packages ok (runner
    48.2s, verify 44.6s, cmd 24.0s).
  - Process scan afterwards: no helpers left.
  - Not available locally: `golangci-lint` and `govulncheck`; left to CI.
  2. The maintainer publishes the branch and pull request; CI runs.
  3. The CI log is checked to confirm that the moved tests ran on Windows.
  4. Decide whether to fix the Windows containment gap before the pilot.

### 2026-10-13 — PR #1 CI failures: lint and Windows concurrent sessions

- Source: branch `agent-runtime-verify`, HEAD
  `60352dcc896156ec000514108bf91501c122a530`, clean before this pass.
- PR #1, CI run 38292246544, tested merge
  `690b2db2cc33ec361e68ea608b9c180b7b9ab59f`:
  - Linux tests and vulncheck passed.
  - `lint` failed (job 114954853530).
  - `test (windows-latest)` failed (job 114954853204). All other Windows
    packages passed, including `internal/runner`, which holds the moved
    handoff and quiescence regressions.
  - macOS was cancelled and needs a fresh native run.
- **Lint** (`golangci-lint` v1.64.8, `ineffassign`): in `verify.Compact`,
  `exit := result.ExitCode` was always overwritten by the following switch.
  It is now `var exit int`; the switch is unchanged, so the status and exit
  precedence are unchanged.
- **Windows `TestRun_DistinctSessionsRunConcurrently`**: the CI log shows only
  the combined message "concurrent sessions shared an endpoint or run
  identity", so it does not say which half failed. Evidence gathered:
  - **Port reuse could not happen while the sessions overlapped.**
    `session.allocate` holds an exclusive lease per allocated port
    (`LockFileEx` with `LOCKFILE_FAIL_IMMEDIATELY` on Windows, `flock` on
    Unix). The lease is per handle, so a second session in the same process
    is refused that port. A shared port would therefore mean the attempts did
    not overlap, and the old test allowed that: nothing forced both to be
    live at once.
  - **The run ID could repeat.** Both generators (`session.Acquire` and the
    `Runner.Run` fallback) formatted `"<pid>-<time.Now().UnixNano()>"`. In Go
    1.26.8 (`src/runtime/time_windows_amd64.s`), `time.now` reads the wall
    clock from `KUSER_SHARED_DATA.SystemTime` (`0x7ffe0014`). The kernel
    updates it on the clock interrupt, not every nanosecond, so two attempts
    acquired in one test process within one tick get the same run ID. Linux
    reads a fine-grained clock, which is consistent with Linux passing.
  - **Attribution:** this is a mechanism established from source, plus a
    deterministic reproduction under a fixed clock. It is not a native
    Windows observation: which half failed in run 38292246544 cannot be
    recovered from its log. The rewritten test reports each half separately,
    so the next native run will show it if either recurs.
- Changes:
  - `ipc.NewRunID` keeps the `<pid>-<nanoseconds>` format and is unique
    within a process: a reading at or before the last issued value is moved
    just past it. Both generators now call it. Nothing parses run IDs.
  - Regression `TestNewRunID_UniqueWhenTheClockDoesNotAdvance`, added to the
    `ipc` package's existing test file: 8×64 concurrent calls with one fixed
    clock value; IDs must be unique, keep the format, and be at or after the
    clock. A backwards clock must not reuse an ID. It **failed** with the old
    formatting temporarily restored
    (`duplicate run ID "143304-1791813600000000000"`) and passes with the fix.
  - `TestRun_DistinctSessionsRunConcurrently`: a bounded barrier in the
    `afterStartup` seam (15s, released in `t.Cleanup`) makes both attempts be
    past readiness, holding leases with stacks running, before either runs
    its check. The test fails if they did not overlap. Ports and run IDs are
    asserted separately, and each failure prints both sessions' run ID,
    endpoint map, and start/ready/check/finish times. The isolation assertions
    are unchanged in strength, and nothing is skipped. A temporarily inverted
    port assertion confirmed that the diagnostics print both sessions in
    full; it was then restored.
  - Not attributed to the known Windows containment gap: this test starts no
    child before `Track`, and the failure was an identity/endpoint
    assertion, not a leak.
- Checks, Linux/WSL2, cached Go 1.26.8,
  `GOTOOLCHAIN=local GOPROXY=off GOSUMDB=off TMPDIR=/tmp/opencode`:
  - Focused, first: `go test -race -count=1 ./internal/ipc -run TestNewRunID -v`
    passed; `go test -race -count=1 ./internal/verify -run '^(TestRun_DistinctSessionsRunConcurrently|TestEncode_CompactErrorNeverReportsSuccess)$' -v`
    passed (the second is the existing compact-result regression).
  - `go test -race -count=1 -timeout 400s ./internal/ipc ./internal/session ./internal/runner ./internal/verify ./cmd/proc-compose`:
    all ok (runner 48.0s, verify 44.6s, cmd 25.0s).
  - `GOOS={linux,windows,darwin} go vet ./...`: clean.
  - `gofmt -l` on the changed Go files: clean.
  - `GOOS={windows,darwin} GOARCH=amd64 go test -c` for ipc, session, runner,
    and verify: compiled, **not executed**.
  - Process scan afterwards: no helpers left.
  - `golangci-lint`/`ineffassign`: **not available locally** (not installed,
    and this pass installs nothing). The lint fix is verified only by
    inspection until CI reruns.
- Remaining blockers: native Windows confirmation of both fixes, a fresh
  macOS run, and lint all need the follow-up commit pushed and CI rerun.
  Nothing was committed, pushed, or merged. No CI or configuration changes.

### 2026-10-13 — Release-preparation fixes (proposed v1.5.3)

- Source: branch `agent-runtime-verify`, HEAD
  `60352dcc896156ec000514108bf91501c122a530`, plus the uncommitted CI-repair
  pass above (run-ID generator, compact-result correction), which is kept.
  PR #1 is a draft; its only CI run is still 38292246544 on the old head.
- **Barrier timeout (review finding).** The `afterStartup` barrier in
  `TestRun_DistinctSessionsRunConcurrently` let an attempt continue when its
  15s wait expired. It is now a `startupBarrier` test helper: on timeout it
  records the timeout and cancels the context shared by both attempts, so
  neither can pass. The test asserts that both attempts reach readiness, that
  neither leaves the barrier or finishes during a 200ms hold, and that each
  leaves at or after the deliberate release. Result collection is bounded
  (2 min, then cancel). Port and run-ID assertions and diagnostics are kept.
  - New `TestStartupBarrier_TimeoutCancelsTheAttempt` drives the timeout path
    through a real attempt that is never released.
  - Mutation check: a barrier that does not hold failed the concurrency test
    ("an attempt left the barrier ... before release"); restored afterwards.
- **Interruption before the check starts (existing-contract defect, found by
  the timeout test).** An interruption after readiness but before the check
  started was reported as `failed`, `check_start_failed`, exit 1, precedence
  `failure_without_interruption`. The plan requires `interrupted`/130 for any
  catchable interruption after acquisition. `recordCheck` now treats a check
  refused with `context.Canceled` under an interrupted parent as the
  interruption. A genuine start failure that coincides with an interruption
  keeps `check_start_failed` and also sets interruption precedence. Before the
  fix the new test failed with exactly that misclassification.
- **Windows containment before execution (release blocker).** `Setup` adds
  `CREATE_SUSPENDED` to the command's existing `SysProcAttr`. `Track` assigns
  the suspended process to its Job Object and then resumes it. Go 1.26.8
  `syscall.StartProcess` closes the primary thread handle
  (`syscall/exec_windows.go`), so the thread is found with a
  `CreateToolhelp32Snapshot(TH32CS_SNAPTHREAD)` walk. `go-winjob`'s own
  `StartInJobObject`/`ResumeProcess` was not reused: it skips the first
  snapshot entry and leaves a suspended process alive if `Assign` fails.
  - Cancellation before `Track`: `Kill` records it; `Track` contains the
    command without resuming it, and teardown terminates the job (or `Seal`'s
    kill-on-close ends it).
  - Assignment failure: the command never ran; the existing lifecycle path
    kills it through the owned process handle and the seal observes `empty`.
  - Resume failure: `Track` terminates the job and waits (bounded by
    `killSettle`, without reaping) for the leader, then reports the error.
  - No new dependency: `golang.org/x/sys/windows` is already a direct
    requirement. Direct argv checks and shell commands both go through the
    same `exec` path, so both are covered.
- Tests:
  - Windows (`processgroup_windows_test.go`): immediate descendant spawned with
    `Track` held for 1s; cancellation before `Track` ended by `Kill` and by
    `Seal`; resume failure; `Track` racing `Kill`/`Close`; a closed handle
    refusing `Track` with the command never run.
  - `TestRunnerAdmission_TrackFailureKillsAndReapsStartedProcess`,
    `TestRunnerAdmission_CancelBetweenStartAndRegistration`, and
    `TestRunnerCleanupHandoff_FailedTrackDoesNotHoldAdmissionOrRun` moved
    unchanged from `runner_unix_test.go` to `runner_test.go`.
  - `pidIsRunning` now also asks Windows for the exit code, because an open
    handle keeps an exited process openable. On Unix the hook returns false.
- Checks, Linux/WSL2, cached Go 1.26.8,
  `GOTOOLCHAIN=local GOPROXY=off GOSUMDB=off TMPDIR=/tmp/opencode`:
  - Focused: the moved admission tests passed (`-race`); the barrier timeout,
    concurrency, interruption-during-check, and compact-result tests passed.
  - `GOOS={linux,windows,darwin} go vet ./...`: clean. `gofmt -l` on changed
    Go files: clean. `go build ./cmd/proc-compose`: ok.
    `GOOS={windows,darwin} GOARCH=amd64 go test -c` for runner, verify, ipc,
    and session: compiled, **not executed**.
  - Full `go test ./... -race -count=1 -timeout 600s`: run 1 **failed** in
    `internal/runner` (51.3s); the failing test name was not captured.
    Runs 2 and 3 passed every package. The failure did not reproduce in an
    isolated runner run or in `-count=4` runner runs alongside a `-count=2`
    verify and cmd load. It is unexplained and stays open; whether it
    predates this pass is unknown.
  - Process scans before and after every run: no test binaries or helpers.
  - Not available locally: `golangci-lint`, `govulncheck`, native Windows and
    macOS.
- Remaining: originating-agent review, commit, push to PR #1, and a green CI
  run on that commit (lint, vulncheck, Linux, macOS, Windows). Nothing was
  committed, pushed, tagged, or released. No workflow or dependency changes.

### 2026-10-13 — Review follow-up, lost exit status, native Windows runs

- Source: same branch and HEAD; still uncommitted. Review findings P2
  (unbounded collector) and P3 (premature native-CI claim) addressed.
- **P2.** `collect()` could wait forever: its one-shot timer fired once, then
  the loop blocked. It is now `collectResults`, which cancels and returns the
  partial results on timeout. `TestCollectResults_TimeoutReturnsPartialResults`
  covers 0, 1, and 2 arrivals. The old logic failed it (0 arrivals hung until
  the 5s guard; 1 arrival returned a fabricated second result). The `<-b.left`
  receives are bounded at 5s.
- **P3.** The v1.5.3 notes now mark native Windows/macOS verification as
  pending CI evidence.
- **Earlier unexplained runner failure: found, product defect, pre-existing
  in v1.5.2.** Reproduced once under load (runner `-count=5` beside a full
  suite) as `TestRestart_RecoversFailedServiceInLiveDaemon`: "worker never
  failed". The worker log said `exited`, not `exited: exit status 1`, so the
  status was lost, not slow. Cause: at the end of `exec`, the reap result was
  read only `if !reap.taken()`, and `taken()` means "published", not
  "received". On the reap-free observer path (Linux, macOS) nothing had
  received it yet, and an already-exited leader is often reaped before that
  check. The exit status was dropped, so a command that exited non-zero was
  reported as a clean exit. Present at `lifecycle.go:603` in v1.5.2.
  - Fix: always `err = reap.recv()` there; `recv` is idempotent by contract.
  - Regression `TestRun_FastFailureKeepsExitStatus` uses a new `reapStarted`
    runner seam, next to `waitProcess`, to hold `exec` until the reap has
    published. Before the fix it failed every run (`state=exited exit=0`);
    without the seam, 3×40 iterations never hit the window unloaded.
  - After the fix, the reproducing load pattern passed (runner ×5 plus a full
    suite).
- **Native Windows** (this host is Windows 10.0.26200; tests were
  cross-compiled with Go 1.26.8 and run through WSL interop, **without
  `-race`**, which needs cgo on Windows):
  - Harness corrections, not product changes: run each binary by absolute
    path (a relative `os.Args[0]` makes every helper re-exec fail with
    `ErrDot`), and give `update` a cwd where `../../README.md` exists.
  - `runner`: 127 pass, 0 fail, 10 skip. This includes every
    containment-before-execution test, the moved admission and failed-`Track`
    regressions, and `TestRun_FastFailureKeepsExitStatus`. The containment and
    admission subset passed ×3.
  - Found natively: `TestRunnerAdmission_CancelBetweenStartAndRegistration`
    gated only `Terminate`, but Windows teardown has no graceful phase and
    sends `Kill`. The fixture now gates the first teardown signal of either
    kind.
  - Found natively: `TestRun_TerminalReceiptFailureIsReportedAndSkipsPrune`
    ran a bare `true`, which this host's PATH lacks. It now runs the test
    binary with no helper mode, a portable `true`. `verify`: 23 pass, 0 fail,
    1 skip (POSIX-shell diagnose test).
  - ansi, config, daemon, doctor, ipc, logrotate, monitor, paths, session,
    systemd, update: all pass.
  - **Unavailable locally:** 9 `cmd/proc-compose` tests and 1 `bootstrap` test
    run `go build`, and there is no Windows Go toolchain here (installing one
    is out of scope). They need CI.
  - **Open, pre-existing (v1.5.2), not fixed:**
    `TestQuery_ConcurrentRotationNeverSilentlySkipsOrReplays` failed 2 of 31
    native runs: `Query` returned `open …events.jsonl.3: Access is denied`
    instead of its retryable "retry this query" error. The mechanism is not
    established. A delete-pending hypothesis was tested and disproved: this
    host removes a pinned file's name at once. This can make the CI Windows
    job fail intermittently.
  - Process scan (`tasklist`) afterwards: no test binaries left.
- Linux, final tree: `GOOS={linux,windows,darwin} go vet ./...` clean; `gofmt`
  clean; `git diff --check` clean; full `go test ./... -race -count=1`: all 16
  packages pass; process scan clean. Retained JSON logs:
  `/tmp/opencode/proc-compose-{runner-stress,fullrace-load}-20261013.jsonl`
  (reproduction), `…-fixed-20261013.jsonl` (after the fix), and
  `/tmp/opencode/proc-compose-fullrace-final-20261013.jsonl`. Native Windows
  logs are under `%TEMP%\pcwin-20261013`.
- Remaining: review; decide on the rotation flake and the version; commit;
  push to PR #1; green CI on that commit, including the first macOS run and
  the `go build`-dependent Windows tests.

### 2026-10-13 — Windows journal rotation race: mechanism and fix

- Source: same branch and HEAD; uncommitted. The second review also reproduced
  the failure (2 of 20 native runs;
  `/tmp/opencode/proc-compose-second-review-winrotation-current-20261013.log`)
  and kept it release-blocking.
- **Review P2 (reap hold).** The `reapStarted` hold in
  `TestRun_FastFailureKeepsExitStatus` continued silently after 10s. It now
  records the timeout and fails from the test goroutine. The run uses the
  cancellable, bounded `runDone` cleanup pattern. Mutation check: a 1ns hold
  failed the test with "publish-before-read ordering was not forced"; restored.
- **Mechanism (measured, native Windows 10.0.26200, Go 1.26.8).** A temporary
  diagnostic test, not committed and retained at
  `/tmp/opencode/proc-compose-rotation-diag_windows_test.go.txt`, opened the
  journal names with `openJournal`'s exact flags. It read the thread's NTSTATUS
  (`RtlGetLastNtStatus`) right after each failed `CreateFile`, while a rotator
  repeated `logrotate.rotate`'s operations and timestamped each one. It ran
  once against the real `logrotate.Writer` too.
  - Every failure, 749 mirror + 18 real + 475 + 621 in later runs, was on
    `.3` with `0xC0000056 STATUS_DELETE_PENDING`, which Win32 reports as
    `ERROR_ACCESS_DENIED`.
  - Each failure fell inside the rotator's `remove .3` window (`os.Remove` is
    `DeleteFile`). Within about 0.5ms the name resolved to
    `0xC0000034 STATUS_OBJECT_NAME_NOT_FOUND`.
  - So the error is an open racing an in-progress delete, not a permission
    failure. `snapshotFiles` returned any open error other than not-exist
    immediately, so the query failed instead of using its bounded,
    identity-checked retry.
  - The earlier delete-pending test was negative because the delete had
    already completed before the open. The state exists only while the
    deleting handle is open.
- **Fix.** `openJournal` (Windows) reads the NTSTATUS only when `CreateFile`
  fails with `ERROR_ACCESS_DENIED`, with the OS thread locked and ntdll
  resolved first. `STATUS_DELETE_PENDING` becomes an error that counts as
  `fs.ErrNotExist`; every other access-denied keeps its error.
  `snapshotFiles` uses `errors.Is(err, fs.ErrNotExist)`. The existing
  `os.SameFile` stability check still decides coherence: a name still being
  deleted fails `os.Stat` and forces a bounded retry, then the documented
  "retry this query" error. A new `.3` from the next rename does the same.
  Unix is unchanged.
- **Regressions** (`internal/logs/open_windows_test.go`; the delete-pending
  state is made deterministic with a legacy `FileDispositionInfo` handle held
  open):
  - `TestOpenJournal_DeletePendingCountsAsNotExist` and
    `TestQuery_DeletePendingBackupIsRetryable` failed before the fix with the
    production error (`events.jsonl.3: Access is denied.`) and pass after it.
  - `TestOpenJournal_GenuineAccessDeniedIsNotMasked` and
    `TestQuery_PermissionFailureIsNotRetryable` (a directory where a backup is
    expected) pass before and after: genuine access denial stays a hard,
    non-retryable error.
- **Native Windows repetitions (no `-race`):**
  `TestQuery_ConcurrentRotationNeverSilentlySkipsOrReplays` passed 200 of 200
  (before: 4 of 51 failed across both reviews, so about 16 failures were
  expected in 200). The whole logs package passed `-count=20`.
- **Linux:** `GOOS={linux,windows,darwin} go vet ./...` clean; `gofmt` and
  `git diff --check` clean. `go test -race -count=1` on logs, logrotate,
  runner, and cmd/proc-compose: all pass
  (`/tmp/opencode/proc-compose-affected-race-20261013.jsonl`). Process scans
  on Linux and Windows: no test processes left.
- Remaining: review, commit, push to PR #1, and green CI: macOS, Windows with
  `-race` and the `go build`-dependent tests, lint, and vulncheck.

### 2026-10-13 — First CI run of the follow-up: lint and macOS fixes

- Follow-up commit `5322d48` was pushed to PR #1 after review. CI run
  38306358474: Linux, Windows, and vulncheck passed. `lint` and
  `test (macos-latest)` failed.
- **Lint** (v1.64.8, `SA4006` at `lifecycle.go:593` and `:605`). Making the
  final `err = reap.recv()` unconditional left the two earlier assignments in
  the durable and ownership-lost branches dead. `recv` returns one published
  result, so they were removed with no behavior change. The Windows durable
  path was rerun natively: runner package 127 pass, 0 fail.
- **macOS: defect from `60352dc`, not in v1.5.2.** Every natural exit went
  unobserved: `Run did not return for a command that exits at once`, checks
  ended in `check_timeout`, and two packages hit the 10-minute test timeout.
  - Cause: `60352dc` changed `reapFreeObserver.Observe` from a blocking
    `waitid` to `WNOHANG` polling that decides on `info.Pid`. `waitSiginfo`
    used Linux's 64-bit `siginfo_t` layout (`si_pid` at offset 16) on both
    platforms. On Darwin `si_pid` is at offset 12 and offset 16 is `si_uid`
    (Go 1.26.8 `runtime/defs_darwin_{amd64,arm64}.go`).
  - `3e7b389` passed macOS because its blocking `Observe` returned on any
    successful `waitid` without reading the payload, as the Darwin comment
    still said.
  - The macOS job on `60352dc` was cancelled, so this was never seen.
- **Fix.** `waitSiginfo` is now defined per platform, in `exitwatch_linux.go`
  (unchanged layout) and `exitwatch_darwin.go` (Darwin layout, 128-byte
  buffer). `exitwait_unix.go` builds only for `linux || darwin`, the only
  platforms that use it. Other Unix targets still use the reaping fallback in
  `exitwatch_other.go`. New `TestWaitSiginfo_PidMatchesKernelLayout`
  (`exitwait_unix_test.go`) pins the offset: 16 on Linux, 12 on Darwin.
- **Checks.**
  - `GOOS={linux,windows,darwin,freebsd} go vet ./...` clean;
    darwin/amd64 and darwin/arm64 runner tests compile.
  - Focused Linux race tests passed; full Linux `go test ./... -race` passed
    16 of 16 packages.
  - Local golangci-lint is v2.14 and cannot read the repo's v1 config. A
    scratch v2 translation, outside the repo, found no `SA4006`. Its other
    findings are v2-only checks and do not match the v1 CI result.
  - **macOS behavior is not verified locally** (no Mac here); it needs the
    next CI run.

## Recording future work

For each implementation session or milestone update, append:

- Date, milestone, actual branch and source revision, and relevant uncommitted
  state if results were obtained before a commit.
- User-visible change and affected files.
- Decisions resolved or superseded, with rationale and alternatives.
- Checks executed, their exact commands, platform/toolchain, and outcomes.
- Runtime fixture ownership and cleanup evidence when processes were started.
- Blockers, limitations, deferred checks, and the concrete next action.
- Links to retained evidence or artifacts and their retention limitations.

Keep trial-level pilot data in a linked artifact when collected. Record project,
scenario, condition, repetition, tool/model versions, independent success,
false-success reports, interventions, wall-clock time, cumulative command time,
tool calls, token/byte counts with units, and leaked processes. Summarize results
here with sample sizes and a continue/revise/stop decision; never mark an
unperformed check or uncollected measurement complete.
