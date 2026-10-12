# Agent-assisted development: product strategy

Status: direction agreed with the maintainer on 2026-10-12. M0–M6 first
implementations landed on 2026-10-12; the reviewed implementation was accepted
for Linux on 2026-10-13. Native Windows/macOS verification is pending. A
tool-level pilot and n=1 agent runs are recorded, but no benefit has been
established. The three-repetition agent pilot is proposed in the plan and not
yet run. See the record.

## Start here

- This document records the product thesis, research, priorities, and evaluation.
- [Implementation plan](AGENT_RUNTIME_PLAN.md): milestones, contracts, acceptance,
  implementation seams, and verification gates.
- [Decision and progress record](AGENT_RUNTIME_RECORD.md): decisions, open
  questions, evidence, and the next action. Update it as work proceeds.
- [Existing session/log milestone](SESSION_LOGS_MILESTONE.md): previously
  recorded implementation and verification evidence, not evidence for new work.

The initial source inspection used `main` at
`3e7b389a6e6fb848b48b2e8a895f54ab79d9c4af`. Refresh implementation assumptions
against the actual checkout before starting each milestone. Proposed commands
and interfaces in these documents are design sketches, not released features.

## Product thesis

**Give every coding task a runnable environment, a reliable feedback loop, and
evidence of what actually worked.**

Position proc-compose as the runtime verification layer for agent-assisted
development. Connect a code change, its running environment, application checks,
failure evidence, and cleanup into one dependable workflow.

The breakthrough to pursue is making an agent's claim that "it works"
inexpensive to check. This is a product hypothesis to validate through real use;
research and a feature plan alone do not establish a breakthrough.

### Initial users and jobs

Start with developers using coding agents on local multi-process applications:
frontend/API stacks, database-backed APIs, and applications with workers.

| User need | Human outcome | Agent outcome |
|---|---|---|
| Configure an unfamiliar project | Understand suggested YAML and missing setup | Repair configuration using precise diagnostics |
| Run a task-specific stack | Obtain working URLs without disrupting another task | Track explicit session identity and lifecycle ownership |
| Verify a change | See exactly which checks ran and their results | Get a bounded, machine-readable verification result |
| Investigate a failure | Inspect relevant runtime evidence | Avoid unrelated logs and stale process generations |
| Resume or review work | Open the correct environment and retained artifacts | Recover context without guessing what is still running |
| Finish a task | Know whether managed processes were stopped | Receive an explicit cleanup verdict |

## Existing foundation

| Capability already present | Source |
|---|---|
| Project discovery, findings, suggested YAML, confidence and manifest metadata | [doctor report](internal/doctor/report.go), [bootstrap](internal/bootstrap/bootstrap.go) |
| Named sessions, endpoint assignments, runtime directories, advisory resource leases | [session](internal/session/session.go) |
| Service/task modes, dependencies, readiness configuration | [config](internal/config/config.go), [runner lifecycle](internal/runner/lifecycle.go) |
| Run/process generation identity and versioned runtime events | [IPC model](internal/ipc/ipc.go) |
| Bounded log pages, filters, resumable cursors, gap disclosure | [log queries](internal/logs/query.go) |
| Live or retained failure evidence and dependency blockers | [why command](cmd/proc-compose/why.go) |
| Offline agent operating instructions | [agent guide](internal/agentguide/GUIDE.md) |

Extend these components. Rejected reuse alternatives are a second process
supervisor, a separate log store for agents, and a competing test language:
they would duplicate existing lifecycle, event, and project-test contracts.

### Observed gaps

1. Agents must assemble startup, verification, diagnosis, and cleanup themselves.
2. `bootstrap --verify` executes a dry-run, but its verifier sets `Started` and
   `Ready` true on success. See [verify.go](internal/bootstrap/verify.go).
   Machine output must distinguish config validation from live checks.
3. The [agent guide](internal/agentguide/GUIDE.md) documents that startup
   readiness timeout can leave a newly started daemon running and that failed
   commands do not consistently return JSON.
4. Current process generations and journal cursors do not by themselves identify
   a complete application verification attempt or the source it exercised.
5. Named sessions separate declared resources; databases, build outputs, and
   hardcoded ports still require explicit application-specific isolation.

These are inspection findings. No new runtime benchmark was conducted for this
strategy, and the inspected repository files contained no recorded agent-task
timing dataset.

## Research and competitive context

Sources consulted on 2026-10-12. Product documentation can change; recheck it
before implementing a third-party integration.

| Source | Observation | Product implication |
|---|---|---|
| [OpenAI: Harness engineering](https://openai.com/index/harness-engineering/) (2026-02-11) | Describes per-worktree application instances, local observability, and agent-driven browser verification | Make the runtime feedback loop accessible without requiring every project to construct it from scratch |
| [Anthropic: Writing effective tools for agents](https://www.anthropic.com/engineering/writing-tools-for-agents) (2025-09-11) | Recommends focused workflow tools, useful context, bounded output, and empirical tool evaluations | Consolidate high-value operations and measure correctness as well as interaction cost |
| [Anthropic: Code execution with MCP](https://www.anthropic.com/engineering/code-execution-with-mcp) (2025-11-04) | Discusses context costs from tool definitions and intermediate results | Keep CLI composition useful and MCP surfaces small; published savings are not proc-compose measurements |
| [Codex local environments](https://developers.openai.com/codex/app/local-environments/) | Documents worktree setup scripts and common project actions | Existing agent environment actions are integration points for proc-compose |
| [Codex worktrees](https://developers.openai.com/codex/environments/git-worktrees.md) | Documents concurrent checkouts and handoff workflows | Runtime identity and resource ownership should remain explicit across worktrees |
| [Process Compose MCP documentation](https://f1bonacc1.github.io/process-compose/mcp-server/) | The separate `process-compose` project exposes process controls, readiness, dependency graphs, and ranked log search | MCP and searchable logs alone provide limited differentiation |

This bounded research sample supports the direction, not a market-share claim
or proof that most development is agent-driven. Validate demand with users.

## Priorities

| Order | Investment | User-visible outcome |
|---|---|---|
| 0 | Truthful machine contracts | Config validation, startup, readiness, application checks, and cleanup are distinguishable |
| 1 | Verified development runs | One owned operation starts a stack, runs an existing check command, records evidence, and cleans up |
| 2 | Change-aware diagnosis | Explain what happened during one verification attempt using existing events and cursors |
| 3 | Explainable YAML authoring | Schema, structured validation, resolved startup plans, and evidence-backed suggestions |
| 4 | Human/agent session handoff | Show ownership, worktree, endpoints, latest verification, and intentional retention |
| 5 | Thin agent integrations | CLI recipes and an MCP adapter use the same runtime/result implementation |
| Later | Baseline/candidate comparison | Run one scenario against two provided environments and compare recorded behavior |

These are strategic priorities, not measured rankings of time savings. Ship and
evaluate the first complete workflow before expanding the roadmap.

### Flagship workflow

Illustrative proposed interface:

```sh
proc-compose --session checkout-fix verify --json -- npm run test:smoke
```

The operation would validate configuration, acquire an owned environment, start
services, wait for declared probes, provide resolved endpoints to the check,
execute it, retain relevant evidence, and verify cleanup. Human and JSON views
would render the same result.

A verification receipt records what was checked and what happened. Passing a
smoke test establishes that check's outcome, not universal application health.
Source metadata is provenance, not proof that a server loaded every edit.

### Longer-term: compare running versions

Given caller-provided baseline and candidate worktrees, run an existing scenario
against both and compare results: response assertions, persisted behavior,
worker outcomes, screenshots, or startup measurements. Retain each environment's
receipt and artifacts so a developer can review the behavioral difference.

Require explicitly isolated test data. Coordinate existing test/browser tools;
automatic database cloning, general-purpose replay, hosted previews, and a new
browser engine are outside the initial scope. A receipt alone does not make an
environment reproducible: dependencies, build inputs, and data must also be
controlled and their limitations recorded.

## Evaluation and adoption

### Baseline observation

Collect 5–10 real attempts using current proc-compose before ranking workflow
optimizations. Record setup, startup, checks, diagnosis, cleanup, agent errors,
and human interventions. Preserve the source revision and installed version.

### Small falsifiable pilot

Compare current proc-compose with the verified-run prototype on:

1. Frontend plus API.
2. API plus database and migration task.
3. API plus background worker.

Use healthy startup, readiness timeout, missing migration, and an application
check failure despite successful readiness as scenarios. Fix the initial
agent/model and tool budget, reset fixtures between attempts, counterbalance
condition order, and run three repetitions per condition: 72 attempts if all
three projects use all four scenarios. Validate promising results with a second
agent before broad integration work.

| Metric | Interpretation |
|---|---|
| Independently checked task completion | Did the requested outcome occur? |
| False-success reports | Did output imply verification that never happened or passed incorrectly? |
| Human interventions | How often did the developer have to rescue the workflow? |
| Wall-clock completion time | Actual elapsed time experienced by the user |
| Tool calls and returned tokens/bytes | Interaction and context overhead; bytes are not token counts |
| Leaked task-owned processes | Lifecycle correctness after success, failure, and interruption |

Report cumulative command time separately from wall-clock time when commands
overlap. Distinguish observations from causal claims; small samples and flaky
application checks limit conclusions.

Candidate go/no-go targets, to finalize before collecting comparison results:
zero false-success reports and leaked owned processes in the pilot, plus either
at least 30% lower median wall-clock completion time or 30% fewer human
interventions, without lower independently checked completion. A zero baseline
intervention count cannot support a relative reduction. These are targets, not
promised improvements or universal reliability guarantees.

Stop expansion and investigate if lifecycle correctness fails. If correctness
holds but the workflow brings little benefit, revise the operation or its
onboarding using transcripts rather than adding features speculatively.

### Distribution

Demonstrate one broken application check, its evidence, a fix, and a passing
receipt. Publish short recipes for agent environment actions and CI using the
same CLI. Extend the embedded agent guide when behavior ships. Observe whether
developers voluntarily use the workflow on subsequent tasks, not just whether
they complete the demo.
