# Agent workflow opportunities and validation

## 1. What do documented agent development loops actually require?

### Takeaway

The concrete opportunity is reliable service identity, readiness, observation, and ownership across agent/test sessions—not enabling agents to execute commands, which they already can. Git worktree support establishes a parallel-checkout workflow, but does not establish isolated ports or application data. The latter is an inference requiring validation, not a vendor claim. [Codex worktrees](https://developers.openai.com/codex/app/worktrees), [Claude tools](https://code.claude.com/docs/en/tools-reference.md)

### Method and source table

Study/retrieval date: **2026-10-19**. Three initial searches returned no results; direct official-document fetches supplied the evidence. Six primary pages below form the original corpus. `/codex/security` describes a vulnerability-scanning product, not agent permissions, and was excluded. Sources were read, not executed. No services, agent sessions, benchmarks or repository capability audit were performed. Wording and pilot corrections from the [adversarial review](../../reports/Adversarial%20review%20of%20agent%20research.md) are applied below without adding runtime evidence.

| ID | Exact retrieved URL / title | Publication date and qualification | Retrieved |
| --- | --- | --- | --- |
| S1 | https://developers.openai.com/codex/app/worktrees — **Worktrees** | Undated living documentation; desktop-managed Git worktrees, not every Codex surface. | 2026-10-19 |
| S2 | https://developers.openai.com/codex/integrated-terminal.md — **Integrated terminal** | Undated; ChatGPT desktop app terminal. | 2026-10-19 |
| S3 | https://developers.openai.com/codex/agent-approvals-security.md — **Agent approvals & security** | Undated; distinguishes local, cloud, OS, sandbox, and network-policy configurations. | 2026-10-19 |
| S4 | https://code.claude.com/docs/en/tools-reference.md — **Tools reference** | Undated; background limits require v2.1.285+, with changed scope in v2.1.288; TaskOutput removed in v2.1.277. | 2026-10-19 |
| S5 | https://code.claude.com/docs/en/permissions — **Configure permissions** | Undated; saved repository-wide permission behavior changed in v2.1.211. | 2026-10-19 |
| S6 | https://playwright.dev/docs/test-webserver — **Web server** | Undated, current Node.js Playwright Test guide; no installed-version equivalence established. | 2026-10-19 |

### Cited Findings

- **Launch/observe:** Codex's desktop terminal is scoped to the current project/worktree; the assistant can read current terminal output, including a running development server or failed build. Reusable actions run terminal commands. This does not document universal CLI child-process persistence. [S2](https://developers.openai.com/codex/integrated-terminal.md)
- **Lifetime/stop:** Claude Bash commands run in separate processes; exported variables do not persist between commands. `run_in_background` supports servers/watch builds; `/tasks` and `TaskStop` stop tasks. Main-conversation/background-subagent commands can outlive a final response, while foreground-subagent commands stop when that subagent ends. Non-interactive `-p` runs can remain open waiting for main-conversation background commands. Current local interactive sessions have no background time limit; unattended sessions do, with version-qualified defaults. [S4](https://code.claude.com/docs/en/tools-reference.md#background-commands)
- **Bounded observation:** Claude output is truncated above documented limits; background output can be read from a file. Its Monitor tool already supports log watching and polling, with deadlines. An orchestrator should not assume unlimited inline logs or that it must invent an agent event channel. [S4](https://code.claude.com/docs/en/tools-reference.md)
- **Worktrees:** Codex documents independent parallel chats, optional environment setup, and handoff to Local when users can run only one application instance. Ignored files do not ordinarily follow Git handoff; `.worktreeinclude` selectively copies ignored files into local managed worktrees. Claude exposes isolated worktree entry/exit, with approval boundaries for entering some existing paths. [S1](https://developers.openai.com/codex/app/worktrees), [S4](https://code.claude.com/docs/en/tools-reference.md)
- **Permission boundary:** Codex separates OS sandbox capabilities from approval policy; default local command networking is disabled. Its optional network proxy treats local/private destinations separately. Claude explicitly says prompt/CLAUDE.md instructions do not change enforced permissions; command-pattern rules are not an OS security boundary. [S3](https://developers.openai.com/codex/agent-approvals-security.md), [S5](https://code.claude.com/docs/en/permissions)

### Inferences

- Agents need a discoverable service lifecycle across interrupted tool calls, not necessarily a service that survives every session. Persistence and cleanup must be explicit choices.
- Worktree-scoped ports, URLs, writable data, and environment selection matter independently of isolated source files. Copying the same `.env` can plausibly reproduce port/database collisions.
- A pre-existing supervisor outside the agent sandbox could launch commands under different permissions. Treat its control interface as a capability boundary, not a way around approvals.

### Gaps

The selected docs do not specify Codex terminal child lifetime on chat closure, reliable readiness of arbitrary application dependencies, or process cleanup when a managed worktree is removed. File-worktree cleanup must not be presented as proof of service cleanup. No adoption rate or agent-wide failure frequency was established.

## 2. What should remain in the CLI and existing browser harness?

### Takeaway

Playwright already starts, waits for, reuses and logs development servers, and shuts down commands it launched, including multiple servers. It does not adopt reused servers for teardown. Name the accountable owner per resource, allowing verified hierarchical delegation rather than competing independent launchers. [S6](https://playwright.dev/docs/test-webserver)

### Cited Findings

Playwright `webServer` supports `command`, `cwd`, inherited/overridden environment, startup timeout (60 seconds by default), stdout/stderr routing and multiple entries. `reuseExistingServer: true` reuses an available endpoint; false rejects one meeting the configured availability test. In URL mode this is HTTP availability, not every listener or checkout identity. An unavailable endpoint can trigger the launch command. `use.baseURL` supplies browser-relative URLs; these primitives do not automatically allocate per-worktree ports. [S6](https://playwright.dev/docs/test-webserver)

Its URL readiness accepts 2xx, 3xx, 400, 401, 402 or 403. Output-regex waiting (v1.57+) supports named captures exported into the environment; **URL and output waits are OR, not AND** when combined. For launched commands, default shutdown force-kills the process group; configurable SIGTERM/SIGINT precedes SIGKILL after a positive timeout, while zero disables later SIGKILL and Windows ignores this graceful option. This is not proof against escaped descendants. [S6](https://playwright.dev/docs/test-webserver), [reviewed version/scope evidence](../Adversarial%20review%20of%20agent%20research/agent-source-verification.md#cited-findings)

### Inferences: evidence-to-opportunity mapping

The following are **proposed contracts**, not claims about existing proc-compose features. Compare them against the separate source audit before proposing implementation.

| Documented pressure | Candidate machine-facing contract | Existing-CLI/documentation first |
| --- | --- | --- |
| Sessions/worktrees and reused endpoints can coexist. [S1](https://developers.openai.com/codex/app/worktrees), [S6](https://playwright.dev/docs/test-webserver) | Status includes run ID, worktree/cwd, endpoint, ownership, and generation; reject mismatched reuse. | Document explicit project/run selection and how tests receive the same URL. |
| A responding URL need not mean the app's dependencies work. [S6](https://playwright.dev/docs/test-webserver) | Distinguish starting, running, ready, unhealthy, exited; bounded wait checks application readiness. | Document the actual health endpoint/check, timeout, and dependency assumptions; do not use sleep as proof. |
| Tools have time/output limits. [S4](https://code.claude.com/docs/en/tools-reference.md) | Non-interactive operations, machine-only JSON stdout, diagnostics on stderr; bounded logs by service/time/cursor, with truncation declared. | Reuse established JSON/status/log flags if available; teach exact commands rather than parsing a TUI. |
| Test failures and startup failures differ. [S6](https://playwright.dev/docs/test-webserver) | Document exit meanings: success, readiness timeout, invalid configuration, permission denial, ownership conflict; retain underlying service exit/signal. | A documented stable CLI contract can serve shells and agents without another transport. |
| Background processes can outlive responses. [S4](https://code.claude.com/docs/en/tools-reference.md) | Idempotent stop, owner-scoped cleanup, child-process handling, explicit retained-service policy. | Put cleanup responsibility and exception behavior in the workflow example. |

Two integration choices should be compared, not combined accidentally:

1. **Test-owned:** retain Playwright's native `webServer` baseline. If testing a foreground proc-compose supervisor command, Playwright owns that supervisor's lifetime; verify signal forwarding and child cleanup rather than assuming them.
2. **Developer/orchestrator-owned:** explicitly start/reuse the stack, wait and validate identity, then pass its endpoint to Playwright's `baseURL` without another server launcher. The orchestrator/session owner performs cleanup; a browser test finishing should not stop unrelated shared services.

Per-worktree environment variables can parameterize server commands, readiness URL, and baseURL in project configuration; this is an integration pattern inferred from documented options, not a Playwright allocator. [S6](https://playwright.dev/docs/test-webserver)

An AGENTS.md/CLAUDE.md recipe can name verified startup/status/wait/log/test/stop commands, worktree variables, permitted reuse, and who cleans up. Instructions guide behavior, not permissions. **No evidence here requires MCP.** First test the existing CLI plus that recipe; only compare a thin protocol adapter if measured discovery/argument errors remain. An adapter would still need the same ownership/security contract. [S5](https://code.claude.com/docs/en/permissions)

### Gaps

Whether proc-compose already satisfies these contracts is outside this assignment. The cited guide does not establish that endpoint reuse validates checkout identity, nor that broad HTTP readiness implies database/worker readiness. Browser and shell network policies can differ; validate both instead of treating one successful curl as browser access proof. [S3](https://developers.openai.com/codex/agent-approvals-security.md)

## 3. Which end-to-end pilot could falsify the opportunity?

### Takeaway

Test lifecycle correctness before claiming speed or productivity. The study's [canonical pilot](../../reports/Agent%20workflow%20opportunities%20and%20validation.md#canonical-pilot) is the only current specification. This note's earlier six-scenario/two-worktree design is superseded, not a second active experiment. Documented worktrees and harnesses motivate hypotheses, not observed customer demand. [S1](https://developers.openai.com/codex/app/worktrees), [S6](https://playwright.dev/docs/test-webserver)

### Cited Findings

Official workflows support parallel checkouts, background development commands, and harness-managed server testing. They provide no comparable measurement of proc-compose's incremental benefit. [S1](https://developers.openai.com/codex/app/worktrees), [S4](https://code.claude.com/docs/en/tools-reference.md), [S6](https://playwright.dev/docs/test-webserver)

### Inferences: scenario and falsifiable hypotheses

**Specification:** defer to the [canonical pilot](../../reports/Agent%20workflow%20opportunities%20and%20validation.md#canonical-pilot) for the actual baseline, candidate, task, controls, exposures and decision gates; do not combine it with this note's superseded scenario list.

The agent/browser evidence contributes these questions to that single pilot, not separate comparison arms:

- **Identity:** can real application-path observations detect one safe known mismatch? No eligible exposure or valid oracle means inconclusive, not pass.
- **Readiness:** does the existing application probe represent the required dependencies? A listening URL alone is insufficient.
- **Cleanup:** can independent observers detect check-owned children/listeners alive and exited, while retained services and unrelated controls remain untouched? Declare visibility limits and observation windows.
- **Interaction cost:** does the selected integration bundle show an observed advantage? Equal counts can be a floor; component-level or general productivity attribution is not supported.

Three edits are a feasibility sample, not three guaranteed exposures to every contract. Safety aborts override repetition. Parallel-worktree, network-denial and broader fault probes require separate scope. Record complete wall-clock time, not summed concurrent durations; no gains were measured here.

### Gaps and source limitations

No pilot has run. Living docs are not pinned release guarantees; record actual installed versions before testing. The sample covers two vendors and one Node.js harness, not all agents, operating systems, browser integrations, or developer populations. Native Playwright may already be sufficient for a small test-only stack; persistent multi-service work is a hypothesis to test, not a reason by itself to build a protocol or roadmap.
