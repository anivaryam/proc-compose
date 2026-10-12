# Verified development runs: implementation plan

Status (reconciled 2026-10-13). Three facts are tracked separately:

| Axis | Status | Evidence |
|---|---|---|
| Reviewed implementation acceptance (Linux scope) | **Accepted.** M0 accepted 2026-10-12. M1 and the M2–M6 code under review: the originating agent independently closed all five review findings (check quiescence, single cleanup deadline, structured invalid-session refusal, YAML merge diagnostics, read-only discovery) and every YAML follow-up (merge precedence, recursive merges, inherited quoted `<<`). | Record entries dated 2026-10-13 and the reconciliation entry |
| Platform verification | **Linux native only; release-gated on CI.** PR #1 run 38292246544 tested `60352dc`: Linux tests and vulncheck passed, lint and the Windows verify concurrency test failed, macOS was cancelled. The follow-up fixes (lint, run-ID uniqueness, barrier timeout, Windows containment before execution, interruption before check start) are uncommitted and pass the full Linux race suite; Windows/macOS are cross-compiled and vetted only. The admission-race and failed-`Track` regressions now run on every platform, but have never run natively on Windows. | [Native CI readiness](#native-ci-readiness) |
| Measured product benefit | **Not established.** A tool-level pilot (18 trials) and two agent-in-the-loop runs (n=1 per cell) were recorded on 2026-10-12. The latest agent run showed one fewer tool call and 11–17% lower wall time per cell. With n=1, no retained transcripts, and no interventions to reduce, it does not establish causality or meet the 30% target. | Record pilot entries; [Pilot 2 protocol](#pilot-2-protocol) (proposed, not run) |

M2–M6 remain first implementations and candidates, built ahead of pilot
evidence. M3 is partial (doctor evidence not done), and M5 is recipes only:
MCP is deferred (D10).

[Strategy and research](AGENT_RUNTIME_STRATEGY.md) ·
[Decisions and progress](AGENT_RUNTIME_RECORD.md)

## Delivery sequence

| Milestone | Depends on | Exit condition |
|---|---|---|
| M0: Truthful verification vocabulary | Source/consumer inspection | Dry-run output cannot be mistaken for live verification |
| M1: Owned verification operation | M0; resolve D1–D5 in the record | Accurate receipt and verified cleanup for one existing check command |
| M2: Attempt-aware diagnosis | M1 and pilot findings | Explain one attempt with bounded, generation-aware evidence |
| M3: Explainable configuration | M0; prioritize from pilot findings | An agent can inspect and repair YAML using structured evidence |
| M4: Session handoff | M1 and ownership contract | Human and agent can identify and intentionally retain the same environment |
| M5: Agent integrations | Stable core contracts and demonstrated benefit | Thin integrations pass the same lifecycle/result contract |
| M6: Baseline/candidate comparison | Reliable receipts and explicit data isolation | Same scenario runs against two supplied environments with reviewable results |

M2–M6 are follow-on candidates. M1 plus its pilot is the first product milestone;
it does not require implementing the entire roadmap. Collect baseline workflow
observations before introducing M1, so the comparison remains meaningful.

## M0 — Truthful machine contracts

### Work

- [x] Inspect bootstrap report consumers, CLI rendering, and existing tests.
- [x] Define config-validation outcomes separately from process startup and
  readiness; choose a compatibility treatment for existing JSON fields.
- [x] Correct `bootstrap --verify` output without turning it into a live startup.
- [x] Add regression coverage for the real command-verifier path; fake
  verifier results alone do not prove its semantics.
- [x] Update CLI help, README, and embedded guide for the resulting contract.

### Acceptance

- Successful dry-run explicitly reports config validation, never live startup or
  application readiness. Failed validation remains a nonzero result.
- Human and machine output agree about what occurred.
- Verification without writing preserves the existing no-final-config-write
  behavior. This change does not silently start user applications.
- The compatibility decision and any release/migration note are recorded.

Review consistent structured failures for the upcoming operation here. Changes
to every existing command's error envelope can ship incrementally; a global CLI
rewrite is not a prerequisite for M1.

## M1 — Owned verification operation

### Smallest coherent feature

Accept an existing proc-compose config and one existing project check command.
Start a fresh owned environment, wait for configured readiness, run the check,
retain a receipt, and stop the managed process trees. Return human output or
one bounded JSON result from the same result model.

Proposed syntax, pending D1:

```sh
proc-compose --session checkout-fix verify --json -- npm run test:smoke
```

Proposed complete syntax (contract draft):

```text
proc-compose [global flags] verify [--dir PATH] [--startup-timeout DURATION]
  [--check-timeout DURATION] [--cleanup-timeout DURATION] [--json]
  [processes...] -- PROGRAM [ARG...]
```

The JSON, timeout and optional working-directory flags belong to `verify`.
Selectors before `--` use existing process names and dependency closure; the entire argv
after `--` is the check. An empty command is a usage error. The command is
started directly (no implicit shell); callers who need shell syntax explicitly
invoke their platform shell themselves. The check defaults to the absolute
directory containing the selected config; `--dir` overrides it, resolving a
relative value against that config directory. See D1–D5 in the record for the
proposed contract and examples; originating-agent review is still required.

Implemented in `cmd/proc-compose/verify.go`, `internal/verify`, and
`runner.RunCheck`. The implemented schema follows the contract below with the
additions recorded as D5a in the record (`owner`, `endpoints_not_available`,
`evidence.events`, and extra stable error codes).

The first version uses fresh task-owned sessions and refuses an already-active
target identity. Shared-session reuse and intentional retention belong to a
later explicit contract. A refusal must not mutate the existing session.

### Lifecycle and invariants

1. Parse inputs and validate config. Once `verify --json` is unambiguously
   recognized, semantic/configuration failures and occupied-session refusals
   return the structured response below even though no receipt is written.
2. Acquire session identity, declared endpoints, and resource leases using the
   existing session machinery. Refuse an occupied target identity. Bind every
   later action to this operation's acquired run ID and owned runner; never
   resolve a replacement session from mutable metadata during cleanup.
3. Start the selected stack through existing runner behavior. Record process
   generations and readiness observations.
4. Wait for required startup conditions within a bounded deadline. Distinguish
   declared probe success from a service that merely started without a probe.
5. Run the check against the resolved environment. Its process tree is owned and
   participates in timeout, cancellation, output capture, and cleanup.
6. Collect bounded relevant evidence and preserve check results independently
   from startup and cleanup results.
7. Stop only the processes this operation owns. On a startup/check expiry or any
   other cleanup trigger, cancel the work context and enter the independent
   cleanup budget immediately. The CLI owns the runner synchronously; no
   in-process background coordinator is left to outlive the CLI.
8. Within cleanup, make signaling authority withdrawal the hard exit boundary.
   After withdrawal, freeze the evidence high-water mark, persist the terminal
   receipt if possible, prune only after a successful terminal write, release
   leases, and render the response. A failed receipt write is reported in the
   JSON response; it does not strand leases after signaling authority is gone.

#### Attempt states, receipt versus response

The machine-readable JSON **response** is the CLI result; a **receipt** is the
durable per-attempt record. They share the result schema, but a response is
required even when no receipt can safely be created. A refusal before ownership
does not create or overwrite another attempt's receipt. Once a lease is acquired,
write an `active` receipt before launching processes and update it through
cleanup. States are:

| State | Meaning |
|---|---|
| `active` | The owning CLI attempt is in progress. A nonterminal receipt alone is not evidence of abandonment. |
| `completed` | Terminal verdict, evidence finalization, and receipt persistence completed. `status` is `passed`, `failed`, or `interrupted`. |
| `abandoned` | Owner loss is positively evidenced (for example, the recorded owner process identity is absent and its OS lease is no longer held); this is a classification, not a recovery action. |
| `indeterminate` | Available evidence cannot distinguish a still-active attempt from owner loss, or cleanup/termination cannot be verified. Never treat as success. |

Missing heartbeat, stale timestamp, absent journal, or missing metadata alone
does not prove abandonment. M1 does not add a recovery service. Receipts live
under the session runtime directory and can disappear at logout, reboot, or
runtime-directory cleanup; retention is best-effort, not archival durability.
Prune only terminal receipts, after successfully persisting the new terminal
receipt and before releasing ownership. Preserve active/indeterminate receipts.
The final ordering is: stop/escalate while authority is held; withdraw all
signaling authority; freeze/finalize evidence; attempt terminal receipt write;
prune eligible older terminal receipts only if that write succeeded; release
leases; render the truthful JSON response. If receipt persistence fails, do not
prune; release leases after authority withdrawal and evidence finalization, and
return `receipt_persisted:false` with a failed verdict. The old on-disk `active`
receipt may later be classified abandoned only from positive owner-loss and
released-lease evidence; otherwise it remains indeterminate. No runner goroutine
or persistent coordinator is expected to retain leases after CLI exit.

#### Deadline and verdict contract (D4 proposal)

Initial policy defaults are 120s for startup/readiness, 10m for the check, and
30s for independent cleanup. They are reasonable policy defaults, explicitly
**unmeasured**. Each deadline is a monotonic-clock phase budget: startup begins
immediately before stack start and covers start plus readiness; check begins
immediately before the check process starts and ends when its exit and owned
output capture are observed; cleanup begins on the first cleanup trigger and is
independent of the expired/cancelled work context. Parsing/config validation and
receipt writes are not charged to these phase budgets. Proposed overrides are
positive finite durations, bounded to startup 1s–10m, check 1s–1h, cleanup
30s–2m; malformed, zero, negative, non-finite, or out-of-range values are usage
errors before ownership. No unbounded overall timeout is exposed.

At startup/check expiry, cancel the phase immediately and begin a fresh
independent 30-second cleanup budget; do not spend the cleanup budget inside the
startup/check deadline. Cleanup schedule (one monotonic deadline, all groups
handled by shared runner machinery):

1. At cleanup `T+0`, cancel readiness/check work and invoke the platform's
   graceful termination for every owned process group. Allow at most 5 seconds
   for graceful exit (shorter configured shutdown grace may be honored; longer
   grace is capped at 5 seconds). Windows has no separate graceful job signal;
   `Terminate` is immediate job termination there.
2. By `T+5`, send forced termination to every group that is not observed empty.
    Observe containment/liveness through five seconds before the cleanup deadline
    (`T+25` with the 30-second default); stop waiting earlier if all
   groups are empty and their leaders have been reaped.
3. Five seconds before the cleanup deadline (`T+25` by default), stop issuing
   new signals and begin the platform-specific seal for every group (`Withdraw`
   on Unix; synchronized Job Object `Close` on Windows). The final 5 seconds are
   reserved for serialized in-flight signal completion and sealing. The seal
   shares synchronization with `Terminate`/`Kill`; after it returns, every later
   signal attempt is refused or has no live handle.
4. Do not wait beyond the cleanup deadline (`T+30` by default) for `cmd.Wait()`, reader joins, or process exit.
   If group emptiness and leader reaping were not both observed before the
    boundary, set cleanup `unverified`, preserve unresolved identities, and never
    report success. Once every group has safely sealed signaling authority,
   the CLI may finish with that explicit failure even if reaping/termination is
   unresolved. The process may survive; the receipt/response must say so.

The existing `processgroup.go` interface specifies one-way withdrawal and
serialization with signals. Unix `Withdraw` takes the same mutex used by signal,
and lifecycle calls it before `cmd.Wait()`. On Windows, however,
`windowsProcessGroup.Withdraw` is currently a no-op: the Job Object handle keeps
authority until `Close`, whose kill-on-close policy terminates remaining members.
The shared runner therefore needs a platform-aware final seal: Unix withdraws
the group; Windows serializes against handle operations and closes the Job Object
at the boundary. A non-empty Windows group at that point is `unverified` even
though close requests kernel termination; do not claim a post-close observation.

Only shared runner machinery can reach all groups and prevent lifecycle/stack
sweep signal producers from outliving this seal. Launch admission now shares
`lifecycleMu` with shutdown/restart transitions: a launch holds it across
`cmd.Start`, `Track`, and registry insertion, while shutdown closes admission
before taking the group snapshot. IPC restarts and reload-driven replacements
use the same transition, and a failed `Track` is killed and waited through its
owned process handle before admission unlocks. Parent-context cancellation is
also checked while holding the lock, so a restart/reload request cannot slip
through before the cancellation watcher runs. This prevents a started process
from escaping the sealing snapshot. It does not yet solve workers blocked in
observer/readers/`cmd.Wait()`: no bounded handoff or post-seal worker-quiescence
contract is implemented. Implement no CLI-side `ProcessGroup` constructor/
signaling and no detached coordinator. A delayed in-flight signal must finish
before the shared seal returns; if a signal or seal cannot complete inside the
reserved budget, the bounded safe-exit gate is not met, leases must not be
released, and M1 is blocked on that platform. After the seal, unresolved
`cmd.Wait()` or process termination does not prevent CLI exit, but remains
explicitly unverified. A caller timeout or `cmd.Wait()` timeout alone never
proves process termination.

Verdict and numeric exit precedence (highest first): CLI argument parsing that
cannot identify `verify --json` is outside this contract; structured usage or
pre-acquisition failures exit 2; after acquisition, any catchable interruption
sets overall status `interrupted` and exit 130, while preserving every
simultaneous check/runtime/cleanup/persistence failure; absent interruption,
any startup/readiness/check/runtime/cleanup failure, unresolved cleanup, or
receipt persistence failure sets `failed` and exit 1; only all successful
required phases, verified cleanup, and persisted completed receipt yield
`passed` and exit 0. A persistence failure cannot be reported as a persisted
receipt, but JSON stdout still reports the failure. Required selected-service
failure/restart during checking fails even if the check exits zero. Preserve
each phase's result independently; the overall precedence never erases facts.

Hard termination/host loss can prevent final output. An on-disk nonterminal
receipt then stays `active` until positive evidence permits `abandoned`, or is
classified `indeterminate`; no reader may infer `passed` from a nonterminal
record.

### Proposed response and receipt contract (originating-agent review gate)

| Area | Required meaning |
|---|---|
| Envelope | `schema_version`, `kind` (`verify.response` or `verify.receipt`), `state`, `status`, `phase`, `attempt_id`, `receipt_path`, `receipt_persisted` (terminal receipt), `started_at`, and `finished_at`; nullable values are JSON `null`, never omitted. |
| Identity | Session, run ID, absolute config/workdir, config SHA-256 of the exact config-file bytes, selected process names and every started process generation; pre-acquisition values are null/empty with an explicit phase result. |
| Provenance | Tool version and source revision/dirty-state/observation when available; unavailable values are null and accompanied by `limitations` codes. `loaded_code_proven` is always false. |
| Startup | Always-present phase object with status, start/end/deadline, validation, `readiness_observations`, unprobed processes, and structured failures. Each observation has process, generation, nullable probe, status (`passed`, `failed`, `timed_out`, `not_configured`, or `not_run`), and nullable observation time. If readiness never ran, observations are `[]`; if a process had no probe, include `status:"not_configured"` and list it in `unprobed_processes`. |
| Check | Always-present phase object with status, nullable started check-process generation, exact argv when accepted, SHA-256 of compact UTF-8 JSON encoding of argv and its exact byte count, cwd, start/end/deadline, exit/signal/timeout; not-run generation/times/exit are null and argv is empty only when no command was accepted. |
| Endpoints | Always-present object with resolved ports/URLs actually given to check (empty if none), plus explicit `not_available` reason if acquisition did not complete. |
| Evidence | Always-present object with journal path, first/last actual `<run_id>:<sequence>` cursor or null, gap/truncated flags, and excerpt status (`complete`, `truncated`, `incomplete_at_cleanup_deadline`, or `not_collected`). At the final signal seal, freeze the latest published cursor; if readers or processes may still emit after that point, set `truncated:true` and `excerpt_status:"incomplete_at_cleanup_deadline"` rather than waiting past budget or implying complete capture. |
| Cleanup | Always-present phase object with attempted/status/deadline, signaling-authority state, verified termination, unresolved process/generation identities, and failures; pre-acquisition cleanup is `not_needed`. |
| Errors/exit | Always-present `errors` array of `{code,count}` records (`code` is a stable machine code; `count` is a positive integer), numeric `exit_code`, and precedence basis. No free-form string errors are allowed in v1. Response and receipt verdicts agree when the terminal receipt was persisted. |
| Retention | Receipt location/state and explicit runtime-directory durability limitation. |

The fields above are the proposed full v1 schema and are now the review contract.
Every listed top-level field and phase object is present in every response and
receipt. Nullable fields are explicit `null`; arrays/maps are empty when there
are no entries; v1 omits no schema fields. Overall statuses are `passed`,
`failed`, or `interrupted`; phase statuses are `not_run`, `passed`, `failed`,
`timed_out`, `interrupted`, `unverified`, or `not_needed`. `phase` names the
current/final phase (`validation`, `acquiring`, `starting`, `waiting_ready`,
`checking`, `cleaning_up`, or `completed`). Source `limitations` is a required
array of stable codes; use `revision_unavailable`, `dirty_state_unavailable`,
and/or `observation_unavailable` when corresponding values are null. Startup
`readiness_observations` is always present; a not-run phase has an empty array,
not a fabricated failed probe. Each error record requires exactly `code` and
positive `count`; repeated occurrences of one code are represented by count,
and v1 has no error-detail string field. Initial stable v1 codes are
`invalid_configuration`, `session_occupied`, `startup_failed`,
`startup_timeout`, `check_exit_nonzero`, `check_timeout`,
`required_process_failed`, `interrupted`, `cleanup_unverified`,
`receipt_persistence_failed`, and `result_size_limit`. Output stdout contains exactly
one JSON object and no application logs/human notices; diagnostics go to stderr.
For `kind:"verify.receipt"`, `state` is the attempt lifecycle (`active`,
`completed`, `abandoned`, or `indeterminate`) and `completed` requires terminal
receipt persistence. For `kind:"verify.response"`, `state:"completed"` means
the response itself is finalized; it does not imply a receipt exists or that a
failed terminal write changed an on-disk `active` receipt. `receipt_persisted`
always means that this attempt's terminal receipt was successfully persisted.
If Cobra/global argument parsing fails before the program can identify both the
`verify` command and `--json`, ordinary parser text is an explicit exception.
Once that invocation is identified, all semantic usage/config/occupancy
failures use the JSON response schema, even when no receipt is safe to persist.

Do not store full environment values by default. The exact accepted argv is part
of full-result identity and can itself contain sensitive text; D5 review must
approve its retention/redaction policy before implementation. Existing
application logs are retained as emitted. Resource cleanup means
managed runtime cleanup, not rollback of arbitrary database or external effects.

Source metadata does not establish loaded-code freshness. At minimum, report
its observation time and limitations. Decide how concurrent source changes and
application build/version signals affect the result before claiming that a
particular source snapshot was tested.

#### Proposed wire examples (review proposal, not implemented)

Success:

```json
{
  "schema_version": 1,
  "kind": "verify.receipt",
  "state": "completed",
  "attempt_id": "01J9RUNTIME8N4M2K6Q1V7A3C5D0",
  "status": "passed",
  "phase": "completed",
  "session": "checkout-fix",
  "run_id": "4182-1791813600000000000",
  "config_path": "/work/app/proc-compose.yml",
  "config_sha256": "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
  "workdir": "/work/app",
  "source": {"revision": "a1b2c3d", "dirty": true, "observed_at": "2026-10-12T12:00:00Z", "loaded_code_proven": false, "limitations": []},
  "tool_version": "dev",
  "selected_processes": [{"name":"api","generation":1},{"name":"web","generation":1}],
  "endpoints": {"api.http": {"port": 43127, "url": "http://127.0.0.1:43127"}},
  "startup": {"status":"passed","started_at":"2026-10-12T12:00:00Z","finished_at":"2026-10-12T12:00:05Z","deadline_at":"2026-10-12T12:02:00Z","validation":"passed","readiness_observations":[{"process":"api","generation":1,"probe":"http","status":"passed","observed_at":"2026-10-12T12:00:04Z"},{"process":"web","generation":1,"probe":null,"status":"not_configured","observed_at":null}],"unprobed_processes":["web"],"errors":[]},
  "check": {"status":"passed","generation":1,"argv":["npm","run","test:smoke"],"argv_sha256":"sha256:77b12cdda054b51951953dbeae73284c02ee13b8f1ce5692919f52fa08745c92","argv_bytes":26,"cwd":"/work/app","started_at":"2026-10-12T12:00:05Z","finished_at":"2026-10-12T12:00:23Z","deadline_at":"2026-10-12T12:10:05Z","exit_code":0,"signal":null,"timed_out":false,"stdout":"2 passed\\n","stderr":"","output_truncated":false},
  "cleanup": {"status":"passed","attempted":true,"started_at":"2026-10-12T12:00:23Z","finished_at":"2026-10-12T12:00:24Z","deadline_at":"2026-10-12T12:00:53Z","signaling_authority":"withdrawn","termination_verified":true,"unresolved_processes":[],"errors":[]},
  "evidence": {"journal":"/run/user/1000/proc-compose/pc-...events.jsonl","first_cursor":"4182-1791813600000000000:18","last_cursor":"4182-1791813600000000000:42","gap":false,"truncated":false,"excerpt_status":"complete"},
  "errors":[],"exit_code":0,"precedence":"all_required_phases_passed",
  "receipt_path":"/run/user/1000/proc-compose/pc-.../attempts/01J9RUNTIME8N4M2K6Q1V7A3C5D0.json","receipt_persisted":true,"retention":"runtime-directory; may disappear at logout/reboot",
  "started_at": "2026-10-12T12:00:00Z",
  "finished_at": "2026-10-12T12:00:24Z"
}
```

Failure with cleanup failure (both facts remain visible):

```json
{
  "schema_version": 1,
  "kind":"verify.receipt",
  "state":"completed",
  "attempt_id": "01J9RUNTIME8N4M2K6Q1V7A3C5D1",
  "status": "failed",
  "phase":"completed",
  "session": "checkout-fix",
  "run_id": "4190-1791813700000000000",
  "config_path":"/work/app/proc-compose.yml","config_sha256":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","workdir":"/work/app",
  "selected_processes":[{"name":"api","generation":2}],"endpoints":{"api.http":{"port":43129,"url":"http://127.0.0.1:43129"}},
  "source":{"revision":"a1b2c3d","dirty":true,"observed_at":"2026-10-12T12:01:00Z","loaded_code_proven":false,"limitations":[]},"tool_version":"dev",
  "startup":{"status":"passed","started_at":"2026-10-12T12:01:00Z","finished_at":"2026-10-12T12:01:05Z","deadline_at":"2026-10-12T12:03:00Z","validation":"passed","readiness_observations":[{"process":"api","generation":2,"probe":"http","status":"passed","observed_at":"2026-10-12T12:01:04Z"}],"unprobed_processes":[],"errors":[]},
  "check":{"status":"failed","generation":1,"argv":["go","test","./..."],"argv_sha256":"sha256:641077b9ce8287a24483a198d4fa91614c2f5dd22b5982c6207b02c2ba848cf9","argv_bytes":21,"cwd":"/work/app","started_at":"2026-10-12T12:01:05Z","finished_at":"2026-10-12T12:01:20Z","deadline_at":"2026-10-12T12:11:05Z","exit_code":1,"signal":null,"timed_out":false,"stdout":"--- FAIL: TestX\\n","stderr":"","output_truncated":true},
  "cleanup":{"status":"unverified","attempted":true,"started_at":"2026-10-12T12:01:20Z","finished_at":"2026-10-12T12:01:50Z","deadline_at":"2026-10-12T12:01:50Z","signaling_authority":"withdrawn","termination_verified":false,"unresolved_processes":[{"name":"api","generation":2,"identity":"unavailable"}],"errors":[{"code":"cleanup_unverified","count":1}]},
  "evidence":{"journal":"/run/user/1000/proc-compose/pc-...events.jsonl","first_cursor":"4190-1791813700000000000:4","last_cursor":"4190-1791813700000000000:19","gap":false,"truncated":true,"excerpt_status":"incomplete_at_cleanup_deadline"},
  "errors":[{"code":"check_exit_nonzero","count":1},{"code":"required_process_failed","count":1},{"code":"cleanup_unverified","count":1}],"exit_code":1,"precedence":"failure_without_interruption",
  "receipt_path":"/run/user/1000/proc-compose/pc-.../attempts/01J9RUNTIME8N4M2K6Q1V7A3C5D1.json","receipt_persisted":true,"retention":"runtime-directory; may disappear at logout/reboot",
  "started_at":"2026-10-12T12:01:00Z","finished_at":"2026-10-12T12:01:50Z"
}
```

Pre-acquisition semantic failure response (config invalid; no persisted
receipt):

```json
{
  "schema_version":1,"kind":"verify.response","state":"completed","attempt_id":null,
  "status":"failed","phase":"validation","session":"checkout-fix","run_id":null,
  "config_path":"/work/app/proc-compose.yml","config_sha256":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","workdir":"/work/app",
  "source":{"revision":null,"dirty":null,"observed_at":null,"loaded_code_proven":false,"limitations":["revision_unavailable","dirty_state_unavailable","observation_unavailable"]},"tool_version":"dev",
  "selected_processes":[],"endpoints":{},
  "startup":{"status":"failed","started_at":null,"finished_at":null,"deadline_at":null,"validation":"failed","readiness_observations":[],"unprobed_processes":[],"errors":[{"code":"invalid_configuration","count":1}]},
  "check":{"status":"not_run","generation":null,"argv":["npm","run","test:smoke"],"argv_sha256":"sha256:77b12cdda054b51951953dbeae73284c02ee13b8f1ce5692919f52fa08745c92","argv_bytes":26,"cwd":"/work/app","started_at":null,"finished_at":null,"deadline_at":null,"exit_code":null,"signal":null,"timed_out":false,"stdout":"","stderr":"","output_truncated":false},
  "cleanup":{"status":"not_needed","attempted":false,"started_at":null,"finished_at":null,"deadline_at":null,"signaling_authority":"none","termination_verified":null,"unresolved_processes":[],"errors":[]},
  "evidence":{"journal":null,"first_cursor":null,"last_cursor":null,"gap":false,"truncated":false,"excerpt_status":"not_collected"},
  "errors":[{"code":"invalid_configuration","count":1}],"exit_code":2,"precedence":"pre_acquisition_failure",
  "receipt_path":null,"receipt_persisted":false,"retention":"no receipt created",
  "started_at":"2026-10-12T12:02:00Z","finished_at":"2026-10-12T12:02:00Z"
}
```

An occupied-session response uses the same fields with `phase:"acquiring"`,
`startup.status:"not_run"`, `startup.validation:"passed"`,
`errors:[{"code":"session_occupied","count":1}]`, null attempt/run IDs, cleanup
`not_needed`, exit 2, and no receipt. Both forms keep check status `not_run`
with null exit/times while retaining the accepted exact argv and its digest; no
receipt is claimed.

### Baseline gate: minimum evidence to start

Historical gate (written before any trials). Partly superseded on 2026-10-12:
the tool-level pilot and two n=1 agent runs on municipality-central-system
are recorded in the record. Those runs did not retain transcripts, cumulative
command time, phase timing, or interventions under an agent harness, so the
fields below are still the requirement for [Pilot 2](#pilot-2-protocol).
Original text: no real application baseline is available; this is missing
evidence, not completed evaluation. To begin with one representative
application, obtain:

1. A named repository/worktree and owner authorization, a runnable existing
   proc-compose config, source revision, proc-compose version, fixture/data
   reset instructions, and the project's existing check command.
2. One defined task/scenario and 5–10 real attempts using current proc-compose,
   with attempt start/end wall time; overlapping command intervals and
   cumulative command time; setup/startup/check/diagnosis/cleanup phases; agent
   errors, human interventions, independently verified outcome, false success,
   and leaked task-owned process observations.
3. Agent/model/version, initial context/tool budget, tool calls and token/byte
   counts with units, plus the independent outcome verifier and its procedure.

Missing fields stay marked missing. Do not use Go test timings as substitutes,
scan unrelated repositories, or run application workflows without authorization.
Record a small falsifiable pilot only after this baseline input exists.

#### Serialized-size enforcement

The 64 KiB ceiling applies to the exact UTF-8 bytes of the complete serialized
JSON object, after JSON escaping, for both response and receipt. Reserve space
for the mandatory envelope/phase/status/identity/verdict fields first; enforce
input caps and measure the complete candidate with the actual JSON encoder
before ownership, then remeasure after every receipt update. Proposed caps:
argv encoded bytes 16 KiB, selected process names plus generations 8 KiB,
resolved endpoint map 8 KiB, config/source/tool metadata 4 KiB, error codes and
counts 4 KiB,
and stdout/stderr 4 KiB each; these are caps on encoded contribution, not raw
characters. Remaining room is allocated to evidence excerpts only. This leaves
budget for required fields and JSON escaping; the final encoded byte count is
still authoritative. Do not persist environment values.

If optional excerpts and provenance have been removed and the complete full v1
object still cannot fit, do not truncate required identity or emit a partial
full-schema object. Emit the following separate, response-only compact form. It
is distinguished by `kind:"verify.compact_error"` and has exactly the listed
fields; it is not a receipt and does not satisfy the full receipt schema.

```json
{
  "schema_version":1,
  "kind":"verify.compact_error",
  "status":"failed",
  "phase":"validation",
  "exit_code":2,
  "attempt_state":null,
  "phase_statuses":{"validation":"failed","startup":"not_run","check":"not_run","cleanup":"not_needed"},
  "errors":[{"code":"result_size_limit","count":1}],
  "identity":{
    "config_sha256":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
    "session_sha256":"sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
    "run_id_sha256":null,
    "attempt_id_sha256":null,
    "argv_sha256":"sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
    "selection_endpoint_generation_sha256":"sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd",
    "complete":false,
    "omitted_fields":["config_path","workdir","session","run_id","attempt_id","argv","selected_processes","process_generations","endpoints","source","tool_version","startup","check","cleanup","evidence","receipt_path","retention","started_at","finished_at"]
  },
  "terminal_receipt_persisted":false
}
```

Compact-form field rules: every field shown is required; `phase_statuses` always
contains validation/startup/check/cleanup; `errors` is the bounded stable
`{code,count}` array and includes `result_size_limit` plus any known verdict
facts. `status` is `failed` or `interrupted`, never passed; a catchable
interruption after acquisition uses status `interrupted`, error code
`interrupted`, and exit 130. Otherwise exit is 2 before lease acquisition and 1
after. `null` digests mean that source identity
did not exist/reach the caller; `omitted_fields` is always an array (possibly
empty), and no other field may be omitted. Digests are
lowercase SHA-256: config digest is over exact config-file bytes; session/run/
attempt digests are over the exact UTF-8 identifiers; argv digest is over compact
UTF-8 JSON encoding of exact argv; `selection_endpoint_generation_sha256`
covers canonical JSON for the complete ordered selection, actual allocated
endpoints, and every observed process generation; canonical JSON is compact
UTF-8 with map keys lexicographically sorted and process/generation arrays in
stable selection order. The fixed hash strings in
these examples illustrate field shape, not measured or repository-derived data.
`identity.complete` is always false because compact form cannot
reconstruct readable identity or prove that every optional source was observed.
It explicitly discloses identity loss and can never represent success.
`phase_statuses` values use the full phase-status vocabulary. `attempt_state` is
null before acquisition and `indeterminate` after acquisition.
`terminal_receipt_persisted` is false; do not write a compact
form as a receipt. If an initial active receipt exists, leave it intact; after
CLI exit it is classified only by the separate owner-loss evidence rules. The
compact object is encoded and measured like every other response and must itself
fit within 64 KiB. If it cannot be emitted, return failure on stderr and do not
claim JSON or receipt persistence.

When an acquired full result cannot be serialized, safely withdraw runner
signaling authority, freeze the bounded evidence high-water mark, preserve the
prior `active` receipt and skip pruning, release leases, then render the compact
response. If ordinary receipt
writing fails for an I/O reason, use the full response schema with
`receipt_persisted:false` and error code `receipt_persistence_failed`; release
leases after authority withdrawal and evidence finalization, and do not prune
receipts. Journal excerpts use `logs.Query` with run and generation filters;
cursor IDs are the actual `<run_id>:<sequence>` form.

### Implementation slices

- [x] Draft D1–D5 and document the CLI/result contract with examples; await
  originating-agent review before implementation.
- [x] Add a shared `ProcessGroup.Seal` boundary: Unix withdraws under its
  signaling lock before its final observation; Windows observes Job Object
  membership and closes the handle under the handle lock, preserving the
  pre-close state and close/query errors. The shutdown sweep uses this seal for
  groups still registered after per-process cleanup.
- [x] Preserve immutable identity in the `Seal` observation before platform
  resources are released; survivor reporting uses that observation rather than
  reading closed handles.
- [x] Close process launch/restart admission with the shared lifecycle mutex.
  Serialize `cmd.Start` through `Track` and registry insertion against terminal
  cleanup; reject later launches/restarts; clean up a started command directly
  if containment tracking fails. Cover cancellation between start and
  registration through `Runner.Run`.
- [x] Bounded blocked-reap handoff: `Runner.CleanupHandoffTimeout` seals every
  registered group, then releases lifecycle workers through a handoff channel;
  unresolved reaps are reported as unverified and workers are quiescent before
  `Run` returns. `verify` releases leases only after `Run` returns (authority
  withdrawn) and the terminal receipt write was attempted.
- [x] Implement one end-to-end happy path using a real temporary service and
  check process; wire lifecycle, output, and regression coverage together.
- [x] Handle failed startup/checks, deadlines, interruption, and descendant
  cleanup through the existing lifecycle machinery.
- [x] Persist receipts and expose bounded evidence for success and failure.
- [x] Add ownership/concurrency and incomplete-result regression cases.
- [x] Document the command and update the embedded guide using its existing
  parity check. Record actual verification results and remaining limitations.
- [ ] Run the pilot defined in the strategy; record measurements before
  prioritizing follow-on work. **Partial:** a tool-level pilot (18 trials) and
  two n=1 agent runs were recorded on 2026-10-12, on one project. The
  three-repetition agent pilot with retained transcripts and an independent
  verifier has not run. Its protocol is [Pilot 2](#pilot-2-protocol), which
  awaits approval.

### Acceptance and regression matrix

| Scenario | Required result |
|---|---|
| Healthy stack and successful check | Check executed against resolved endpoints; receipt records success; cleanup verified |
| Invalid config or unavailable required resource | Check not run; structured cause; no unrelated session modified |
| Startup/readiness failure | Check not run; relevant startup evidence; owned processes cleaned up |
| Missing readiness probe | Receipt distinguishes process start from probe-backed readiness |
| Nonzero check or timeout | Accurate check outcome and bounded evidence; check descendants and stack cleaned up |
| Required service fails during check | Recorded runtime failure cannot be hidden by check exit zero |
| Catchable interruption during startup/check | Interrupted outcome where output remains possible; bounded cleanup |
| Check failure plus cleanup failure | Both outcomes preserved; after authority withdrawal terminal status is failed, with `cleanup.status:"unverified"` when termination/reaping was not established |
| Existing active target session | Refusal without restart, stop, or other mutation |
| Two concurrent attempts | Independent declared endpoints/evidence; resource contention explained; stopping one preserves the other |
| Session identity changes before cleanup | Stale operation cannot stop a replacement run |
| Large/rotated output | Bounded result, explicit gaps/truncation, surviving event references handled honestly |
| Receipt storage/output failure | Failure reported without pretending persistence succeeded; cleanup still attempted; lease release follows authority/evidence persistence ordering |
| JSON invalid config or occupied session | Complete structured response with not-run phase objects, exit 2, no unsafe receipt write |
| Parser failure before `verify --json` is identifiable | Documented parser-text exception; no promise of JSON |
| Result budget exhausted by escaped argv/endpoints/process metadata | Pre-acquisition structured refusal or the explicitly discriminated response-only `verify.compact_error`; identity loss is disclosed and verdict cannot be success |
| Cleanup wait outlives caller budget | Runner seals all signaling authority within the cleanup budget, then finalizes evidence/result and releases the lease; unresolved termination/reaping is unverified and cannot succeed. Verified by `TestRunnerCleanupHandoff_*` (controlled delayed signal plus blocked reap through `Runner.Run`) on Linux; Windows cross-compiled only. |
| Cancellation races process start/registration | Admission and registry insertion are serialized with shutdown; every successful start is visible to cleanup or a later start is refused. Verified for the start-to-Track boundary through `Runner.Run`; blocked-reap handoff is covered by the row above. |
| Final receipt write fails after cleanup | Emit full response with `receipt_persisted:false` and `receipt_persistence_failed`; do not prune; release lease only after authority withdrawal and evidence finalization |
| Nonterminal receipt with held lease/owner | Classified active; no abandonment from timestamp or missing metadata alone |
| Lost owner with positive process/lease evidence | May classify abandoned; no automatic cleanup/recovery is inferred |
| Runtime directory removed on logout/reboot | Receipt is unavailable; no durable-retention claim |
| Source metadata unavailable or source changes | Provenance limitation recorded; no unsupported freshness claim |

M1 stops when this contract and the affected verification gates pass and the
pilot results are recorded. A new checks DSL, general database provisioning,
MCP server, hosted service, and automatic source editing are outside M1.

## Reuse and implementation seams

These paths identify existing responsibilities, not a mandate to modify every
file. Recheck their current behavior before coding.

| Concern | Existing seam and regression coverage |
|---|---|
| Dry-run semantics | [bootstrap verifier](internal/bootstrap/verify.go), [report](internal/bootstrap/report.go), [bootstrap tests](internal/bootstrap/bootstrap_test.go), [CLI tests](cmd/proc-compose/bootstrap_cli_test.go) |
| CLI wiring and readiness/shutdown waits | [main](cmd/proc-compose/main.go), [helpers](cmd/proc-compose/helpers.go), [helper tests](cmd/proc-compose/helpers_test.go), [handshake tests](cmd/proc-compose/handshake_test.go) |
| Endpoint resolution and leases | [session](internal/session/session.go), [session tests](internal/session/session_test.go) |
| Service/task lifecycle and process trees | [lifecycle](internal/runner/lifecycle.go), [runner tests](internal/runner/runner_test.go), [Unix tests](internal/runner/runner_unix_test.go), [Windows process-group tests](internal/runner/processgroup_windows_test.go) |
| Shared event and project metadata | [IPC](internal/ipc/ipc.go) |
| Evidence selection and bounded output | [query](internal/logs/query.go), [query tests](internal/logs/query_test.go), [why](cmd/proc-compose/why.go) |
| Config diagnostics and discovery | [config](internal/config/config.go), [doctor checks](internal/doctor/check.go), [doctor report](internal/doctor/report.go), [doctor tests](internal/doctor/doctor_test.go) |
| Agent instructions | [embedded guide](internal/agentguide/GUIDE.md), parity coverage in [helper tests](cmd/proc-compose/helpers_test.go) |

Implementation responsibility proposal (historical; written before M1 and
since implemented along these lines, see the record):

| Slice | File-level responsibility |
|---|---|
| CLI validation and shared rendering | `cmd/proc-compose/verify.go` (new): argv/selector/flag validation, invocation of operation, human/JSON rendering and exit mapping; wire command in `main.go` only after contract approval |
| Attempt orchestration and receipt | `internal/verify/` (new module if the boundary is approved): phase state, deadlines, in-progress/final receipt persistence, bounded output and verdict aggregation |
| Session ownership/endpoints | Extend `internal/session` only where required: acquire unique run/session and expose the resolved values; existing `Acquire` leases `instance:<hash>`, named resources, and TCP endpoints but does not by itself prove daemon liveness or safe replacement cleanup |
| Managed stack lifecycle | Use `internal/runner` lifecycle/start/stop and status observations; add a narrow exported orchestration seam only if current foreground wiring cannot be safely invoked; no second supervisor |
| Check command containment | `runner.ProcessGroup` is exported, but its constructor and supporting execution/lifecycle machinery are private. Extend shared runner orchestration to own the one-shot direct-argv command and return bounded cleanup state; preserve signaling/withdrawal/reaping discipline there. Do not construct or signal groups independently from the CLI. |
| Evidence | Query `internal/logs.Query` using the acquired journal/run/generation; preserve its explicit gap/truncation semantics; no parallel event store |
| Tests | Extend existing `cmd/proc-compose/helpers_test.go`, `internal/session/session_test.go`, `internal/runner/runner_test.go` and `internal/logs/query_test.go`; add module-local tests only if a new receipt/orchestrator module is approved |

Existing session `Acquire` uses OS-held advisory lease files and returns a unique
run ID, but its lock is an ownership primitive, not a liveness or stale-metadata
proof. Existing process groups verify managed trees, yet detached descendants
can remain explicitly unverified. Runner configured commands use shell parsing;
that is not a direct-argv API for the user check. Current IPC/log query support
run ID and process generation, but no attempt ID; adding attempt-scoped events is
M2, so M1 must scope receipt references without changing the journal schema.

### Exact regression scenarios for implementation

1. Happy path passes argv containing spaces/quotes literally, receives the
   leased endpoint matching the services' resolved config, and cleans both stack
   and check descendants.
2. Selecting a service includes its dependencies; selecting a task follows
   existing selection semantics; unknown/empty selection is rejected before
   ownership. Empty command and literal metacharacters confirm no implicit shell.
3. Workdir default and relative/absolute `--dir` use the documented config-root
   resolution. Inherited environment remains available; endpoint and proc-compose
   variables override conflicting inherited keys; receipt contains no values.
4. Two concurrent verifications of the same session: exactly one acquires;
   the loser does not mutate state or stop the winner. Distinct sessions receive
   distinct declared endpoints and do not stop one another.
5. Replace session metadata/run after acquisition, then trigger cleanup: the
   stale attempt cannot send shutdown to or kill the replacement. Verify under
   cancellation as well as normal completion.
6. Healthy process with and without probes distinguishes process-started from
   probe-backed readiness. Startup timeout prevents check execution.
7. Check nonzero, check timeout, signal during startup/check, required service
   exit/restart during check, and task failure each preserve the correct phase
   and result. Check zero cannot mask a required-service failure.
8. Check failure plus cleanup failure preserves both; cleanup gets its full
   independent budget after cancellation. Check and stack descendants are
   absent when reported clean; an escaped descendant is explicit unverified.
9. Large output and journal rotation stay within receipt/page limits, expose
   truncation and gaps, and do not silently advance past missing event ranges.
10. Receipt directory unwritable, interrupted write, or process termination
    after initial receipt leaves truthful failure/incomplete evidence where
    possible; no unwritten receipt is claimed as retained.
11. Missing Git metadata, dirty tree, concurrent source edit, and source edit
    during the attempt preserve provenance limitations and never claim loaded
    code freshness.
12. JSON stdout contains exactly one receipt (no check stdout or human notice);
    diagnostics go to stderr. Verify documented status-to-exit-code mapping.
13. Invalid config and occupied-session JSON responses contain every schema
    field, explicit null/not-run checks, exit 2, and no receipt write; parser
    failures before JSON invocation recognition exercise the documented
    exception.
14. Escaped/oversized argv, process selectors/generations, endpoint maps, and
    metadata hit exact serialized-byte caps; required identity/verdict survives
    and optional evidence alone is truncated with disclosure.
15. In shared runner tests, delay an in-flight signal near the withdrawal reserve,
    then block `cmd.Wait()` beyond the cleanup deadline. Withdrawal serializes
    against the delayed signal; no signal is possible after it returns; lease
    release follows withdrawal and evidence/result finalization. CLI returns
    failed/unverified without claiming reaping or termination. If withdrawal
    misses its budget, the test fails the runner feasibility gate; do not pretend
    caller timeout makes it safe. On Windows, also prove the Job Object handle
    is closed/sealed before lease release and that non-empty-at-seal stays
    unverified.
16. Hold a healthy owner while its receipt is nonterminal and prove it is not
    classified abandoned; separately prove abandonment requires positive
    owner-loss plus released-lease evidence.
17. Persist terminal result, prune old terminal receipts, then release lease;
    preserve active/indeterminate receipts. Force final receipt persistence
    failure and prove no pruning, explicit JSON failure, and lease release only
    after signaling authority withdrawal and evidence finalization. Simulate
    runtime-dir loss without claiming durable retention.
18. Force overflow after optional evidence/provenance removal and validate the
    exact `verify.compact_error` schema, fixed required fields, canonical digest
    formats, omitted-field disclosure, phase verdicts, correct exit precedence,
    and `status` never indicating success.

The check command is rerunnable per attempt; current startup tasks have a
different lifecycle. Reuse execution and teardown primitives without silently
redefining existing `mode: task` behavior. Introduce a separate receipt module
only if its responsibility warrants one; extend existing tests for existing
modules and avoid duplicating journal events into a parallel log system.

## Follow-on milestones

### M2 — Attempt-aware diagnosis

Implemented: journal events carry `attempt_id`; `logs.Options.AttemptID`; `verify.Diagnose` and `why --attempt <id|latest>`; `attempts` listing with evidence-based classification. Old journals fall back to the attempt's exclusive run ID with a disclosed limitation.

- Add attempt boundaries/references to the existing event model.
- Extend `why` to select an attempt and its process generations/dependencies.
- Summarize state changes; optionally group identical output while preserving
  counts and access to originals.
- Acceptance: old failures and unrelated runs are not presented as current
  evidence; missing history is explicit. Temporal association is not labelled
  an established business-level cause.

### M3 — Explainable configuration

Implemented: `config schema` (reflection-generated from struct tags, parity-tested), `config check --json` (codes with path/line/column/hint), `config plan --json` (levels, readiness kinds, endpoint-reference treatment). Not done: doctor confidence evidence and observed endpoint-binding mismatch diagnosis; only the first semantic `Load` error is reported per run. `config check` walks merge keys with yaml.v3 v3.0.1 precedence, including the decoder suppressing a merged key named like the parent's `<<` directive. The originating agent closed this follow-up for the reviewed Linux scope on 2026-10-13.

- Publish a versioned schema and structured validation locations/codes.
- Expose the resolved startup/dependency plan and endpoint-reference treatment.
- Extend existing doctor confidence/manifest metadata with actionable evidence.
- Diagnose observed endpoint-binding mismatches without pretending static scans
  prove runtime behavior.
- Acceptance: invalid fields/references produce precise repair information;
  schema/parser parity is tested where they cannot share a source of truth;
  planning remains distinguishable from live execution.

### M4 — Session handoff

Implemented: `ipc.Project.owner` (command, PID, `PROC_COMPOSE_OWNER` label, worktree, start time) and a read-only `sessions` command with latest attempt and stop instructions. Retention decision D9: `up --silent` sessions are the intentional retained environments; `verify` never retains.

- Add discoverable creator/owner, worktree, endpoint, and latest-receipt metadata.
- Design explicit retain/handoff/reuse semantics and cleanup authority.
- Render the same information in human and machine interfaces.
- Acceptance: another reader can inspect an environment without acquiring
  mutation authority by accident; retained sessions have clear stop instructions.

### M5 — Thin integrations

Implemented: CI and agent recipes in README and the embedded guide; `verify --json`/`compare --json` share the operation/result implementation. MCP deferred (D10) until pilot evidence shows benefit.

- Publish short CLI recipes for agent actions, setup workflows, and CI.
- Add a small MCP surface only after the core workflow demonstrates benefit.
- Reuse the operation/result implementation across transports.
- Acceptance: integrations preserve deadlines, errors, identity checks, output
  budgets, and cancellation behavior; no alternate lifecycle engine emerges.

### M6 — Baseline/candidate comparison

Implemented: `compare --baseline DIR --candidate DIR [--repeat N]`, sequential sides with alternating order, `PROC_COMPOSE_COMPARE_SIDE` for caller-isolated data, outcome digests, median check duration, and explicit limitations. Assertions/artifacts beyond the check's exit/output are not compared.

- Accept caller-provided worktrees and explicitly isolated fixtures.
- Execute the same external scenario and associate each result with its receipt.
- Compare assertions/artifacts and repeated measurements where timing matters.
- Acceptance: both environments and cleanup outcomes remain distinguishable;
  uncontrolled data/dependencies or nondeterminism are disclosed; differences
  are not automatically attributed to the code change without adequate controls.

## Native CI readiness

Assessed read-only on 2026-10-13, then refreshed after the PR #1 follow-up
fixes the same day. Nothing was pushed or triggered.

### State of the checks for the reviewed changes

| Check | Status for the reviewed working tree | Evidence |
|---|---|---|
| Linux native, full `go test ./... -race -count=1` | Passed on the final working tree (16 packages). The earlier intermittent runner failure was a lost exit status, fixed with a deterministic regression. | Record entry "Review follow-up, lost exit status, native Windows runs" |
| `go vet`, GOOS linux/windows/darwin | Passed (static analysis only) | Same |
| Windows native tests | **Failed on `60352dc`** (run 38292246544). Follow-up: run locally on Windows 10.0.26200 through WSL interop, without `-race`. All packages pass except 10 tests that need a Windows `go` toolchain (unavailable). The intermittent log-rotation failure (`STATUS_DELETE_PENDING`) is fixed: 200/200 native passes. CI still pending. | Same |
| macOS native tests | **Cancelled** on `60352dc`; follow-up unrun. | Same |
| `golangci-lint` (CI `lint` job) | **Failed on `60352dc`** (`ineffassign`, fixed by inspection). Not installed locally. | Same |
| `govulncheck` (CI `vulncheck` job) | Passed on `60352dc`. Not installed locally. | Same |
| CI run | **Pending.** Requires the reviewed follow-up commit to be pushed to PR #1. | — |

### What CI would exercise natively

[CI](.github/workflows/ci.yml) runs `go build` and `go test ./... -race -count=1`
on `ubuntu-latest`, `macos-latest`, and `windows-latest` (Go 1.26.9; local runs
used 1.26.8), plus lint and vulncheck on Ubuntu. It triggers on a push to `main`
and on a pull request targeting `main`. It has no `workflow_dispatch`.

| Contract | Windows native | macOS native |
|---|---|---|
| Job Object seal, close, and pre-close observation (`TestWindowsJob_*`) | Runs | n/a |
| Seal waits for an in-flight signal (`TestRunnerSweep_SealWaitsForSignalAndPreservesUnverifiedResult`) | Runs | Runs |
| Verify operation: happy path, startup timeout, check failure/timeout, required-service exit and masking, interruption during check, receipt-write failure, concurrent sessions (`internal/verify/verify_test.go`) | Runs, except `TestDiagnose_ScopesToAttemptAndGroupsOutput` (POSIX shell) | Runs |
| Blocked-reap handoff and check quiescence (`TestRunnerCleanupHandoff_SealsBeforeReturningAndQuiescesWorker`, `TestRunCheck_StackShutdownQuiescesActiveCheck`, `TestRunCheck_StackHandoffBoundsBlockedCheckReap`) | Runs (moved to the untagged `runner_test.go` on 2026-10-13; compiled, **never executed natively**) | Runs |
| Single cleanup budget with a blocked check reap (`TestRun_BlockedCheckReapSpendsOneCleanupBudget`) | Runs (moved to the untagged `verify_test.go` on 2026-10-13; compiled, **never executed natively**) | Runs |
| Admission race and failed `Track` (`TestRunnerAdmission_*`, `TestRunnerCleanupHandoff_FailedTrackDoesNotHoldAdmissionOrRun`) | Runs (moved to untagged `runner_test.go` after the containment fix; compiled, **never executed natively**) | Runs |
| Containment before execution (`TestWindowsJob_ImmediateDescendantIsContained`, `_CancelBeforeTrackNeverRuns`, `_ResumeFailureEndsTheContainedCommand`, `_TrackRaceWithKillAndClose`, `_TrackRefusesAClosedHandle`) | Runs (compiled, **never executed natively**) | n/a |
| Check-descendant cleanup and escaped descendants (`verify_unix_test.go`) | **Not compiled** (POSIX shell fixtures) | Runs; the escaped-descendant cases skip if `setsid(1)` is absent |
| CLI exit codes and JSON contract (`TestVerifyCLI_ExitCodesAndJSONContract`) | Skipped (POSIX `sleep` fixture) | Runs |

Without `-v`, a green job does not show which tests skipped.

### Blockers

1. **Follow-up not committed.** `60352dc` is on PR #1 (draft). The release
   fixes are uncommitted and await originating-agent review; CI then needs one
   follow-up commit pushed to `agent-runtime-verify`. Record the head SHA, the
   merge SHA, and the run ID.
2. **Windows coverage gap: product fix made, native proof pending.**
   - The blocked-reap handoff, check-quiescence, and single-cleanup-budget
     regressions are platform-neutral and use real Job Objects on Windows.
   - The containment gap is fixed in the shared runner: Windows creates the
     command with `CREATE_SUSPENDED`, assigns it to its Job Object, and only
     then resumes it. Cancellation before `Track` leaves it suspended inside
     the job for teardown; assignment or resume failure ends it before it runs.
     The admission-race and failed-`Track` regressions now run on Windows.
   - Until a native Windows job passes on the follow-up commit, Windows
     containment stays **unverified**. A green Windows job verifies these only
     if its log shows that they ran.
3. **Skip visibility (optional).** The CI log cannot distinguish passed from
   skipped cleanup tests. See the proposed diff below.
4. **Lint** failed on `60352dc`; the fix is verified by inspection only. The
   `lint` job installs `golangci-lint` `latest` with Go 1.25, so a new linter
   release can change the result independently of this code.

### User actions to publish and trigger CI

1. After originating-agent review, commit the follow-up on
   `agent-runtime-verify` and push it to PR #1. Do not push to `main`.
2. Watch it (`gh pr checks 1 --watch`), then record the head and merge SHAs,
   the run ID, and each job's conclusion in the record. Treat a passing
   Windows job as evidence only for the rows marked "Runs" above.
3. Release readiness requires every job green on that reviewed commit.

### Proposed workflow change (not applied; awaiting approval)

Trigger: blocker 3. The current log cannot show whether the seal, handoff, and
cleanup regressions ran or skipped on macOS and Windows. Proposed change to the
existing `Test` step of the `test` job in `.github/workflows/ci.yml`; it adds
skip visibility without rerunning the suite:

```diff
       - name: Test
-        run: go test ./... -race -count=1
+        run: go test ./... -race -count=1 -v
```

The cost is a much longer log. The workflow does not need to change for CI to
run: a pull request against `main` triggers it.

## Pilot 2 protocol

Status: **proposed, not run; awaiting approval.** Revised 2026-10-13 after
review (tool restriction, per-trial evidence, interrupt targeting, timing
comparability) and for the switch to opencode + MiniMax. Freeze this section,
with any approved edits, after calibration and before T01. Results from earlier
pilots are not reused as data.

### Question and conditions

Does the reviewed candidate improve agent outcomes over the released tool in the
scenarios where the plan predicts benefit: interruption, service failure during
a check, and multi-service diagnosis?

| Condition | Build identity (record at freeze) |
|---|---|
| **B**, baseline | Released `proc-compose v1.5.2`, the installed `~/.local/bin/proc-compose`, with its embedded guide. Record `--version`, the binary's SHA-256, and, if approved, a match against the v1.5.2 release asset checksum. |
| **C**, candidate | Built from the CI-verified commit (blocker 1 above) with the cached Go 1.26.8 toolchain: `go build -trimpath -ldflags "-X main.version=pilot2-<sha12>"`. Record the commit SHA, the binary's SHA-256, the toolchain, and the CI run ID. A dirty-tree build is not allowed. |

### Agent configuration

Both conditions use the same agent and model; only `{BIN}` differs.

- **Agent:** opencode 1.18.35, headless:
  `opencode run --pure --format json -m minimax/<MODEL> --dir <trial dir> --auto "<prompt>"`.
  Each trial is a fresh process and a new opencode session; `--continue`,
  `--session`, `--fork`, `--share`, and `--attach` are never used.
- **Model:** `minimax/MiniMax-M3` (standard, not `-highspeed`), through the
  `minimax` provider credential, never `opencode-go/` or `openrouter/`.
  Chosen by the maintainer on 2026-10-13. Entitlement is still to be confirmed
  by the first calibration call. If the subscription does not include M3, stop
  and ask; do not substitute another model.
- **Isolation:** `XDG_CONFIG_HOME` points to a pilot-only opencode config
  directory. The credential store under `XDG_DATA_HOME`
  (`~/.local/share/opencode/auth.json`) stays in place. The pilot config
  defines only the MiniMax provider and model, configures no MCP servers, and
  sets permissions to allow the bash tool, deny every other built-in tool, and
  deny edits. `--auto` approves only what that config leaves undenied.
- **Calibration acceptance for isolation**, from `--print-logs --log-level DEBUG`
  and the first request:
  - The tool list exposed to the model is exactly bash; no MCP tools.
  - No instruction file is loaded: no `~/.claude/CLAUDE.md`, no global or
    project `AGENTS.md`, no skills.

  If either fails, fix the configuration and repeat; never start trials with
  unverified isolation.
- **Budget per trial:** 15 tool calls (the runner counts tool-use events in the
  JSON stream and terminates the agent at the 16th) and a 10-minute
  wall-clock cap. A trial that hits either is `incomplete`. Spending is limited
  in the MiniMax account, because opencode has no per-run cost cap. Agent runs
  in total: 24 measured, 2 warm-ups, 2 interrupter calibrations, and 1
  isolation probe, so 29 runs against the subscription quota.
- **Data:** prompts, tool output, app logs, and parts of the app config
  (including a committed session-secret value) are sent to MiniMax. The
  maintainer consented on 2026-10-13.

### Application and fixtures

Representative application: `~/projects/municipality-central-system` at
`a6b7356b` (pnpm monorepo, Node API, Next.js apps, local PostgreSQL). Its
existing `proc-compose.test.yml` defines `migrate` (task) → `api`
(`127.0.0.1:4003/health`) → `core`/`portal`/`bims`. The repository is not
modified. Fixture configs live in a pilot directory outside both repositories.
They copy the needed process entries from `proc-compose.test.yml` and set
`dir:` to the application root (supported by v1.5.2 and the candidate).

Every fixture script appends one-line JSON records to a per-trial evidence
file outside the trial directory. The records are timestamped and include the
PID, PGID, and start time. The agent is never told this path.

| Scenario | Selection and fixture | Check given to the agent | Expected verdict |
|---|---|---|---|
| S1 healthy verification | `api` (closure `migrate` → `api`), unmodified | `s1-check.sh`: GET `/health` and one read-only API route chosen at calibration, asserting status and a body token; records each request and assertion | `PASS` |
| S2 interruption during the check | As S1 | `s2-check.sh`: GET `/health`, record a start marker, sleep 90s, exit 0. A trap records SIGINT/SIGTERM receipt. The interrupter acts as described below. | `INTERRUPTED` (never `PASS`) |
| S3 required service fails during the check | `api` wrapped: the wrapper records its start, starts the real command, and when a per-trial fault file appears records `PILOT_FAULT_S3` and terminates its own process group | `s3-check.sh`: GET `/health`, create the fault file, sleep 5s, record exit, **exit 0** | `FAIL_SERVICE`, `FAILED_SERVICE: api` |
| S4 multi-service dependency failure and diagnosis | Selection `core` (closure `migrate` → `api` → `core`). The `api` env adds `NODE_OPTIONS=--require <pilot>/fault-s4.cjs`, which records the fault and throws `PILOT_FAULT_S4: required setting missing` at boot. `core`'s command is wrapped to record a start before `exec`. | `s4-check.sh`: GET `core` `/api/health`; records the request and its result | `FAIL_STARTUP`, `FAILED_SERVICE: api`, `CAUSE` names the S4 fault, and `core` is reported as blocked |

Independent of the agent, a runner-side health poller GETs the `api` and
`core` health endpoints once per second for the whole trial and records each
result. These read-only requests are the service-state timeline.

Calibration, before T01:

- Run each fixture once per condition with a scripted, non-agent invocation to
  confirm that every expected evidence record is produced.
- Run the two interrupter calibrations (one agent run per condition, unscored,
  with a fixed prompt) as described in the interruption procedure.
- Run one unscored warm-up per condition so pnpm/Node caches are equally warm.

Calibration failures change the fixture before freezing, never after.

Equivalent state for each trial:

- A fresh trial directory, and a unique session name (`p2-T01`…`p2-T24`).
- The same app HEAD and the same `git status --porcelain` digest.
- The SHA-256 of every fixture, check, wrapper, preload, and pilot config file
  is recorded before the trial.
- The database is not reset or seeded. `migrate` runs every trial and is a
  no-op on an up-to-date schema. The checks are read-only.
- A pre-scan must be clean: ports 4003 and 3211–3213 free, no process with the
  app root or the pilot directory in its command line or cwd, and no live
  proc-compose session for the pilot. A dirty pre-scan aborts the trial. It is
  rerun at the end of the sequence and flagged.

### Prompt (identical across conditions except `{BIN}`)

```text
You are working in {TRIAL_DIR}. A process manager is installed at {BIN}.
Before anything else, run `{BIN} agent-guide` and follow it.
Project config: {CONFIG}. Use the session name `{SESSION}` for every command.
Task: {TASK}
Do not modify, move, or delete any file you were given, including the config
and check scripts, and do not modify files anywhere else. Do not ask
questions; nobody will reply.
End your final message with exactly these three lines:
VERDICT: <PASS|FAIL_CHECK|FAIL_STARTUP|FAIL_SERVICE|INTERRUPTED|UNKNOWN>
FAILED_SERVICE: <process name or none>
CAUSE: <one line>
```

The task text for S1–S3 is: "Start the `api` service with its dependencies,
confirm it is ready, and run this check against it: `{CHECK}`. Report whether
the running service was verified." For S4 it is: "Start the `core` service
with its dependencies and run `{CHECK}`. If it does not work, find which process
failed and why." The prompt deliberately says nothing about cleanup, because
leaving nothing running is part of what is measured.

### Order (counterbalanced, fixed before T01)

The scenario order rotates per repetition. Each scenario's condition order
alternates across repetitions, so baseline goes first in 6 of the 12 pairs.
With three repetitions, each scenario has a 2:1 order imbalance; report it.

| Rep | Trials |
|---|---|
| 1 | T01 S1-B, T02 S1-C, T03 S2-C, T04 S2-B, T05 S3-B, T06 S3-C, T07 S4-C, T08 S4-B |
| 2 | T09 S2-B, T10 S2-C, T11 S3-C, T12 S3-B, T13 S4-B, T14 S4-C, T15 S1-C, T16 S1-B |
| 3 | T17 S3-B, T18 S3-C, T19 S4-C, T20 S4-B, T21 S1-B, T22 S1-C, T23 S2-C, T24 S2-B |

### Interruption procedure (S2)

- **Target rule:** the target is one process: the direct child of the agent's
  tool-call shell on the ancestor chain of the check script. In practice this
  is the program the agent ran in the foreground. Expected: the check script
  (or the shell running it) in B, and the `proc-compose verify` process in C.
  The runner sends SIGINT to that PID only, never to a process group.
- **Identity:** when the S2 start marker appears, the runner walks the
  ancestor chain from the marker's PID through `/proc/<pid>/stat` up to the
  agent process (the PID the runner spawned). It records the chain and the
  target's PID, PGID, start time, and command line.
- **Exclusions:** the target must be a strict descendant of the agent process.
  It must not be the agent, the runner, the tool-call shell itself, the
  proc-compose daemon (`up --silent` in B), or any process that existed at the
  pre-scan. Any violation means the signal is not sent and the trial is
  `interrupt_not_delivered`.
- **Recheck:** 3s after the marker, immediately before signaling, the runner
  re-reads the target's start time, command line, and ancestry. Any mismatch,
  or a target that has already exited, means no signal, recorded as
  `interrupt_not_delivered` with the reason.
- **Delivery and acknowledgement:** delivery is a successful `kill(pid,
  SIGINT)` return, with its timestamp. Acknowledgement is recorded separately
  as any of: the target exiting within 10s (observed through a pidfd), or a
  check-script trap record. Neither is inferred from the agent's verdict.
- **Not delivered:** an `interrupt_not_delivered` trial is reported
  separately, excluded from S2 comparisons, and rerun at the end of the
  sequence with a flag.
- **Calibration:** one unscored agent run per condition, with a fixed prompt
  telling the agent which command to run, must show that the rule selects the
  expected target in both conditions, that the exclusions hold, and that the
  acknowledgement is observed. If the rule selects different kinds of target
  across conditions in an unexpected way, revise it before freezing.

### Independent verification

The trial runner, not the agent, decides each outcome, using per-trial evidence
rather than fixture construction alone.

| Scenario | Required per-trial evidence |
|---|---|
| All | Check start record, completion or signal record, and exit status (for S4, the check request record). Fixture/config hashes unchanged after the trial. |
| S1 | Every request and assertion recorded as passed. Health poller shows `api` healthy during the check. |
| S2 | Interrupt delivery record; acknowledgement record; no check completion record after delivery |
| S3 | Fault fired; `api` wrapper termination recorded; health poller shows `api` down after the fault; check exited 0 |
| S4 | S4 fault record from `api`; no `core` start record; health poller never saw `core` healthy |

Each required item is classified `observed`, `contradicted`, or `missing`.

Outcome classes, one per trial:

| Class | Meaning |
|---|---|
| `completed` | All required evidence observed; reported verdict, `FAILED_SERVICE`, and (S4) cause match the expected values; within budget |
| `wrong_verdict` | Evidence observed, but the report does not match |
| `false_success` | Defined below. It overrides the other classes. |
| `not_executed` | The report matches, but the evidence shows the requested workflow did not run (for example, no check start record). Never counted as completion. |
| `evidence_missing` | A required item is missing for a reason not attributable to the agent (a fixture or runner failure). Excluded from comparisons and rerun at the end with a flag. |
| `tampered` | A fixture, check, or config hash changed, or the evidence file was touched. Not completion; reported separately. |
| `incomplete` | Tool-call or wall-clock cap reached |
| `interrupt_not_delivered` | S2 only; see the interruption procedure |

Report parsing reads the last `VERDICT`/`FAILED_SERVICE`/`CAUSE` lines of the
final assistant message. For `CAUSE` in S4, match on the fault token or its
message. Disputed classifications go to a human reviewer who reads the
transcript and evidence blind to condition (binary path redacted); record the
dispute.

Cleanup: a post-scan runs 10s after the agent process exits, using the same
probes as the pre-scan, plus the pilot session's runtime directory and PID
file. For C, it also reads the receipt state. Afterwards the runner may stop
leaked processes it identifies by session or pilot path, and records that as
`harness_cleanup`.

### Definitions

| Measure | Definition |
|---|---|
| Completion | Outcome class `completed` |
| False success | `VERDICT: PASS` when the expected verdict is not `PASS`, or any `PASS` in a trial whose evidence lacks a check completion with exit 0 (or, in S1, lacks a passing assertion) |
| Leak | A process, listener, or live session attributable to the trial (app root, pilot path, trial session, or ports 4003/3211–3213, excluding pre-scan PIDs) present at the post-scan. Count distinct processes. |
| Intervention | Any human action during the trial that affects the agent or the environment, other than the scripted interrupter: replying, killing the agent, or manual cleanup before the post-scan. Automatic cap termination and post-scan `harness_cleanup` are recorded separately. |
| Wall-clock time | Monotonic time from spawning the agent process to its exit. Pre/post scans are excluded. |
| Observed tool-call interval time | For each tool call, the time from the runner receiving its tool-use start event to receiving its result event, stamped on receipt from the JSON stream. Report the sum and the union of intervals. It is not command execution time: actual command start/end timestamps are not collected, except for the fixture scripts' own records. |
| Tool calls | Count of tool-use events. Report separately the `proc-compose` invocations among them. |
| Tokens | Per-step usage fields from the opencode JSON events (input, output, reasoning, and cache read/write where present), reported separately and never summed with bytes. Field names are pinned at calibration; if the events carry no usage, tokens are `missing`, not estimated. |
| Tool output bytes | UTF-8 bytes of tool-result content (bytes, not tokens) |

### Trial record (one JSON line per trial; retained)

```json
{"trial":"T01","rep":1,"scenario":"S1","condition":"B","order_position":1,
 "tool_version":"v1.5.2","binary_sha256":"<sha256>","source_commit":null,"ci_run_id":null,
 "agent":"opencode 1.18.35","model":"minimax/MiniMax-M3","budget":{"tool_calls":15,"wall_s":600},
 "app":{"path":"municipality-central-system","head":"a6b7356b","status_digest":"<sha256>"},
 "fixture_hashes":{"before":"<sha256 of manifest>","after":"<sha256 of manifest>"},
 "session":"p2-T01","started_at":"<rfc3339>","finished_at":"<rfc3339>",
 "wall_s":0.0,"tool_interval_sum_s":0.0,"tool_interval_union_s":0.0,
 "tool_calls":0,"proc_compose_calls":0,"tool_output_bytes":0,
 "tokens":{"input":0,"output":0,"reasoning":0,"cache_read":0,"cache_write":0,"source":"opencode-json"},
 "reported":{"verdict":"PASS","failed_service":"none","cause":"..."},
 "expected":{"verdict":"PASS","failed_service":"none","cause_token":null},
 "evidence":{"check_start":"observed","check_end":"observed","check_exit":0,
   "scenario_items":{"s1_assertions":"observed","api_healthy_during_check":"observed"}},
 "outcome":"completed","false_success":false,"incomplete_reason":null,
 "leaks":{"count":0,"items":[]},"harness_cleanup":false,"interventions":0,
 "interrupt":null,
 "prescan_clean":true,"aborted":false,"dispute":null,
 "transcript":"<trial dir>/transcript.jsonl","evidence_file":"<pilot>/evidence/T01.jsonl","notes":""}
```

For S2, `interrupt` is
`{"target_pid":0,"target_pgid":0,"target_cmdline":"...","chain":["..."],"rechecked":true,"delivered_at":"<rfc3339>","delivery":"delivered|not_delivered","not_delivered_reason":null,"ack":"target_exited|trap_recorded|none_within_10s"}`.

Retained per trial in the private pilot directory: prompt, raw opencode JSON
event stream with receipt timestamps, evidence file, health-poller log, pre/post
scans, hash manifests, verifier output, and, for C, the receipt. The record
links summaries only. Do not commit transcripts, because they may contain values
from the application config.

### Success criteria (frozen before T01)

The original targets are from the strategy. B1 is a proposed addition and
**needs the maintainer's approval** before freezing; without approval it is
removed.

- **G1, lifecycle gate (original):** C has zero false successes and zero leaks
  across its 12 trials. On failure: stop expansion and investigate.
- **G2, no regression (original):** C completes at least as many trials as B
  (of 12), and no scenario has C behind B by 2 or more.
- **Benefit**, at least one of:
  - **B2, time (original 30% target):** evaluated only on trials that are
    `completed` and have no leak, no cap, and no intervention, so that a faster
    failure cannot count as a speed win. S2 is excluded, because its duration is
    set by the interrupter. For each of S1, S3, and S4 with at least 2 such
    trials per condition, take the ratio of median wall-clock times (C/B). Pass
    requires at least 2 evaluable scenarios, a geometric mean of ratios ≤ 0.70,
    and C lower in every evaluable scenario but at most one. Failed, incomplete,
    and capped trials are reported separately with their times.
  - **B3, interventions (original 30% target):** only evaluable if B has 3 or
    more interventions; C then needs at least 30% fewer. Zero baseline
    interventions cannot support a relative reduction, so B3 is then reported as
    not evaluable.
  - **B1, correctness (proposed; pending approval):** B has 2 or more trials
    with `false_success`, a leak, or `wrong_verdict`, and C has none.
- **Decision:** G1, G2, and a benefit → continue, by validating with a second
  model before any integration work. G1 and G2 without a benefit → revise; do
  not expand. G1 or G2 fails → stop and investigate.
- Tokens, tool calls, bytes, and tool-call interval time are descriptive only.
  With n=3 per cell, report medians and ranges; make no significance or causal
  claims.

### Requires user approval before execution

1. Use of `~/projects/municipality-central-system` (`a6b7356b`): running its
   `corepack pnpm db:migrate`, `--filter api start`, and `--filter core start`
   commands through proc-compose. No file changes; its existing uncommitted
   docs change and `.vscode/` are left alone.
2. The local PostgreSQL database `municipal_test` (`/run/postgresql`):
   `migrate` runs each trial, and `api` connects to it. No reset, seed, or
   check-driven writes. Optionally, a read-only `psql` count of applied
   migrations before and after the pilot.
3. Ports 4003 and 3211 on 127.0.0.1, and once-per-second read-only health
   polling of them during trials.
4. A new private directory outside both repositories (proposed
   `~/proc-compose-pilot2/`) holding fixture configs, check scripts,
   `fault-s4.cjs`, the S3 and `core` wrappers, the interrupter, the health
   poller, a small trial runner, the pilot-only opencode config, the candidate
   binary, and the retained evidence. The trial runner is an approved-execution
   artifact, not a product benchmark harness.
5. Building the candidate from the CI-verified commit, and optionally
   downloading the v1.5.2 release checksum with `gh`.
6. About 8 scripted calibration runs, which make no agent calls, and 29
   opencode runs on `minimax/<MODEL>` against the MiniMax subscription quota:
   24 measured, 2 warm-ups, 2 interrupter calibrations, and 1 isolation probe.
7. Sending prompts, tool output, app logs, and app config values to MiniMax
   (consented 2026-10-13).
8. Interrupter authority: sending SIGINT to one rechecked target PID per S2
   trial and per interrupter calibration.
9. Post-scan cleanup authority over processes attributable to a trial.

## Verification ladder

Use the narrowest checks that can disprove each change, then complete affected
checks at the milestone boundary. Exact test selectors depend on the changes;
the commands below are planning guidance, not a record that they passed.

1. Documentation-only work: review relative links, proposal/current-feature
   labels, source references, and `git diff --check`.
2. M0: targeted bootstrap and CLI regression tests, then affected package tests
   with the race detector and formatting checks.
3. M1: targeted runner/session/IPC/query/CLI tests as touched, then affected
   packages with `-race -count=1`; vet/build and repository-configured lint.
4. Lifecycle/platform changes: real temporary-process fixtures covering endpoint
   wiring, descendant cleanup, cancellation, and concurrent ownership. Verify
   task-owned processes exit and ports release. Distinguish native runtime tests
   from cross-compilation evidence.
5. Cross-cutting milestone completion/release triggers the broader repository
   gates in [CI](.github/workflows/ci.yml), including the platform build/test
   matrix and vulnerability check. [Makefile](Makefile) `make test` runs
   `go test ./... -race -count=1`; state the trigger before broad local runs.

Record command, source revision, platform/toolchain, result, and deferred checks
in the progress record. Earlier milestone results are historical evidence and
must not be copied forward as proof for changed code. Do not start or reconfigure
unrelated applications while exercising lifecycle fixtures.
