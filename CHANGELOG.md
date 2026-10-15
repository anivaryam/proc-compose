# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- Probe owner attribution (Linux and Windows). An `http` or `tcp` readiness
  probe or health check passes when any process answers at its address, so a
  server already listening there (another worktree's dev server on a
  hardcoded port, a stale run) made a process ready before it had bound
  anything. proc-compose now records who answered each `http` or `tcp`
  readiness probe and health check. As soon as each attempt's connection is
  established, before its request is sent, it finds the server side of that
  connection and the process holding it (procfs on Linux; the TCP table on
  Windows), then compares that process with the incarnation's process group
  or Job Object: `owned`, `foreign` (with the holder's `pid` and process
  name; only for a process of the same user) or `unattributable` (with a
  `reason`; also for another user's process, which is never named, and for a
  server outside this network namespace, such as a container address). `status --json` shows
  `readiness_owner` and `health_owner`; `verify` readiness observations gain
  `owner`, and human `verify` output prints a warning for a foreign one. A
  foreign answer logs `readiness.foreign_listener` or
  `health.foreign_listener` (warn). The named processes are only observed,
  never signalled, and only their process name is read. macOS reports
  `unattributable`. Receipts written by earlier versions have `owner: null`.
- `foreign_listener` process field: `allow` (the default) keeps the behavior
  above, where attribution only reports. `reject` makes an `http` or `tcp`
  probe attempt answered by a foreign process count as failed: readiness
  keeps waiting for the process's own server and, at `ready_timeout`, fails
  with an error naming the holder; a health check counts as failed. An
  `unattributable` answer is never refused. It requires an `http` or `tcp`
  `ready_when` or `health_when`.

### Changed

- `http` probes and health checks no longer reuse connections: every
  attempt opens its own, so who answered it can be attributed, and a
  refused answer's connection is never used again.

### Fixed

- The configuration docs said `http` and `tcp` readiness probes poll every 3
  seconds. They make their first attempt at once and retry about once a
  second; 3 seconds is the progress-message interval.

## [1.8.0] - 2026-10-14

### Added

- Environment reports for `verify` and `compare`. The check receives
  `PROC_COMPOSE_ENVIRONMENT_REPORT`, a per-attempt path where it can write
  `{"observations": [{"name", "status", "expected", "observed"}]}` with status
  `matched`, `mismatched` or `unobserved`, to say whether it exercised the
  intended build, endpoints and data. Results and receipts gain an
  `environment` object (`required`, `status`, `reason`, `observations`) with
  status `eligible`, `ineligible`, `unknown` or `not_run`, kept separate from
  `check.status`. An `ineligible` environment fails the attempt with
  `environment_ineligible`; an invalid report fails it with
  `environment_unverified`. `--require-environment` also fails an attempt whose
  check writes no report or no observations. `compare` reports
  `not_comparable` when any attempt has either environment error, passes
  `--require-environment` to both sides, and shows each attempt's
  `environment_status`. `verify.compact_error` lists `environment` among its
  `phase_statuses`, and `why --attempt` lists it among its phases. Receipts
  written by earlier versions have no environment status. The observations are
  the check's own; proc-compose does not verify them. A report that is not one
  JSON object, or that repeats a field in any object, is invalid.
- `examples/environment-report`: a runnable stateful example (API, worker,
  SQLite, file queue) whose check reports its environment, with a pilot of
  seeded faults and acceptance gates, and an evidence export that is read after
  the runtime directory is removed. The CI recipe in `docs/verify.md` now also
  keeps `why --attempt` output.

## [1.7.0] - 2026-10-13

### Added

- `ready_when.cmd`: a readiness probe that runs a shell command in the
  process's directory and environment and passes when it exits 0, for
  processes that open no port and print no ready line. Failed attempts are
  retried one second apart until `ready_timeout`; each attempt is limited to
  10 seconds; a timeout's error names the last attempt's exit status and the
  start of its stderr. The state and output of probe runs stay out of `status`, the
  monitor and the logs; probe runs are stopped and verified with the process
  they probe. `config plan` reports the
  probe as `cmd`, and `verify` receipts record it as the readiness probe.
- `health_when`, `health_interval` and `health_action`: ongoing health checks
  for services after they become ready. `health_when` takes an `http`, `tcp`
  or `cmd` probe (`log` is rejected); checks run one at a time every
  `health_interval` seconds (default 10, also used for 0), each limited to the
  interval or 10 seconds, whichever is shorter. Three failed checks in a row
  make the service `unhealthy`, and one pass makes it `healthy` again.
  `health_action: report` (the default) keeps the service running;
  `health_action: restart` restarts it through its restart policy, counting
  toward `max_restarts`, and is rejected with `restart: never`. `status
  --json` and IPC state report `health` and `health_error`, the monitor shows
  `unhealthy` in its readiness column, `/health` returns 503 while a service
  is unhealthy (`/ready` and `--wait-ready` are unchanged), `verify` fails the
  attempt with `required_failed` when a required service becomes unhealthy
  during the check, and the journal records `health.check_failed`,
  `health.unhealthy` and `health.recovered` events. A `cmd` check that
  proc-compose cannot launch (for example when the crash-recovery record
  cannot be written) is logged as `health.check_skipped` and never counts as a
  failure; `health` reads `unknown` until a check runs again. `health` is also
  `unknown` whenever the process is not running. `config plan --json`
  reports each process's health check.
- `mode: teardown`: processes that run once after the stack stops, to release
  what it held. They run only after every managed process is verified
  terminated (otherwise they are skipped and reported), one at a time in name
  order within a fixed 15-second budget, after `stop`, `stop --force`, Ctrl+C,
  a stack that ends on its own, or a failed task, and for the whole stack
  under `up <process>`. They take `cmd`, `dir`, `env`, `env_file` and a
  `shutdown_timeout` of at most 5, get the session's variables and
  references, and cannot be depended on or selected. A teardown process that
  does not pass fails `up` and the stop verdict with lines starting
  `teardown:`, without changing whether termination was verified. The journal
  records `teardown.started`, `teardown.completed`, `teardown.failed` and
  `teardown.skipped`; `config plan` lists them under `teardown`. A changed
  teardown definition applies on `reload` without a restart. `verify` runs
  teardown inside its cleanup budget and records it in the receipt's
  `cleanup.teardown`; a teardown that does not pass fails the attempt with
  `teardown_failed`. `stop` waits 15 seconds longer by default when the
  session declares teardown processes, says "termination was verified, but
  teardown did not pass" when only teardown failed, and reports "teardown did
  not run" after a daemon crash. Session metadata lists the teardown
  processes (`teardown`).
- Docs: a reset → seed → services recipe with chained tasks.

### Fixed

- `verify` no longer races on its stack cleanup budget when the stack stops
  on its own, for example after a required process fails. The budget now
  starts at that stop, like any other cleanup trigger, instead of being
  written after the runner may already have read it.
- `stop` no longer reports success on Linux and macOS when the daemon has
  crashed and its managed processes cannot be verified as terminated (see the
  next entry for how recorded process groups are recovered). For a PID file
  without a group record, or one that cannot be read, `stop` exits non-zero,
  keeps the PID file, and says the processes were not verified; `stop --force`
  clears that stale state with a warning; it exits 0 for a readable file and
  non-zero for an unreadable one, which may have recorded groups. On
  Windows a crashed daemon's managed processes end with its job objects, so
  `stop` clears the stale state and exits 0.
- After a daemon crash, `stop` now recovers from the PID file's process-group
  record. On Linux it terminates every recorded group whose leader is still
  alive with its recorded start time, signalling each member through a pidfd
  (SIGTERM, then SIGKILL at `--timeout`; SIGKILL at once with `--force`). It
  reports groups that are already gone as empty and leaves the rest unknown
  without signalling them: a group whose leader was reaped while members keep
  running, a launch that never started, a record or machine without a
  readable boot ID, pids from another namespace (or a
  `/proc` mounted for another PID namespace), or a kernel without pidfds.
  Processes that left their group with `setsid` or `setpgid` are not in the
  record, so `stop` neither reports nor signals them. On macOS it never
  signals; it only reports groups as empty or unknown. `stop` prints one `recovered group=... outcome=...
  reason=...` line per group, exits 0 only when every group is empty or
  terminated, and refuses without signalling while another command holds the
  session. With a group left unknown, `stop --force` clears the state but
  exits non-zero. A PID file without a group record keeps the earlier
  behaviour, and an unreadable PID file can now be cleared with `stop --force`,
  which still exits non-zero because its groups are unknown.
- `up` and `verify` no longer overwrite a crashed run's PID file while its
  recorded process groups may still be running. `up` refuses and points at
  `proc-compose stop`; `verify` fails with `session_occupied`. Neither
  signals anything. On Unix an unreadable PID file also refuses the start
  instead of being deleted.
- A service stopped because its readiness probe timed out (`ready_when` with
  `ready_timeout`) now always counts as failed, even when it exits 0 on
  SIGTERM. Before, depending on timing, it could be reported as `exited`: it
  did not restart under `restart: on-failure`, did not count toward
  `max_restarts`, and did not fail the run under `restart: never`.
- A readiness probe that finishes after its process was restarted or stopped
  no longer marks the next incarnation ready.
- After `reload` replaces a process definition, `status`, `/ready`, and
  `/health` describe the running incarnation's definition, the same one the
  event stream reports, instead of the definition loaded at startup. Both now
  come from one builder. `reload` already refuses mode and `depends_on`
  changes, so no field in today's output differed.

### Changed

- Documentation reorganized: a shorter README, guides under `docs/`, this
  changelog, and `FEATURES.md`. Command help links point to the new pages.
- `docs/cli.md` now describes how `stop` and `stop --force` actually work: the
  daemon terminates its managed processes and reports a verdict. It no longer
  says `stop` sends SIGTERM and then SIGKILL to the daemon.
- On Linux and macOS, the daemon's PID file now records each process group it
  launches until that group is proven empty, so a crashed run can be
  recovered. The entry is written before the command starts; if it
  cannot be written, that launch fails and the process's restart policy
  applies. If the PID file cannot be updated when the stack starts, or it is
  deleted while the stack runs (for example when its runtime directory is
  removed), recording is turned off for the rest of that run with a warning
  and launches continue. A PID file that another daemon has recreated in the
  meantime is never overwritten.
- PID files are now written atomically (temporary file, sync, rename), the
  same way verification receipts already were.

## [1.6.0] - 2026-10-13

### Added

- `verify -- CHECK...` starts a fresh session, starts the selected processes,
  waits for readiness, runs the check directly (no implicit shell), stops
  everything it started, and records a receipt. Exit codes: 0 passed,
  1 failed, 2 nothing started, 130 interrupted.
- `attempts`, attempt-aware `why --attempt`, `sessions`, `compare`, and
  `config check|plan|schema` commands.

### Changed

- Linux binaries are statically linked on amd64 as well as arm64, so they run
  on any distribution, including older glibc releases and musl-based ones such
  as Alpine. The amd64 binary previously required glibc 2.34 or newer.
- On Windows, a configured command now starts suspended for the brief interval
  before it joins its Job Object.

### Fixed

- Windows: a child spawned immediately by a command can no longer escape
  cleanup by starting before the command joins its Job Object.
- Run identifiers stay unique when the clock does not advance between two runs.
- An interruption after readiness but before the check starts is reported as
  interrupted (exit 130), not as a check start failure.
- IPC commands sent while a stack shuts down, including `stop`, are answered
  instead of timing out after 30 seconds.
- Windows: `logs` no longer fails with "Access is denied" when it races the
  deletion of the oldest rotated journal file.
- Linux and macOS: a command that exits with a failure is no longer
  occasionally reported as a clean exit.

## [1.5.2] - 2026-10-10

### Added

- `agent-guide` prints an offline, version-matched operating guide for coding
  agents.
- `--session NAME` selects an independent daemon identity, journal, and runtime
  directory.
- Named `ports` with `${ports.*}` and `${urls.*}` references, and exclusive
  `resources` leases between sessions.
- Structured event journal with `logs --json` pages, resumable cursors,
  retention gaps, truncation metadata, and filters.
- `why PROCESS` produces a bounded failure evidence packet.
- `restart --wait-ready`, `up --silent --wait-ready --json` session metadata,
  and a `/ready` endpoint on the health server.

### Fixed

- Failed `restart: never` services can recover while their daemon stays live.
- Reloaded processes use their new readiness probe and timeout; unsupported
  mode or dependency changes are rejected before anything changes.
- Environment changes distinguish absent keys from empty values.
- Daemon JSON output no longer duplicates plain-text log entries.
- Selected-process dry runs use the real transitive dependency selection.
- Windows journal readers no longer block log rotation.

### Security

- Release binaries are built with Go 1.26.9 to address standard-library
  vulnerabilities, and the CI vulnerability scan now fails on findings.

## [1.5.1] - 2026-10-03

### Fixed

- Update notifications target the directory of the running binary instead of
  assuming brokit manages it.
- The install-directory override is passed to the installer side of the pipe.
- PowerShell guidance covers both brokit-managed and unmanaged installs.
- Brief Windows cache-replacement access errors are retried.

## [1.5.0] - 2026-10-03

### Added

- Interactive update notifications when a newer stable release exists, backed
  by a shared per-user 24-hour cache that never delays a command.

## [1.4.13] - 2026-10-02

### Fixed

- `stop` and `stop --force` report success only after termination of the
  managed process tree is confirmed.
- Descendants in the same process group are terminated even when their parent
  exits first.
- Termination that cannot be verified is reported as a failure, not as success.

## [1.4.12] - 2026-10-01

### Fixed

- Services are reported as not ready after exits, failures, and during
  restarts; completed tasks keep their readiness.
- `--wait-timeout` is enforced when a daemon stops sending events.

## [1.4.11] - 2026-05-31

### Fixed

- Port preflight tests run on every platform.

## [1.4.10] - 2026-05-31

### Changed

- Process `dir` is resolved from the config file's directory.
- Stricter `merge:` and platform command checks.
- Documented cross-platform support and the Windows shell contract.

## [1.4.9] - 2026-05-31

### Fixed

- Stable border rendering in the monitor TUI.

## [1.4.8] - 2026-05-21

### Fixed

- Daemon discovery works across `XDG_RUNTIME_DIR` mismatches.

## [1.4.7] - 2026-05-18

### Fixed

- Monitor enables VT processing on Windows terminals.

## [1.4.6] - 2026-05-18

### Added

- Command help links to the online documentation.

## [1.4.5] - 2026-05-18

### Added

- Expanded command help for modes, environment variables, and daemon paths.

## [1.4.4] - 2026-05-17

### Fixed

- Help overlay alignment in the monitor TUI.

## [1.4.3] - 2026-05-17

### Fixed

- Monitor column alignment for STATUS, CPU, and MEM.

## [1.4.2] - 2026-05-17

### Fixed

- MEM column width when values overflow.

## [1.4.1] - 2026-05-17

### Fixed

- Race between IPC client shutdown and event fan-out.
- Config discovery on macOS symlinked paths.

## [1.4.0] - 2026-05-17

### Added

- `mode: task` for one-shot processes such as migrations; dependents start
  after a task exits 0.
- Config discovery from child directories.
- `bootstrap --write --force` overwrites an existing config.

### Changed

- Improved `doctor` service detection and monitor status rendering.

## [1.3.1] - 2026-05-15

### Added

- `ready_when` is injected for merge-port using its listen port.

## [1.3.0] - 2026-05-15

### Fixed

- Monitor flicker, using synchronized output and per-row erase.
- Test helpers on Windows and macOS.

## [1.2.0] - 2026-05-15

### Added

- `bootstrap` scans a project and generates a config, optionally validated with
  a dry run.

## [1.1.0] - 2026-05-15

### Added

- `doctor` scans a project and diagnoses proc-compose setup.

## [1.0.0] - 2026-05-11

### Added

- Initial release: processes from one YAML file, color-coded logs, restart
  policies, readiness probes, dependencies, background daemon, monitor TUI,
  and merge-port integration.

[Unreleased]: https://github.com/anivaryam/proc-compose/compare/v1.8.0...HEAD
[1.8.0]: https://github.com/anivaryam/proc-compose/compare/v1.7.0...v1.8.0
[1.7.0]: https://github.com/anivaryam/proc-compose/compare/v1.6.0...v1.7.0
[1.6.0]: https://github.com/anivaryam/proc-compose/compare/v1.5.2...v1.6.0
[1.5.2]: https://github.com/anivaryam/proc-compose/compare/v1.5.1...v1.5.2
[1.5.1]: https://github.com/anivaryam/proc-compose/compare/v1.5.0...v1.5.1
[1.5.0]: https://github.com/anivaryam/proc-compose/compare/v1.4.13...v1.5.0
[1.4.13]: https://github.com/anivaryam/proc-compose/compare/v1.4.12...v1.4.13
[1.4.12]: https://github.com/anivaryam/proc-compose/compare/v1.4.11...v1.4.12
[1.4.11]: https://github.com/anivaryam/proc-compose/compare/v1.4.10...v1.4.11
[1.4.10]: https://github.com/anivaryam/proc-compose/compare/v1.4.9...v1.4.10
[1.4.9]: https://github.com/anivaryam/proc-compose/compare/v1.4.8...v1.4.9
[1.4.8]: https://github.com/anivaryam/proc-compose/compare/v1.4.7...v1.4.8
[1.4.7]: https://github.com/anivaryam/proc-compose/compare/v1.4.6...v1.4.7
[1.4.6]: https://github.com/anivaryam/proc-compose/compare/v1.4.5...v1.4.6
[1.4.5]: https://github.com/anivaryam/proc-compose/compare/v1.4.4...v1.4.5
[1.4.4]: https://github.com/anivaryam/proc-compose/compare/v1.4.3...v1.4.4
[1.4.3]: https://github.com/anivaryam/proc-compose/compare/v1.4.2...v1.4.3
[1.4.2]: https://github.com/anivaryam/proc-compose/compare/v1.4.1...v1.4.2
[1.4.1]: https://github.com/anivaryam/proc-compose/compare/v1.4.0...v1.4.1
[1.4.0]: https://github.com/anivaryam/proc-compose/compare/v1.3.1...v1.4.0
[1.3.1]: https://github.com/anivaryam/proc-compose/compare/v1.3.0...v1.3.1
[1.3.0]: https://github.com/anivaryam/proc-compose/compare/v1.2.0...v1.3.0
[1.2.0]: https://github.com/anivaryam/proc-compose/compare/v1.1.0...v1.2.0
[1.1.0]: https://github.com/anivaryam/proc-compose/compare/v1.0.0...v1.1.0
[1.0.0]: https://github.com/anivaryam/proc-compose/releases/tag/v1.0.0
