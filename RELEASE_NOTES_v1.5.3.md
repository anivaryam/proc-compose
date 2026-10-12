# v1.5.3 — Verified runs and Windows containment

This release adds the `verify` workflow and related agent-runtime commands, and
fixes process containment on Windows. Existing configurations and default
sessions remain supported.

> Maintainer decision pending: these notes describe new CLI commands. Under
> semantic versioning that is a minor release (v1.6.0), not a patch. v1.5.2
> also shipped features as a patch; choose the version before tagging.

## New

- `proc-compose verify -- CHECK...` acquires a fresh session, starts the
  selected processes, waits for readiness, runs the check directly (no
  implicit shell), stops everything it started, and records a receipt. Exit
  codes: 0 passed, 1 failed, 2 nothing started, 130 interrupted.
- Attempt-aware `why`, attempt listing and comparison, session listing, and
  configuration explanation commands, described in `proc-compose agent-guide`.

## Fixes

- Windows: commands are created suspended and resumed only after they are in
  their Job Object. A child spawned at once by a command can no longer escape
  cleanup. Cancellation, assignment failure, and resume failure in that window
  end the command before it runs.
- Run identifiers are unique within a process even when the clock does not
  advance between two runs (seen on Windows).
- An interruption that arrives after readiness but before the check starts is
  reported as interrupted (exit 130), not as a check start failure.
- The compact JSON error keeps its status and exit precedence.
- Windows: `logs` queries no longer fail with "Access is denied" when they
  race the deletion of the oldest rotated journal file. The race now gives
  the documented retryable error, and real permission errors are still
  reported.
- A command that exits with a failure is no longer occasionally reported as a
  clean exit on Linux and macOS. The exit status could be lost when the
  process was reaped quickly, so a failed `restart: never` service could show
  as `exited`.

## Compatibility

- No configuration changes. Run identifiers keep the `<pid>-<nanoseconds>`
  format.
- On Windows, a configured command's process now starts suspended for the brief
  interval before it joins its Job Object.

## Maturity and limitations

- `verify` and the agent-runtime commands are first implementations; measured
  benefit for coding agents is not yet established.
- CI verification is **pending**. Locally the fixes passed natively on Linux
  (with the race detector) and on Windows (without it); macOS was only
  cross-compiled. Replace this line with the CI run ID and job results of the
  green release-candidate commit before publishing.
- `stop --timeout` is still ignored on Windows, and Windows has no graceful
  signal phase.
