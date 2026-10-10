# Branch-safe sessions and agent-first logs

## Acceptance

- Two named sessions can run the same configured stack with independently
  resolved named ports, endpoint references, and runtime directories.
- Declared shared resources are exclusively leased for a session's lifetime.
  A conflicting session fails before any configured command starts.
- Restart/reload use a fresh readiness cycle and accurately report unsupported
  changes. Failed services in a live stack can be recovered explicitly.
- Human output, persisted events, IPC, and agent queries use one event model.
  Events identify the run, session, process generation, time, stream, severity,
  and stable lifecycle/probe code.
- Log queries are filtered and bounded, return a resumable cursor, and disclose
  truncation or a cursor falling outside retained history. Follow is cancellable.
- Failure explanations report observed state, dependency blockers, exit status,
  and probe errors. Stopping one session preserves the other and verifies the
  managed process trees are gone.

## Implementation seams

Extend the config parser, existing dependency resolver, IPC `LogEntry` and
`ProcState`, rotating writer, readiness waiter, and verified shutdown path.
Keep default config-derived daemon identities compatible; named sessions add an
identity component. Session resource locks use OS advisory locks and release on
process exit. Application-specific database provisioning remains an explicitly
configured task; the runner leases declared resource names.

The persistent event journal is separate from human/boot output. Its JSON-lines
schema has a version and uses `message` for content. The reader also accepts the
historical IPC `line` field. Cursors identify a run and sequence, rather than a
byte offset that becomes invalid on rotation. Raw application text is data;
only explicit application JSON severity is normalized. Runtime codes and
readiness/exit metadata are emitted by the supervisor.

## Verification

Use existing cross-platform process fixtures for failed-service recovery,
reloaded log probes/timeouts, unsupported mode changes, and environment changes.
Exercise event queries at rotation, pagination, stream, Unicode, and size-limit
boundaries. Exercise two live sessions, resource contention, endpoint wiring,
failure explanation, and independent cleanup. Run affected Go tests with the
race detector, vet/build, formatting, and cross-platform compilation. Use
temporary fixtures rather than starting or changing the municipal application.

## Verified outcome

- The full Go test suite passed with `-race -count=1` on Linux using Go 1.25.14.
- `go vet ./...` passed. Windows amd64 and macOS arm64 builds passed;
  those builds are compilation proof, not native runtime verification.
- Repository-configured lint passed with golangci-lint 1.64.8, and
  `git diff --check` passed. Changed Go files are formatted. A repository-wide
  format scan also reports three untouched baseline files:
  `internal/runner/metrics.go`, `internal/runner/probe.go`, and
  `internal/systemd/generator.go`.
- Two CLI-managed HTTP/API/worker sessions obtained distinct ports. Each web
  endpoint reached its own API, and stopping the first preserved the second.
- Injecting worker exit 7 produced a bounded agent error page and a failure
  packet. Resuming the cursor did not replay the error. Evidence remained
  readable after stopping the session.
- Both daemons, managed process groups, and all four listening ports were
  verified released. No municipal application was started or configured.

## Scope boundary

This milestone does not add a package manager, build cache, arbitrary code
analysis, automatic database cloning, public preview hosting, or an MCP server.
The CLI/event contracts supply the foundation those integrations can consume.
