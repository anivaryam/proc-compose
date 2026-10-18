# proc-compose research

Research on local development with LLM coding agents: existing capabilities, real workflow constraints, alternatives, risks and falsifiable validation. This folder contains research Markdown only; it does **not** authorize implementation, configuration changes or runtime experiments.

## Agent workflow opportunities and validation

Study date: **2026-10-19**. Inspected checkout: **main@7a9e93d3361278e72d72672f6253773c96e73ad7** (`v1.10.0`).

- [Research report](reports/Agent%20workflow%20opportunities%20and%20validation.md) — recommendation, capability baseline, alternatives and one proposed matched pilot.
- [Research notes](research_notes/Agent%20workflow%20opportunities%20and%20validation/) — [proc-compose baseline](research_notes/Agent%20workflow%20opportunities%20and%20validation/proc-compose-baseline.md), [agent/browser workflows](research_notes/Agent%20workflow%20opportunities%20and%20validation/agent-workflows.md), and [alternatives/opportunities](research_notes/Agent%20workflow%20opportunities%20and%20validation/alternatives-and-opportunities.md).
- [Adversarial review](reports/Adversarial%20review%20of%20agent%20research.md) — historical findings against the pre-correction study, with [independent review notes](research_notes/Adversarial%20review%20of%20agent%20research/).

**Applied documentation slice:** CLI runtime/log descriptions and JSON parser caveats are corrected, the existing [agent/browser workflow guide](../docs/sessions-and-logs.md#agent-and-browser-test-workflow) is extended, and review corrections are incorporated into the study/notes. No code, configuration or runtime behavior changed; that documentation-only stage ran no pilot.

**Recommendation:** CLI/documentation/integration first is a conservative hypothesis, not a proven best opportunity. Use only the study's [canonical pilot](reports/Agent%20workflow%20opportunities%20and%20validation.md#canonical-pilot), requiring separate approval. Supporting notes defer to it. Retained checks and MCP remain conditional; no-feature outcomes apply to the selected job, not all future users.

## Municipal system compatibility gate

[Preflight results](reports/Municipal%20system%20compatibility%20gate.md), 2026-10-19: inspected `municipality-central-system@f3492dc`; **9/9 focused runner/configuration tests passed**. An external proc-compose mode already exists, but its official invocation stops, resets/seeds, starts, checks and stops the topology. The retained-stack comparison is therefore blocked for that gate without changing its contract. No browser run, reset, application edit or timing comparison was performed; the three-edit task remains unselected.

## Municipal card workflow baseline

[Executed baseline](reports/Municipal%20card%20workflow%20baseline.md), 2026-10-19: after separate authorization, the existing guarded Chromium card journey passed **1/1** on the same application revision. Full invocation **89.58s**; Playwright reported **39.8s**; shared-package preparation **7.14s**, separate. The runner reset only the local test database and cleaned up its topology; sessions, ports, recorded PIDs/groups and DB connections were checked afterward. No application code/configuration changed. This was one baseline, not three edit iterations, a retained comparison or a speedup measurement; PDF download bytes and fault exposures remain unverified.

## Municipal OSM fix workflow observation

[Observed development loop](reports/Municipal%20OSM%20fix%20workflow%20observation.md), 2026-10-19: a real tile-referrer fix was committed as municipal **7b2f637a** and published in [PR #42](https://github.com/Fake-Taxi-Org/municipality-central-system/pull/42), without merging. **8/8 Node tests, 3/3 Chromium journeys and Core/BIMS types/builds passed**; two baseline lint errors remain disclosed separately. Three official invocations measured **27.45s** (fixture setup failure), **44.60s** (two intended RED failures) and **47.49s** (three GREEN journeys). Existing Playwright-owned reset/start/check/stop behavior was retained; no proc-compose service ownership, retained comparison or speedup was tested. Provider authentication, delegated-test correction and fixture environment were observed friction, not a demonstrated supervisor defect. GitHub CI could not start because of account billing limits; **10 jobs were marked failed, 2 skipped** across push/PR runs. The report records limits and a small optional preflight hypothesis, not authorization for another experiment or feature.

## Earlier background and future studies

The [earlier report](reports/Local%20development%20agent%20opportunities.md) and [earlier notes](research_notes/Local%20development%20agent%20opportunities/) were relocated into this folder for publication, preserving their content apart from relative-link repairs and explicit provenance labels. They are historical background, not fresh runtime verification; their earlier pilot proposals defer to the [canonical pilot](reports/Agent%20workflow%20opportunities%20and%20validation.md#canonical-pilot). Original line labels in historical citations identify review-time text, not necessarily current line positions.

**Local-only evidence:** `file://` citations identify private local records or runtime locations, not publicly retrievable sources. Their referenced files, raw logs and runtime artifacts are not included in this publication. Such citations preserve attribution, not independent public verification. Additional local context is recorded in the [agent-local-development case history](file:///mnt/c/Users/U%20S%20E%20R%20-PC/Documents/workflow-runs/proc-compose-agent-local-dev-research/WORKFLOW-STATE.md) and [earlier improvement case history](file:///mnt/c/Users/U%20S%20E%20R%20-PC/Documents/workflow-runs/proc-compose-improvement-discovery/WORKFLOW-STATE.md); unresolved supervisor-lifetime proposals are not implementation approval, and the earlier cross-session work is already in the inspected repository baseline.

Extend research with a new named or dated study under `research/research_notes/` and a report under `research/reports/`; link it here. Keep research deliverables inside `research/`. Preserve earlier studies and review records; transparently mark approved corrections rather than silently rewriting their history. Record checkout/version, retrieval versus document dates, evidence versus inference, missing measurements and approval gates for experiments.
