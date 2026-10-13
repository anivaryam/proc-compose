# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Changed

- Documentation reorganized: a shorter README, guides under `docs/`, this
  changelog, and `FEATURES.md`. Command help links point to the new pages.

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

[Unreleased]: https://github.com/anivaryam/proc-compose/compare/v1.6.0...HEAD
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
