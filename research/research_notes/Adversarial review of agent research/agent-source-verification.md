# Adversarial verification of agent and browser sources

## 1. Are the exact cited sources accessible, correctly attributed, and version-qualified?

### Takeaway

**Desk verification on 2026-10-19, not runtime proof:** all six original agent/browser URLs returned substantive pages with the stated titles. Their live content supports most of the study's technical claims. No inaccessible original was replaced with Context7 or another source. The principal actionable qualification concerns Playwright's availability-based reuse/refusal, with smaller ownership and execution-environment wording issues below. [S1][S1] [S2][S2] [S3][S3] [S4][S4] [S5][S5] [S6][S6]

### Cited Findings

**Review scope and local citations.** Read the complete `research/reports/Agent workflow opportunities and validation.md` (97 lines), `research/research_notes/Agent workflow opportunities and validation/agent-workflows.md` (102 lines), and `research/README.md` (18 lines). Below, **R** means the first file and **N** the second; line numbers identify the inspected text, not an immutable committed snapshot. Original artifacts were not edited. Repository-code claims, competitor claims, and private historical application records are not independently certified by this external-source review.

**Exact-source access ledger.** Each original URL was fetched with `webfetch`. Separate public, read-only HTTP **HEAD** requests returned **200 with no Location/HTTP redirect** for every URL. HEAD results describe this access, not all clients or earlier accesses. OpenAI pages link to reorganized `learn.chatgpt.com` documentation, but an index banner or cross-link is not an observed redirect. [S1][S1] [S2][S2] [S3][S3]

| ID | Exact original URL / observed title | Current scope and qualification | Provenance verdict |
| --- | --- | --- | --- |
| S1 | https://developers.openai.com/codex/app/worktrees — **Worktrees** | Codex-managed Git worktrees on the connected computer/development environment; desktop-specific instructions, with mobile control also described. `.worktreeinclude` copying is expressly limited to local desktop-managed worktrees, not remote or manually created Git worktrees. | **PASS** for N:15,27 and R:24; not universal execution or application-state isolation. |
| S2 | https://developers.openai.com/codex/integrated-terminal.md — **Integrated terminal** | Actual body says **ChatGPT desktop app**. “Each chat … includes a terminal scoped to its current project or worktree”; ChatGPT can read current output. This is not a Codex CLI execution-tool lifetime specification. | **PASS** with the scope already recorded at N:16,24; retain that qualifier when summarizing R:24. |
| S3 | https://developers.openai.com/codex/agent-approvals-security.md — **Agent approvals & security** | Distinguishes local/cloud execution, sandbox/approval layers, OS implementations, command networking, optional proxy policy, and browser/tool connections outside that proxy. | **PASS** for N:17,28,75 and R:32, subject to actual mode/policy. |
| S4 | https://code.claude.com/docs/en/tools-reference.md — **Tools reference** | Claude **Code**, not every Claude product or agent harness. Explicitly states TaskOutput removal in **v2.1.277**, background-limit introduction in **v2.1.285**, and interactive/unattended scope change in **v2.1.288**. | **PASS** for the stated version thresholds at N:18,25 and R:93; no installed-version proof. |
| S5 | https://code.claude.com/docs/en/permissions — **Configure permissions** | Enforced tool rules are not prompt instructions or an OS boundary around arbitrary subprocess actions. Saved-approval behavior changed in **v2.1.211**, with Windows/non-repository and other storage exceptions. | **PASS** for N:19,28,71 and R:32,93; do not universalize repository-wide storage. |
| S6 | https://playwright.dev/docs/test-webserver — **Web server** | Current **Node.js Playwright Test** guide. HTTP acceptance, URL/output OR semantics, multiple commands, reuse and platform-qualified shutdown are documented. | **PASS** for the core facts at N:20,48–50 and R:26; qualifications in §2. |

**Dates and versions.** Review/access date is **2026-10-19 per the harness**. No publication date for the six original pages was established. HEAD supplied Last-Modified headers for S1 (`2026-10-18`), S4 (`2026-10-18`), and S6 (`2026-10-12`); these are server metadata, **not publication dates, release dates, or evidence of the researcher's earlier retrieval**. The other three HEAD responses supplied no Last-Modified value. The study's undated/living-reference warning at R:93 and N:100–102 is appropriate. [S1][S1] [S4][S4] [S6][S6]

**Context7 consultation.** Ran `resolve-library-id` for Playwright, OpenAI Codex, and Claude Code, then five public, generic `query-docs` queries using resolved IDs `/websites/playwright_dev`, `/openai/codex`, `/llmstxt/learn_chatgpt_llms-full_txt`, and `/websites/code_claude`. All resolved/query calls succeeded. These were supplemental indexes, not independent proof of an original page or an installed release. In particular, the CLI-oriented Codex index was not substituted for the desktop terminal/worktree pages. Index results are not an exhaustive capability/demand survey.

**Only three additional primary clarifications were fetched:**

| ID | URL / precise scope | Why needed |
| --- | --- | --- |
| P1 | https://raw.githubusercontent.com/microsoft/playwright/main/packages/playwright/src/plugins/webServerPlugin.ts — current upstream plugin source | Original guide is imprecise about “listening”; inspect `_startProcess`, `_waitForProcess`, and `teardown` to distinguish URL availability, launched versus reused processes, and shutdown ownership. Mutable `main`, not installed-version evidence. |
| P2 | https://playwright.dev/docs/api/class-testconfig#test-config-web-server — **TestConfig**, `webServer` | Confirm full option context and nested shutdown/wait semantics; do not mistake whole-option “Added in v1.10” for every later subfeature. |
| P3 | https://playwright.dev/docs/release-notes#version-157 — **Release notes**, Version **1.57 → Waiting for webserver output** | Primary confirmation that `webServer.wait` and named-capture support were introduced in **v1.57**; the original guide does not give this threshold. No release calendar date inferred. |

### Inferences

- The correct provenance claim is “these live sources currently support these statements,” not “this review reproduced the earlier research fetch/snapshot.” Same-day access is still not independent authentication of that prior event. The original URLs are sufficient for current-content verification but not historical reconstruction. [S1][S1] [S2][S2] [S3][S3] [S4][S4] [S5][S5] [S6][S6]
- Calling S2 “Codex's integrated terminal” is acceptable family/navigation shorthand only if its actual **ChatGPT desktop app** scope remains visible. It supplies no evidence that Codex CLI, OpenCode, a browser tool, or an agent's Bash executor shares that terminal's state or process lifetime. [S2][S2]

### Gaps

- **Unavailable evidence, not unavailable pages:** no original archived responses, original search transcripts, original fetch receipts/content hashes, or verified installation versions were supplied. N:11's “three initial searches returned no results” and historical retrieval assertions remain **UNVERIFIED** as events. The excluded `/codex/security` route was not fetched in this bounded six-source audit; its reported behavior is not independently re-certified.
- No original library/page was unavailable. No sign-in, browser automation, service startup, network-policy change, agent execution experiment, or cleanup test was performed.

## 2. Which claims fail or need qualification, and which survive adversarial checking?

### Takeaway

There is **one medium-severity precision/safety finding**, plus three low-severity wording/scope qualifications. No high-severity false-source finding was established. The HTTP status list, readiness OR semantics, Claude background-lifetime distinctions, and cited Claude version qualifiers survive checking. A single ultimate accountable lifecycle owner is a defensible design rule; a ban on safe ownership hierarchy is not established by these sources. [S4][S4] [S6][S6] [P1][P1]

### Cited Findings

#### F1 — MEDIUM: “already listening” is stronger than Playwright's URL-based refusal/reuse guard

- **Exact locations:** `research/research_notes/Agent workflow opportunities and validation/agent-workflows.md:48`; relevant summary at `research/reports/Agent workflow opportunities and validation.md:26`.
- **Claim versus evidence:** N:48 says `reuseExistingServer: false` “rejects an already-listening server.” In URL mode, current upstream `_startProcess` first evaluates `_isAvailableCallback`; only an **available** result enters the reuse-or-throw branch. Otherwise it launches `command`. `getIsAvailableFunction` distinguishes HTTP `isURLAvailable` from deprecated port-only TCP checking. The original guide accepts **2xx, 3xx, 400, 401, 402, 403**, not arbitrary HTTP responses. Thus a TCP listener serving, for example, 500 at the configured URL does not supply the stated general refusal guarantee. This is static boundary reasoning, not an executed reproduction. [S6][S6] — option rows `url`, `port`, `reuseExistingServer`; [P1][P1] — `_startProcess`, `getIsAvailableFunction`.
- **Supporting passage:** P1: `const isAlreadyAvailable = await this._isAvailableCallback?.(); if (isAlreadyAvailable) { ... if (this._options.reuseExistingServer) return; ... throw new Error(...) }`, followed by launching the configured command when that branch is not taken. [P1][P1]
- **Corrected wording:** “Playwright reuses or refuses a server that passes its configured availability test. In URL mode this is the documented HTTP test, not a blanket check for any listener. If unavailable, it attempts the configured launch command even with reuse enabled; it does not validate checkout/run identity.” [S6][S6] [P1][P1]
- **Materiality:** a safety recipe must not treat this flag as general port ownership, run identity, or guaranteed no-launch protection. A not-yet-ready or wrong-service listener can still lead to an attempted competing launch. No actual wrong-run incident or destructive behavior is demonstrated.
- **Confidence:** **high for inspected current semantics; unverified for any installation**.
- **Best counterargument:** the study closely follows the primary guide's own loose “listening on the port or url” wording, and R:26 already warns against equating a URL with checkout identity. If “listening” is understood as “passes this configured availability check,” the sentence is shorthand rather than a factual conflict. Nevertheless it should be narrowed before becoming an operational safety rule. [S6][S6]

#### F2 — LOW: lifecycle capability is not ownership transfer; hierarchy is not competing peer ownership

- **Exact locations:** `research/reports/Agent workflow opportunities and validation.md:26,57`; `research/research_notes/Agent workflow opportunities and validation/agent-workflows.md:44,50,64–67`.
- **Claim versus evidence:** “High confidence Playwright already owns lifecycle” (R:57) and the synthesis's either/or framing need “can own the processes it launches.” In current P1, reuse returns **before** `launchProcess` assigns `_killProcess`; `teardown` only calls `this._killProcess?.()`. Reuse therefore does **not** adopt an existing server for teardown. Default process-group shutdown and `gracefulShutdown` describe a launched command's group, not every reused service or all escaped descendants. Also, `gracefulShutdown.timeout: 0` deliberately sends no later SIGKILL; this is not a universally bounded cleanup guarantee. [S6][S6] — `reuseExistingServer`, `gracefulShutdown`; [P1][P1] — `_startProcess`, `teardown`; [P2][P2] — `webServer` type.
- **Supporting passage:** P1 returns on `this._options.reuseExistingServer` before `this._killProcess = gracefullyClose`; teardown is `await this._killProcess?.()`. S6 says “A `0` timeout means no `SIGKILL` will be sent” and the graceful option is ignored on Windows. [P1][P1] [S6][S6]
- **Corrected wording:** “Name one ultimate accountable owner **per resource**, with explicit delegation and reuse. Playwright may own a foreground proc-compose parent, and proc-compose its service children, if signal/cleanup contracts are verified; alternatively it can test an externally retained stack without taking teardown ownership. Avoid independent competing launchers/mutators of the same resources.” [S6][S6] [P1][P1]
- **Materiality:** prevents excluding a viable test-owned hierarchy or mistakenly expecting Playwright to stop a reused retained stack. This is a qualification, **not proof that the study forbids hierarchy**: N:66 explicitly allows Playwright → foreground proc-compose → children.
- **Confidence:** **high** for launched/reused distinction; **medium** that readers would misread the synthesis.
- **Best counterargument:** R:26 can already be read as “one ultimate owner”; its Playwright-owned branch can include the hierarchy expressly permitted by N:66. “Already owns lifecycle” in an options table can mean an available capability, not a statement that every current test invocation owns every server. The report's intended safety objective remains sound.

#### F3 — LOW: shell export non-persistence is a Claude Code Bash contract, not a universal terminal fact

- **Exact locations:** `research/reports/Agent workflow opportunities and validation.md:24`; contrast `research/research_notes/Agent workflow opportunities and validation/agent-workflows.md:16,24–25`.
- **Claim versus evidence:** R:24's standalone “Exported shell variables do not persist between commands” drops the executor qualifier present in N:25. S4 says “The Bash tool runs each command in a separate process” and “An `export` in one command won't be available in the next.” The same heading documents inherited startup state and mechanisms to provide environment across calls: `CLAUDE_ENV_FILE` or a SessionStart hook. S2 describes a distinct integrated terminal and gives no corresponding per-command fresh-process contract. [S4][S4] — **Bash tool behavior → What persists between commands**; [S2][S2] — **Run and validate your project**.
- **Corrected wording:** “In Claude Code's Bash tool, an export inside one call does not carry to the next; supply environment explicitly or through its documented persistent-environment mechanism. Do not infer the same behavior for another agent executor or an interactive terminal.” [S4][S4]
- **Materiality:** low for the no-feature recommendation, potentially important when turning the study into an endpoint-propagation recipe. It should not imply that environment propagation is impossible or that a supervisor is required to solve it.
- **Confidence:** **high** on the documented scope; **medium** on ambiguity in context.
- **Best counterargument:** the sentence immediately follows the Claude tools discussion, so a careful reader can carry that antecedent forward; the underlying notes already scope it correctly.

#### F4 — LOW: “not an approval bypass” should remain a requirement, not a technical guarantee

- **Exact locations:** `research/reports/Agent workflow opportunities and validation.md:32`; explanatory inference at `research/research_notes/Agent workflow opportunities and validation/agent-workflows.md:34,71,75`.
- **Claim versus evidence:** R:32 ends “A supervisor outside the sandbox is not an approval bypass.” As a prohibition/design requirement this is sensible. It is **not** a proven property of a daemon/control interface: S3 limits the command proxy's scope and explicitly excludes MCP/browser/app connections from that proxy; S5 says a command-pattern rule “isn't a security boundary around the program.” These sources cannot prove that a client sandbox constrains commands launched by an external privileged supervisor. N:34 correctly recognizes this as a separate capability boundary. [S3][S3] — **Traffic outside the command network proxy**, **Sandbox and approvals**; [S5][S5] — **What a Bash rule doesn't match**.
- **Corrected wording:** “An external supervisor **must not be used as** an approval bypass. Separately authorize access to its control interface and the effects it can cause; client-side sandbox or command rules alone do not prove confinement of that external service.” [S3][S3] [S5][S5]
- **Materiality:** removes an ambiguous security reassurance; **no bypass or vulnerability in proc-compose was established** by this review.
- **Confidence:** **high** that the sources do not establish the stronger guarantee; **medium** that the original intended anything beyond a normative prohibition.
- **Best counterargument:** the surrounding text demands real shell/browser access validation and notes that written instructions do not change permissions. Reading the last sentence normatively is natural and consistent with N:34; on that reading it is not a factual error.

#### Compact claim/source coverage ledger

**PASS** = supported within the stated product/current-document scope, not runtime certification. **PARTIAL** = needs the indicated boundary or is an inference. **FAIL** = literal stronger wording conflicts with inspected semantics. **UNVERIFIED** = absent historical/runtime/empirical evidence, not a claim of falsity. Rows group repeated statements; R/N abbreviations expand to the exact paths above.

| Research claim/location | Source and small passage or precise heading | Verdict |
| --- | --- | --- |
| N:15,24; R:24 — integrated terminal is project/worktree scoped | [S2][S2], opening: “Each chat in the ChatGPT desktop app includes a terminal scoped to its current project or worktree.” | **PASS**, desktop terminal only. |
| N:24; R:24 — assistant reads current server/build output; actions run commands | [S2][S2], **Run and validate your project**, **Create reusable actions**. | **PASS**; output visibility is not arbitrary command or browser reachability. |
| N:24,38 — no universal Codex CLI child-persistence/worktree-cleanup guarantee | [S2][S2] has no child-lifetime rule; [S1][S1], **Worktree cleanup**, describes worktree deletion/snapshots, not process shutdown. | **PASS** as an explicitly recorded evidence gap. |
| N:7,27; R:24 — parallel chats, local environment setup, handoff when only one app instance is practical | [S1][S1], **Getting started**, **Working between Local and Worktree**: “can run only one instance of your app.” | **PASS**; no isolated service/data guarantee. |
| N:27,33; R:32 — ignored files and `.worktreeinclude`; copying `.env` can preserve shared targets | [S1][S1], **Copy ignored local files into managed worktrees** explicitly lists `.env` and limits copying to local managed worktrees. | **PASS** for copying/scope; **PARTIAL/inference** for resulting collisions, correctly marked plausible rather than measured. |
| N:27 — Claude worktree entry/exit and approval boundaries | [S4][S4], `EnterWorktree`/`ExitWorktree` table: existing paths outside `.claude/worktrees/` prompt; versions **v2.1.203/v2.1.206** qualify older behavior. | **PASS** as a capability, not all worktree kinds or subagent contexts. |
| N:25; R:24 — separate Bash processes and export non-persistence | [S4][S4], **Bash tool behavior → What persists between commands**; also documents `CLAUDE_ENV_FILE`/hooks. | **PASS** in N; **PARTIAL** unqualified R sentence, F3. |
| N:25; R:24 — background dev/watch commands; `/tasks`, TaskStop | [S4][S4], **Background commands** and `TaskStop` table entry. | **PASS**; not a supervisor service guarantee. |
| N:25,62 — main/background-subagent commands can outlive final response; foreground-subagent commands stop at its end | [S4][S4], **When a background command stops**; background-subagent command also stops when the overall noninteractive run exits. | **PASS**; final response, subagent end, and overall run exit are distinct events. |
| N:18,25; R:93 — noninteractive `-p` wait and background-limit versions | [S4][S4], **Time limit for background commands**: v2.1.285+, before v2.1.288 every session; current interactive local no background time limit. Default unattended 30 min / max 2 h; text-prompt `-p` default 10 min; documented variables can raise limits. | **PASS**, mode/version-qualified. No installed release verified. |
| N:18; R:93 — TaskOutput removed in v2.1.277 | [S4][S4], tool-table `TaskOutput`: “Removed in v2.1.277”; use output file with Read. | **PASS**. |
| N:26,60 — bounded output and output-file observation | [S4][S4], **Timeout and output limits → Output limits**. Valid-result overflow yields file path/preview; failure excerpt can lack a file path; command output above 5 GB is killed. | **PASS** for boundedness, not a guarantee all truncated failure output is recoverable. |
| N:26 — Monitor already watches/polls, with deadlines | [S4][S4], **Monitor tool**: default 5 min, max 30 min, max 10 min in single-prompt noninteractive `-p`; unavailable with specified providers/traffic flags; Windows requires Git Bash. | **PASS** as conditional capability, not universal availability. No evidence an agent event channel is necessary. |
| N:17,28; R:32 — sandbox capability versus approval policy, default command network off, optional local/private proxy controls | [S3][S3], **Sandbox and approvals**, **Network access**, **Local and private destinations**. Proxy is opt-in and constrains already-enabled networking; exact local exceptions exist. | **PASS** for documented default/modes; not “every tool has no network.” |
| N:28,71; R:32 — prompt/CLAUDE.md doesn't change enforced permissions; patterns aren't OS security | [S5][S5], **Manage permissions** note; **What a Bash rule doesn't match**. | **PASS**. |
| N:19; R:93 — saved approval behavior changed in v2.1.211 | [S5][S5], **Permission system**: repository-root/main-checkout storage, with Windows/non-Git and other exceptions. | **PASS** for the version change; avoid “all OSes/all approvals are shared.” |
| N:34,75; R:32 — shell and browser access can differ; external supervisor isn't automatically confined by client sandbox | [S3][S3], **Traffic outside the command network proxy** explicitly excludes browser/Computer Use and describes separate browser origin controls; [S5][S5], Bash rule limits. | **PASS** for “can differ” and need to validate both; **PARTIAL** for categorical bypass phrasing, F4. No actual access proven. |
| N:48; R:26 — command/cwd/env/start timeout/output routing/multiple entries | [S6][S6], option table and **Multiple web servers**: env inherits `process.env` plus `PLAYWRIGHT_TEST=1`, timeout default 60000 ms. | **PASS**, Node.js Test runner; not automatic port allocation. |
| N:48,67,69; R:28,73 — baseURL and configuration environment can target external endpoints | [S6][S6], **Adding a baseURL** and env/url options. | **PASS** for primitives; **UNVERIFIED** for the particular application harness's compatibility. |
| N:50; R:26 — HTTP readiness accepts 2xx, 3xx, 400–403 and URL/output waits are OR | [S6][S6], `url` and `wait`: “at least one of the conditions”; [P1][P1], `_waitForProcess` races conditions. | **PASS**. This is availability, not application identity, authentication success, or all-dependency health. |
| N:50 — output regex/named captures become environment values | [S6][S6], `wait`; [P3][P3], **Version 1.57 → Waiting for webserver output**. P1 also resolves either matching stdout/stderr wait; neither forms an implicit AND. | **PASS** in current docs; **PARTIAL** portability without explicit **v1.57+** qualification. N:102/R:73 already demand installed-version checks. |
| N:48 — false rejects any already-listening server | [S6][S6], HTTP acceptance versus port option; [P1][P1], availability branch in `_startProcess`. | **FAIL** under the literal any-listener reading; **PARTIAL** if “listening” means configured availability. F1. |
| N:44,50; R:26,57 — Playwright owns/shuts down servers | [S6][S6], `gracefulShutdown`; [P1][P1], reused path has no `_killProcess`. Timeout 0 disables later SIGKILL; Windows graceful option ignored. | **PASS** as launched-command capability; **PARTIAL** as unconditional ownership/cleanup. F2. |
| N:58,75; R:26 — listening URL and reuse don't verify checkout identity | [S6][S6] availability options contain no checkout/run check; [P1][P1] only availability controls reuse. | **PASS** as limitation of this mechanism, not proof of failure in a separately identity-checking harness. |
| N:44,64–67; R:26 — one lifecycle owner versus hierarchy | [S6][S6], command spawns a process group; [P1][P1] launch/teardown ownership. N:66 explicitly permits foreground-supervisor nesting. | **PARTIAL** as an absolute ban on more than one managing layer; **PASS** as one accountable owner per resource. F2. |
| N:32–35,54–71; R:3,47,51,71–83 — discoverability, lifecycle, CLI/recipe-first and retained-workflow benefits | [S1][S1], [S4][S4], [S6][S6] establish mechanisms, not proc-compose incremental outcomes. | **PARTIAL/inference**, substantially labeled correctly; superiority, demand and speed **UNVERIFIED**. |
| N:71; R:61 — no evidence here requires MCP | The bounded [S1][S1]–[S6][S6] corpus does not establish a named client's need for a proc-compose MCP adapter. | **PASS** as “not established here,” **not** proof no client could require it. |
| N:85,98,102; R:5,51,75–81 — no comparable incremental-benefit measurements/pilot yet | Sources describe product mechanisms, not a paired proc-compose study. | **PASS** as the research's evidence limitation; no productivity/adoption estimate verified. |
| N:11,13–20; R:93 — original fetch events, earlier dates, exact historical content | Current live pages cannot authenticate those prior events. | **UNVERIFIED** historically; current URL/title/content access **PASS** in §1. |
| R:69 — historical agent/application task, six-server gate, later green checks and timings | Private historical record is outside the six public-source corpus. | **UNVERIFIED by this review**, not contradicted; no runtime or private-session audit claimed. |

### Inferences

- Playwright readiness is not intrinsically “weak”: an application can make the configured URL or output marker a strong readiness contract and independently check identity in fixtures/assertions. Conversely, OR-combining a strong HTTP probe with a merely early log line can admit the early signal. “Need application-relevant probes” is sound; “need proc-compose to achieve readiness” is not established. [S6][S6] [P1][P1]
- Hierarchical lifecycle management can be coherent: the outer harness owns its foreground supervisor, the inner supervisor owns its services, and already-running dependencies can stay externally owned. That topology is not itself competing peer ownership. Correct signal forwarding and descendant cleanup remain runtime obligations, not conclusions from the generic command option. [S6][S6] [P1][P1]

### Gaps

- No demonstrated 500-listener collision, leaked descendant, wrong-checkout reuse, permission bypass, browser denial, or export-propagation failure was run here. P1 is mutable upstream source; it cannot certify the historical or installed Playwright implementation.
- No claim of statistical frequency, universal agent need, universal OS behavior, or measured CLI/recipe superiority can be inferred from this small source set. The study generally already acknowledges these limits at N:102 and R:5,51,93.

## 3. Does the external evidence justify the proposed product decision?

### Takeaway

**Retain the study's cautious no-feature-first conclusion, with the qualifications above.** The sources establish existing agent/harness mechanisms and useful failure boundaries, not measured demand for another orchestrator, transport, persistent service, or automation recipe. CLI/recipe-first is a reasonable low-investment hypothesis to test, not an empirically proven best workflow. [S1][S1] [S4][S4] [S6][S6]

### Cited Findings

- **Counterevidence to an orchestrator-first narrative:** Playwright explicitly presents `webServer` as suitable for development testing, supports multiple processes and configurable probes, and recommends existing-server reuse for local development. A health URL, startup command and fixture identity check may already be enough; the source does not establish that a separate lifecycle product is necessary. [S6][S6] — **Introduction**, **Configuring a web server**, **Multiple web servers**.
- **Counterevidence to persistence as a universal need:** Codex explicitly offers handoff to Local when only one application instance is practical; Claude deliberately stops some subagent-launched commands and bounds unattended background commands. A disposable test-owned stack or one developer-owned instance can be the intended workflow rather than a missing capability. [S1][S1] — **Working between Local and Worktree**; [S4][S4] — **When a background command stops**, **Time limit for background commands**.
- **Counterevidence to inventing observation/environment infrastructure by default:** Claude already documents background output files, Monitor, inherited environment, `CLAUDE_ENV_FILE` and SessionStart hooks. Their availability is product/mode/provider-specific; they are evidence for reuse where applicable, not an assurance these features exist in OpenCode or another agent. [S4][S4] — **Bash tool behavior**, **Monitor tool**.
- **Counterevidence to permission instructions as an implementation:** written guidance does not grant enforced access, and command controls do not universally cover tool/browser connections or external process managers. A recipe can reduce ambiguity but cannot repair a denied origin, missing capability, or unsafe control endpoint merely by naming it. [S3][S3] — **Traffic outside the command network proxy**; [S5][S5] — **Manage permissions**, **What a Bash rule doesn't match**.

### Inferences

- The minimum corrected synthesis is: **use existing facilities; identify the actual executor, version, environment and accountable resource owner; allow verified hierarchy; validate endpoint identity/readiness separately from availability; distinguish reused from launched resources; verify actual browser and shell access; measure before preferring retention or recipes.** This is a design inference from the sources, not a vendor requirement for every agent/application. [S1][S1] [S2][S2] [S3][S3] [S4][S4] [S5][S5] [S6][S6]
- The separately approved pilot at R:71–83 appropriately makes external-endpoint compatibility and fixture safety preconditions, labels lifecycle/retention/guidance differences, accepts no-feature outcomes, and denies causal productivity claims from three pairs. This review does not authorize execution or establish compatibility. Treat its recipe preference as falsified if the existing test-owned harness is equally clear/safe, or if fixture resets/cold startup are acceptance requirements. [S6][S6]
- Before promoting the notes into operational instructions, make F1's availability/refusal boundary explicit and preserve F2's ownership delegation, F3's Claude Bash scope, and F4's normative security requirement. Add **Playwright v1.57+ only if a recipe actually uses `webServer.wait`**; do not impose an upgrade requirement on configurations using only older supported mechanisms. [S4][S4] [S6][S6] [P3][P3]

### Gaps

- **Exact external coverage:** **6/6 original pages** fetched; **6/6 HEAD checks** returned 200/no redirect in this access; **3/3 narrowly related Playwright primary clarifications** fetched; **3 Context7 resolutions and 5 queries** succeeded. No web market search, new competitor survey, inaccessible-source substitution, or runtime experiment occurred.
- **Unavailable evidence:** historical page snapshots/retrieval/search receipts; actual installed Codex/Claude/Playwright/OpenCode versions and policy; browser/shell network reachability; application fixture/reset/external-endpoint support; measured iteration latency, interaction cost, adoption, or demand; real signal/descendant-cleanup behavior. Existing self-disclosures are not independent proof of these events or outcomes.
- The audit identifies source-supported corrections/qualifications only. Original report, notes, README, source, configuration, tests, and application processes were left unchanged.

[S1]: https://developers.openai.com/codex/app/worktrees
[S2]: https://developers.openai.com/codex/integrated-terminal.md
[S3]: https://developers.openai.com/codex/agent-approvals-security.md
[S4]: https://code.claude.com/docs/en/tools-reference.md
[S5]: https://code.claude.com/docs/en/permissions
[S6]: https://playwright.dev/docs/test-webserver
[P1]: https://raw.githubusercontent.com/microsoft/playwright/main/packages/playwright/src/plugins/webServerPlugin.ts
[P2]: https://playwright.dev/docs/api/class-testconfig#test-config-web-server
[P3]: https://playwright.dev/docs/release-notes#version-157
