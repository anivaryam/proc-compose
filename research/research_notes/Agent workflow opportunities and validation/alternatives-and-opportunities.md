# Local-development alternatives and agent opportunities

Study/retrieval date: **2026-10-19**. Original research only; no services, benchmarks or runtime changes. Code context: HEAD `7a9e93d3361278e72d72672f6253773c96e73ad7`, tagged `v1.10.0`. Subsequent documentation corrections follow the [adversarial review](../../reports/Adversarial%20review%20of%20agent%20research.md); no new runtime or demand evidence was added.

## 1. What do adjacent tools already cover, and for whom?

### Takeaway

Readiness, ordered tasks, and local process supervision are established capabilities, not an agent-specific opening. The plausible opportunity is a smaller, dependable operating contract for native-process projects—not replacing container orchestration or development-environment provisioning. [process-compose][P]; [Docker][D2]; [Tilt][T]; [devenv][E1]

### Cited Findings

**Methodology.** Bounded comparison of the four assigned tools using six primary URLs. Search returned no results; official pages were fetched directly. Compared documented semantics, not marketing breadth or download counts. Read local README/FEATURES only for context; source/runtime verification belongs to the parallel researcher. No prior report was treated as evidence. “Not established” below describes this source sample, not product absence.

| Alternative | Readiness and tasks/checks | Logs and automation | Isolation and cleanup |
|---|---|---|---|
| **process-compose**—closest native-process substitute | Documents readiness/liveness, dependencies and recovery; README demonstrates `process_completed`. Successful-check receipts are not established here. [P] | CLI/TUI, REST/OpenAPI with optional `PC_API_TOKEN`, cached/per-process logs, and an MCP server exposing processes and optional project controls. Bounded cursor/error contracts are not established here. [P] | Namespaces, namespace operations, environment variables and configuration merging. Worktree-specific resource ownership, port leases and verified descendant cleanup are not established by this README. [P] |
| **Docker Compose**—substitute for containerized stacks, complement for infrastructure | A running container is not necessarily ready. `service_healthy` waits for a healthcheck; `service_completed_successfully` supports successful one-shot dependencies. `run` executes one-off commands. [D2]; [D1] | CLI includes configuration rendering, `ps`, `logs`, events and port inspection; progress supports JSON. This does **not** establish JSON schemas or output bounds for every command. [D1] | Explicit project names, project directories, env files and layered configs; `down` stops/removes containers and networks, with dependency-ordered removal. Host-port/data isolation and volume-deletion policy need separate verification. [D1]; [D2] |
| **Tilt**—broader substitute, including native commands | `local_resource` has one-shot `cmd`, long-running `serve_cmd`, dependencies, optional readiness probes, file dependencies and manual/automatic triggers. `ci_settings` controls pipeline/readiness deadlines for `tilt ci`. [T] | Reference exposes CLI links for logs/get/wait and endpoint links in the UI. Machine-output guarantees and log byte bounds are not established by the inspected API reference. [T] | Local working directories/env plus Compose `project_name` integration. Build/update commands default to serial execution because shared filesystem changes can race; servers may still overlap. Worktree ownership/cleanup guarantees are not established here. [T] |
| **devenv**—environment-oriented complement **and** native orchestration substitute | Native manager documents exec/HTTP/notify readiness, dependency states and `processes wait`; tasks form a DAG, can skip execution using status/input checks and restore cached outputs. The status predicate itself is not cached. [E1]; [E2] | Native status/log/control commands, attach and task JSON I/O are documented. Task I/O does not establish bounded process-log schemas. [E1]; [E2] | Allocates free named ports and passes resolved values into configuration; holds ports during evaluation, releasing them before spawn. `down`, grace/escalation and guardian cleanup are documented. Task runs stop processes they started when the graph finishes. [E1]; [E2] |

### Inferences

- **Best-fit segment:** projects already possessing working host toolchains, running a handful of servers/workers and needing multiple branch/worktree instances. This follows proc-compose’s stated native-process scope; it is a segment hypothesis, not demonstrated demand. [local README](../../../README.md)
- **Reuse boundaries:** leave package/toolchain installation to the existing environment manager; leave container networking/images/volumes to Compose; leave an established Tilt workflow intact. Wrapping another supervisor can create competing lifecycle owners rather than value. [D1]; [T]; [E1]
- Browser harnesses are complementary check executors; delegate lifecycle to an existing harness when it already reliably owns the whole stack. No vendor/harness-specific capability claims are made here; that comparison is assigned elsewhere.

### Gaps

No comparative runtime tests, adoption interviews, platform matrix or performance measurements. Namespaces/project names are not proof of independent databases, caches, credentials or host ports. Complete worktree isolation in any alternative is **not established by the inspected documentation**. No absence claim should be synthesized from these gaps.

## 2. Which incremental contracts merit priority over reuse?

### Takeaway

Evidence and recipes before features is a conservative default hypothesis, not proven priority. Proc-compose advertises most candidate primitives; MCP is not uniquely differentiating relative to process-compose, but may still offer useful typed discovery/invocation to a named client. [local FEATURES](../../../FEATURES.md); [P]

### Cited Findings

- Proc-compose’s current documentation advertises `agent-guide`, JSON status, bounded/cursor-based logs, `why`, named sessions/ports, and `verify` receipts separating startup, check and cleanup. These are **documented existing behavior**, not newly identified implementation gaps or runtime guarantees. [local FEATURES](../../../FEATURES.md#built-for-coding-agents)
- Both process-compose’s MCP/control plane and devenv’s native ports/task-scoped cleanup disconfirm broad claims such as “agents lack process tools” or “parallel local stacks need a new orchestration platform.” [P]; [E1]; [E2]

### Inferences

**Scoring:** qualitative, nonadditive hypotheses, not demonstrated opportunity ordering. **Fit** means relevance to native multi-process work; **differentiation** means distinction supported by this sample, not proven uniqueness; **cost** is design-dependent compatibility/security/support judgment, not measured effort. Adoption/effectiveness confidence is **low for every candidate** until tested.

| Priority / candidate | Fit / differentiation / cost | Decision and disconfirmation |
|---|---|---|
| **1. Version-matched CLI recipes** | High / low / low | First test existing `agent-guide` plus recipes for start→inspect→check→cleanup and separate worktree sessions. Prefer no change if the guide already suffices. Recipes may explain use, not add capability. |
| **2. Prove existing contracts; harden only observed gaps** | High / uncertain / medium | Validate discoverability, status/error semantics, bounded logs, session target identity and exported port/env values. Reuse current commands; do not invent a second schema or supervisor. Competitor gaps are unproven, so sell demonstrated reliability, not uniqueness. |
| **3. Retained-stack checks, conditional** | High / uncertain / medium–high hypothesis | Investigate only a demonstrated deficit after existing CLI/harness reuse. Existing `verify` and devenv tasks may suffice for disposable stacks. The [source audit](proc-compose-baseline.md#cited-findings-1) finds no public retained managed-check operation; that does not establish demand. |
| **Defer: MCP transport** | Medium / low / design-dependent, unmeasured | Require a named client or demonstrated usability need. If justified, adapt the same contracts without duplicating lifecycle logic. A competitor already offers MCP; parity does not imply no utility. [P] |

**Maintenance/security costs:** machine contracts require compatibility rules for fields, exit codes, partial failures and stale cursors; logs/error packets need byte limits and secret-handling policy. Session isolation must distinguish process identity from an endpoint answering successfully. Port selection alone cannot isolate files or data. Retained checks need explicit ownership, freshness and concurrency policy: an old healthy stack can test the wrong revision. These are proposed acceptance concerns, not diagnosed defects.

MCP costs depend on transport, authority and contract reuse: stdio need not expose a network listener, while network transports introduce different access-control concerns. Comparative effort was not measured ([reviewed transport evidence](../Adversarial%20review%20of%20agent%20research/alternatives-verification.md#cited-findings-1)). “Local” is not a permission model: stop/restart must target an explicitly owned session. Configs and logs are untrusted inputs; recipes and servers must not grant blanket authority to execute log suggestions.

**Rejected reuse/alternatives:** do not replace Compose resource management, devenv provisioning or Tilt’s update graph. Do not add a generic task language. Defer a new agent transport until a named client or demonstrated usability need justifies it; do not equate MCP with an always-on network server. Prefer the existing harness when it already meets this job's readiness and cleanup needs. [D1]; [T]; [E1]; [E2]

### Gaps

Documentation cannot establish schema stability, cleanup correctness, secret redaction, retained-stack freshness or client compatibility. Those require source-owner confirmation and boundary tests. No measured speed ranking is justified.

## 3. What would falsify the opportunity?

### Takeaway

Use the study's [canonical pilot](../../reports/Agent%20workflow%20opportunities%20and%20validation.md#canonical-pilot), not a separate comparison or multi-project benchmark. New implementation is an escalation, not the default result for the selected job.

### Cited Findings

- Devenv explicitly warns that attaching to a running manager uses its original configuration; edits require manager restart. This is concrete disconfirming evidence against treating “running” as “current.” [E1]
- Devenv task-run processes stop when the task graph completes, whereas `up` retains them. Lifecycle choice can already be expressed without another test runner. [E2]

Source qualifiers: all six retrieved **2026-10-19**; publication dates were not established. [P] is mutable upstream `main`, not release-pinned. [D1], [D2] and [T] are unversioned current references; supported installed versions must be checked. [E1] labels readiness/ports as 2.0, attach as 2.2 and shutdown as 2.3; [E2] labels dependency states 2.0 and `wantedBy` 2.3.2. These labels are not verification of installed or latest releases.

### Inferences

**Pilot specification—not executed:**

1. Follow the [canonical pilot](../../reports/Agent%20workflow%20opportunities%20and%20validation.md#canonical-pilot) for baseline/candidate commands, three real edits, compatibility gates and equal safety exposures. This supersedes the earlier two-project/12-case proposal.
2. Attribute results only to the integration bundle for the selected job, not independently to retention, recipes, transport or lifecycle ownership.
3. Count eligible exposures per contract. Investigate a serious safety failure immediately; do not repeat harm to satisfy a repetition threshold.
4. Record complete wall-clock time separately from cumulative concurrency. This alternatives assignment inspected no raw timing sample; the baseline note records the bounded historical narrative sample, not benchmark proof.
5. No eligible exposure or valid independent oracle means inconclusive. “No feature” concerns the selected job, not all users or future demand.
6. Parallel-worktree, network-denial and noisy-secret probes are deferred unless separately scoped. The canonical pilot has not run and requires separate approval; this summary is not another executable design.

### Gaps

This pilot cannot establish broad adoption, cross-platform reliability or statistical performance gains. It can reject unnecessary scope and identify one reproducible boundary failure. Before implementation, reconcile candidates with the source and agent/harness researchers; do not convert unverified gaps into a roadmap.

[P]: https://raw.githubusercontent.com/F1bonacc1/process-compose/main/README.md
[D1]: https://docs.docker.com/reference/cli/docker/compose/
[D2]: https://docs.docker.com/compose/how-tos/startup-order/
[T]: https://docs.tilt.dev/api.html
[E1]: https://devenv.sh/processes/
[E2]: https://devenv.sh/tasks/
