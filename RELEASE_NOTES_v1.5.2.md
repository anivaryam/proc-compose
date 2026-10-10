# v1.5.2 — Named sessions and agent-first runtime evidence

This patch release includes new CLI/configuration features alongside lifecycle
fixes. Existing default session identities and configurations remain supported.

## Coding-agent entry point

- Run `proc-compose agent-guide` for the offline, version-matched operating
  guide. The exact Markdown is embedded in the binary and published in the repo.
- Root help and log/failure command help point agents to bounded queries,
  readiness checks, cursor handling, ownership, and verified cleanup.

## Named sessions

- `--session NAME` selects an independent daemon identity, journal, and runtime
  directory. Named port declarations resolve consistent port/URL references.
- Declared resource labels use exclusive OS advisory leases. Concurrent owners
  are rejected before configured application commands run.
- `up --silent --wait-ready --json` exposes resolved session metadata.
- `restart --wait-ready` verifies the replacement process generation.

## Agent-first logs

- One versioned event model captures stdout/stderr, lifecycle, state, readiness,
  and cleanup outcomes with run identity and process generation.
- `logs --json` returns bounded pages with resumable cursors, explicit retention
  gaps, truncation metadata, and process/time/severity/stream/code filters.
- `logs --follow` serves humans; `--follow --json` emits newline-delimited pages.
- `why PROCESS --json` produces a bounded failure evidence packet, including
  dependencies, last errors, exit status, and retained post-stop context.
- Journals rotate independently of custom human log files. Readers pin file
  identities during rotation; oversized lines do not discard subsequent output.

## Reliability fixes

- Failed `restart: never` services can recover while their daemon remains live.
- Reloaded processes use their new readiness probe and timeout. Unsupported
  task/service or dependency transitions are rejected before runtime mutation.
- Environment changes distinguish absent keys from empty-valued keys.
- Daemon JSON output no longer contains duplicate plain-text log entries.
- Selected-process dry-runs use the actual transitive dependency selection.
- `/ready` reports readiness independently of the existing `/health` endpoint.

## Upgrade notes

- Start new sessions with the upgraded binary to obtain the structured journal.
  Older sessions with only text logs report their legacy log path when queried;
  structured records and cursors cannot be reconstructed from arbitrary text.
- IPC keeps the old `line` field for existing monitors; new journals and query
  pages use `message`. Upgraded clients also read older daemon log events.
- Normal `logs --json` returns one page. Follow and `-n 0 --json` return multiple
  newline-delimited pages. Log queries default to a bounded tail.
- Keep config path and session name consistent across commands. Declared port
  leases do not automatically isolate shared databases or build directories.
- Startup readiness timeout still leaves the daemon running. Inspect evidence
  and stop task-owned sessions explicitly; the agent guide documents this path.

## Verification

The release is gated on the complete Go race-test suite, repository lint, native
Linux/macOS/Windows CI, and successful packaging for all five published targets.
The local two-session demo verified independent endpoint wiring and shutdown,
bounded failure evidence, cursor resumption, and post-stop diagnosis.
