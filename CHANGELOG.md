# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Fixed

- A dependency that fails before it is ready and is then restarted no longer
  fails its dependents. The dependents keep waiting for the replacement
  incarnation's readiness probe, and they fail only when the dependency's
  lifecycle ends for good.
- A dependent that is still waiting for a dependency when the stack is stopped
  now ends in the terminal state `exited`, with a `process.stopped` event,
  instead of staying `starting`. A stop that the operator asked for is no
  longer reported as a `dependency.failed` event; a dependency that really
  failed still fails its dependent.
- The `restart: always` policy now waits between restarts the way the failure
  path does: the delay doubles from 1 second up to a maximum of 30 seconds (1,
  2, 4, 8, 16, 30, 30) instead of staying at one second. The failure path's
  ceiling is 30 seconds as documented, not 32. On both paths a stop ends the
  wait at once, so stopping a stack never waits out the delay.
- The shutdown verdict names the processes that survived it. When the members
  of a process group could be enumerated, their PIDs are listed instead of
  saying that the members could not be enumerated. The explanation of an
  unavailable process group now belongs to the group it is reported for, so a
  healthy group is never given another group's reason.
- The monitor stops instead of redrawing in a hot loop when keyboard input
  ends (stdin closed or unreadable). The terminal is restored first and the
  monitor then prints `keyboard input closed — exiting monitor` on stderr.
  When the monitor was not killed by a hangup — a backgrounded monitor, or
  stdin on a pty that is not the controlling terminal — it previously kept
  writing frames with no way out.
- The monitor's highlighted row follows the process it names. A process that
  fails, restarts or appears no longer moves the cursor onto another row, so
  `Enter` filters the logs of the process that is highlighted.
- `PgUp` and `PgDn` page the monitor's log area by exactly one visible page.
  The page size is derived from the rendered frame, so consecutive pages are
  contiguous and no log line is skipped between them.
- Generated systemd units (`up --survive`) refer to the project paths as
  written. `%` is escaped as `%%` in every specifier-expanded setting,
  including `Description=`; the `ExecStart` executable and the `--file`
  argument are quoted; and each `Environment=` value is quoted as a whole
  assignment. A path containing a space is accepted instead of refused, and a
  path containing `%` no longer produces a unit that fails to load or runs
  against a rewritten path. A unit installed by an earlier version keeps the
  file it was written with: re-run `proc-compose up --survive --install` to
  regenerate it with the corrected quoting and escaping.
- A tail log query returns the newest matching events whenever the tail is
  requested, including when it resumes from a cursor. Under tail `has_more` is
  always `false`, `next_cursor` is the last event scanned, and `truncated`
  marks dropped events. The `logs` command output is unchanged.
- Resource labels are normalised when the config is loaded: surrounding
  whitespace is trimmed, and a label that differs only in whitespace from an
  earlier one names the same resource. The session holds one lease for it and
  lists it once, instead of taking two leases for one resource. The length and
  character checks on a label now run on the trimmed label.
- Two resource labels that expand to the same path no longer make a session
  conflict with itself: the session starts instead of failing with a lease
  error that names a foreign owner that does not exist. A label that is an
  absolute path once expanded is cleaned lexically, so `${config.dir}/db`, the
  literal path, a doubled or trailing separator and, on Windows, `/` for `\`
  are one resource. Any other label, including a path behind a prefix such as
  `next-build:`, is compared as written.
- The merge proxy's port is leased for the session's lifetime. A second
  proc-compose session that declares the same merge port is refused at startup
  (`resource "tcp:127.0.0.1:<port>" is owned by another session`) instead of
  failing later, when the proxy cannot bind. The lease excludes other
  proc-compose sessions only; the merge port is never reallocated and is never
  reported as a named port. The lease is taken only when the proxy is part of
  the selected process set: a filtered start that does not select
  `merge-port` never binds the port, so it no longer refuses a second session
  that would have bound it.
- The session metadata is written atomically, so `sessions` and `status` no
  longer briefly omit a session that is starting. The atomic replace always
  writes the file with mode `0600`, so an existing metadata file that had a
  wider mode is tightened.
- A metadata file that describes no session (no run id and no config path) is
  skipped instead of registering a phantom session named `default`.
- A failed connection to the daemon now names the operating system's reason:
  `no proc-compose daemon running (socket: ~/.proc-compose/daemon.sock): no
  such file or directory`. The socket path stays sanitized — the raw home
  path is never printed — and the returned error can be matched with
  `errors.Is`.
- An inbound command frame over the daemon's 64 KiB limit no longer tears down
  the connection. The daemon discards that one line and answers it with an
  `error` acknowledgment that names the limit, so the client can show why;
  commands sent after it on the same connection are still processed. The
  daemon also logs the discarded frame on stderr; the warning no longer names
  a peer, which is empty for a Unix socket.
- A shutdown verdict that the daemon could not deliver is no longer treated as
  delivered. When the acknowledgment write fails (the `stop` client went away)
  or the client was already answered with a timeout, the daemon records the
  verdict and the reason in the event journal as
  `shutdown.verdict_undelivered` (error). The barrier that holds the daemon's
  exit for the verdict is still released as soon as the write attempt
  completes, so a client that has gone away cannot wedge the daemon.
- A log rotation that fails no longer truncates the live log or leaves the log
  without a live file. The live file is renamed aside before anything else
  happens, so a failed rename keeps every byte that was written, and the write
  reports the failure instead of claiming success. A failure after that point
  is rolled back: the live file returns to its path and keeps growing past the
  limit, and every later write retries the rotation, so a transient failure
  such as running out of file descriptors heals at the next write. Retrying
  never drops another rotated copy, and a rotated copy that cannot be moved
  stops the rotation instead of being overwritten. A rotation interrupted by a
  crash is put back when the session next starts. A live log removed by
  another process is recreated at the next rotation, and that write reports
  that the bytes written to the removed file are lost. A run whose journal
  cannot be rotated ends with an `event journal: …` error and exits non-zero.
- `doctor` no longer reports a `missing_dir` error for a process `dir:` that
  still holds a runtime template such as `${config.dir}` or `${session.dir}`.
  The session expands those when it starts, so the doctor cannot say the
  directory is missing. A `dir:` that names a real path is still checked.
- `doctor` now also checks the config it suggests: a suggestion with a
  `merge:` section reports the missing `merge-port` binary even when no
  config exists yet. The finding names the suggested config so it can be
  told apart from a finding about the config on disk.
- `bootstrap --verify` verifies a config that resolves like the one that will
  run. The verification copy is still written to the system temp dir — nothing
  is written into the project — but its `dir:` values are rewritten to
  absolute paths, so the merge topology and the client API URL it checks are
  the ones the project actually uses.
- `bootstrap --verify` no longer leaves an empty log file behind in the
  system temp dir. The file is removed when it is empty and the verification
  report then omits `log_file`; a log file the child wrote to is kept.
- A verification attempt's receipt no longer claims a failed verdict while it
  is still running. The receipt written before the attempt starts carries
  `status: not_run`, `state: active` and the documented `precedence:
  no_verdict`, instead of `status: failed` with exit code 0 and an empty
  `precedence`; the terminal receipt still carries the real verdict. Every
  `precedence` value is documented in `docs/verify.md`.
- A receipt file that does not satisfy the receipt contract is refused like an
  unreadable one. A receipt is named after its `attempt_id` (a ULID), carries
  an `exit_code`, and records a `state`, `status` and `exit_code` that the
  writer produces together: `active` with `not_run` and 0 (or `failed` and 0,
  from earlier releases), or `completed` with `passed`/0, `failed`/1 or
  `interrupted`/130. `proc-compose attempts` skips such a file, `attempts
  latest` and `why --attempt latest` take the newest valid receipt instead of
  failing on it, and the file is preserved rather than pruned. Every receipt
  verify has written satisfies the contract.
- `why --attempt` never scans the whole journal for one attempt. A receipt
  without an attempt id is refused (see the receipt contract).
  `verify.Diagnose` scopes a record without one to its run (`boundary:
  run_id`, limitation `attempt_boundary_unavailable`). A record whose run id
  is null or empty reports `history_missing` with the limitation
  `journal_unavailable`, instead of an empty run id being read as no filter.
- `compare`'s verdict `outcomes_equal` is `false` only for a proven
  difference. It is `null` when nothing was proven but the data cannot rule a
  difference out: when one side's own repetitions disagree, or when an output
  was truncated at the 4 KiB capture cap. Truncated output that is equal up to
  the cap is no longer reported as equal, and it does not hide a difference
  proven elsewhere: a differing status, check status or exit code is `false`
  however much output was cut, and so are truncated outputs that differ at a
  byte both captures hold. When repetitions disagree, a field proves a
  difference only when it differs in every pairing of a baseline and a
  candidate repetition. Every limitation that applies is named in
  `limitations` (`nondeterministic_within_side`, `output_truncated`), also
  beside `false`. `status` stays `comparable` and the exit status stays 0. The
  human output now states the undetermined verdict instead of omitting it.
- `compare`'s `median_check_ms` is the true median of the check durations:
  the middle value for an odd number of samples, and the average of the two
  middle values for an even number, rounded half up.
- The human `compare` output prints a difference's values as values: an exit
  code reads as a number instead of a heap address, and a missing value reads
  as `(none)`. The `--json` output is unchanged.
- The human `verify` output lists the endpoints sorted by name, so the
  rendering is reproducible. The `--json` output is unchanged.
- `verify.Compare` extends the caller's `Options.ExtraEnv` instead of
  replacing it: the caller's variables reach the check, the tool's
  `PROC_COMPOSE_COMPARE_SIDE` and `PROC_COMPOSE_COMPARE_REPETITION` still win,
  and the caller's map is not mutated.
- The cleanup handoff terminates the managed process groups it sweeps instead
  of only sealing them. A managed tree that ignores `SIGTERM` no longer
  outlives the shutdown that was meant to stop it: the sweep force-kills the
  group while it is still ours to signal, and only then seals it. The group is
  reported as terminated rather than as a survivor. The same sweep governs the
  teardown phase and the normal shutdown, so a group that survives either of
  them is reported the same way. On Windows the forced kill empties the Job
  Object before the seal, so a terminated group is now observed empty and is
  reported as terminated rather than as a survivor.
- The sweep's forced kill no longer depends on the group's membership being
  enumerable. A group whose members cannot be enumerated is killed like any
  other group, because the authority to signal it is what decides — a group
  that was sealed without a kill kept its tree alive.
- A managed process group whose membership cannot be enumerated is signalled
  and waited for like any other group. The runner already holds the authority
  to signal it, so an unreadable `/proc` no longer skips the graceful signal
  and the forced kill that follows it.
- A merge upstream must be a **service**. A `task` or `teardown` process is no
  longer selected as one, so the merge wiring no longer writes a `ready_when`
  probe into a process whose mode forbids it and no longer adds a dependency
  edge to a teardown process.
- A merge port is no longer probed on a port the selected process does not
  serve. An upstream is matched by the `PORT` it is started with — its
  `env.PORT`, else the `PORT` in its `env_file`, else its `ports.http` port,
  with a `${ports.<process>.<port>}` reference resolved to that port's
  preferred value. A process selected by name (or by `merge.client_process`)
  whose `PORT` differs from `merge.client` / `merge.server` is refused with
  `invalid_merge` at `merge.client` / `merge.server` instead of being probed
  on the wrong port. A process whose `PORT` cannot be known when the config is
  loaded (none declared, an ephemeral `ports.http: 0`, or a reference only the
  session resolves) is still probed at the merge port.
- The merge upstream selection is deterministic. When two processes declare the
  same `PORT`, the lexicographically first one is now always selected, so the
  readiness probe, the dependency edge and the proxy command line are the same
  on every load.
- An `env_file` line longer than 1 MiB is now an error that names the file and
  the line (`env_file_unreadable`) instead of a silently truncated
  environment. A line of up to 1 MiB (1,048,576 bytes, not counting its `\n`
  or `\r\n` terminator) is parsed in full, whatever the line ending. The
  `env_file_unreadable` hint now states the line limit.
- `ready_timeout` below `-1` is refused (`invalid_value`). The published
  schema already declared `-1` as the floor.
- An `sh -c "..."` command on Windows now runs the script `sh` would run. The
  script argument loses exactly the quoting `sh` removes (one matched outer
  pair, and `\"`, `\\`, `\$` and `` \` `` inside double quotes) instead of
  every quote character being trimmed, and the quoted strings inside the
  script are requoted for `cmd`: `sh -c "echo 'x'"` and `sh -c "echo
  \"hello\""` print `x` and `hello` as on Linux and macOS, and a single-quoted
  argument with spaces reaches a program as one argument. The shim stays
  best-effort; docs/platforms.md lists what `cmd` cannot express.
- A daemon that dies during startup now reports `died during startup` instead
  of `did not become ready within 10s`, and the failure is reported as soon as
  the child exits rather than after the readiness timeout.
- A log query that opens the journal while it is being rotated no longer
  reports that the journal does not exist or asks to be retried: it reads the
  staged file `<journal>.staging` in place of the live journal. A journal
  whose live file is missing with nothing staged is read from its rotated
  copies; a journal with no file left is still reported as missing. The `retry
  this query` error remains only for a journal that keeps rotating while the
  query opens it.
- A dependent that is cancelled while its dependency is between a restart
  request and the dependency's next incarnation now ends in the terminal state
  `exited` instead of being failed with a `dependency.failed` event.
- The release lookup that the update notice schedules is now waited for before
  the process exits, for at most the lookup's own 3-second timeout, so a short
  command records the result for the next invocation. The attempt is recorded
  before the lookup starts, so a release API that stalls or answers too slowly
  is waited for at most once per 24 hours instead of on every command; when
  the attempt cannot be recorded, no lookup is made. The lookup stays silent
  and best-effort, and a run without a terminal still never schedules one.
- The out-of-range merge-port error now names the field it is about
  (`merge.client`, `merge.server` or `merge.port`) instead of the whole
  `merge` section. The `invalid_merge` hint now states the 1-65535 port range,
  the `merge.client_process` rule and that an upstream whose `PORT` differs
  from the merge port is refused, and the `invalid_port` hint says that `.` is
  not allowed in port names.
- A process that is stopped while it waits to be restarted now ends in the
  terminal state `exited`, with a `process.stopped` event, instead of staying
  `restarting`.
- A `restart` request for a `restart: always` process that is waiting to be
  restarted now restarts it at once, as on the failure path. The request used
  to wait for the delay to end and then stop the incarnation that had just
  started.
- A launch whose exit cannot be observed no longer orphans the command's
  descendants or starts copies beside them. On FreeBSD, OpenBSD and NetBSD,
  which cannot observe an exit without reaping it, the command's whole process
  group is killed before the launch fails. When the wait on the command itself
  fails, no group signal is sent, as before; the process is then not restarted
  while its group may still have members, whatever its `restart` policy, and
  the shutdown verdict reports it as unverified.
- On Linux and macOS, process-group setup forces `Pgid` to 0 and honours a
  caller's `Setsid` instead of combining it with `Setpgid`, so a
  caller-supplied `SysProcAttr` can neither move the command into another
  group nor make it fail to start.
- Windows commands that contain double quotes now run as written. The command
  line is handed to `cmd /s /c` verbatim instead of through Go's argument
  escaping, which `cmd` does not understand and which turned every `"` into
  `\"`. A command that started with a quoted path, such as `"C:\Program
  Files\app\app.exe" --flag`, failed with "is not recognized", a quoted
  argument reached the program split and with its quotes, and the PowerShell
  template from `init --template minimal` printed its own command text instead
  of running it.
- A named port is held bound from the moment the session assigns it until
  right before the first process that declares or references it is launched,
  instead of being free between assignment and the application's bind. Another
  program, or the OS ephemeral range for `ports: {http: 0}`, can no longer
  take it while the stack starts; the remaining window is the launch itself. A
  port whose process never starts is held until the session ends. On Windows
  the hold covers `127.0.0.1` only: a program that binds the wildcard address
  can still take the port.
- A reload that changes `merge.port` is refused with `merge port changed;
  restart the session`, like a change to the resources or named ports, instead
  of restarting the proxy on a port the session does not lease.
- The daemon no longer discards a failed write of a client's initial snapshot.
  If part of the snapshot was written, the client is dropped instead of being
  served a stream whose framing is broken; if nothing was written, the client
  is still served without a snapshot. Either way the failure is logged on
  stderr.
- The `invalid_value` hint no longer tells you to use a non-negative value: it
  states each field's floor, including `ready_timeout: -1` (no limit), and the
  restart-policy and probe requirements.
- Every command that accepts `--json` reports an invalid `--session` as one
  JSON object on stdout. `verify` and `compare` keep their own result kinds
  (`usage_error`); every other command prints `{"operation", "status":
  "error", "message"}`, the form `restart` and `reload` already used, with
  nothing on stderr. `status`, `logs`, `why`, `sessions`, `config check`,
  `config plan`, `doctor`, `bootstrap`, `attempts` and `up --silent` used to
  print only a text error on stderr and leave stdout empty.
- A verification attempt whose injected clock (`verify.Options.Now`) reads the
  zero time now runs on the system clock, instead of panicking or failing with
  `startup_timeout` and `cleanup_unverified`. `Options.Now` documents that
  phase budgets are enforced against the system clock.
- On Windows, rewriting the session metadata, the PID file or a verify receipt
  no longer fails with `Access is denied` while another proc-compose command
  reads it, so `sessions`, `status` and cross-session attribution no longer
  miss a session or keep stale metadata during a save. proc-compose's readers
  allow the replace and the replace uses POSIX rename semantics; a reader
  outside proc-compose that blocks it is waited out for up to about 130 ms.

### Changed

- A config that defines a process named `merge-port` **and** a `merge:`
  section is refused with the new `reserved_process_name` code instead of
  having the user's process silently replaced by the generated proxy. A
  service literally named `merge-port` in a doctor-generated config is now
  rejected too.
- A port name may no longer contain a dot (`invalid_port`). The endpoint
  reference name is the process name and the port name joined by a dot, so a
  dotted port name let two endpoints claim one reference; configs with dotted
  port names were already broken. A dot in a **process** name stays legal.
- `merge.client`, `merge.server` and the resolved `merge.port` must be a port
  between 1 and 65535 and are refused with `invalid_merge` otherwise — an
  out-of-range `merge.port` used to surface as the misleading preflight
  message "port(s) already in use". `port: 0` still means unset.
- The published JSON Schema now states every loader rule JSON Schema can
  express: a non-empty `processes` map or a `merge` section, the
  `task`/`teardown` restrictions, the fields that need a probe or a restarting
  policy, exactly one non-empty probe kind, the `merge` modes and port ranges
  (including numeric route targets), the reserved `merge-port` name next to a
  `merge` section, and the trimmed resource label rule (a whitespace-only
  label, or one with an inner line break or NUL, is now schema-invalid; a
  padded label stays valid). The configuration docs list the loader rules it
  cannot express, and the values the YAML decoder reads more leniently than
  the schema.
- `up --survive` refuses a project path that contains a newline, a carriage
  return, a NUL byte, a quote, a backslash or `$`, and its error message now
  names those characters instead of claiming that spaces are refused. A path
  containing a space or a tab is accepted and quoted.
- `reload` with a reordered `resources` list applies the reload. The order of
  the labels is not a resource-set change: only adding or removing a resource
  still requires a new session.
- `doctor` reports its findings in a deterministic order: the processes are
  iterated by name, so repeated runs produce the same findings in the same
  order and the output can be compared across runs. The reported pair of a
  duplicate-port finding is now the two lexicographically-first processes; the
  number of findings does not change.
- The platform documentation now states the leader-side `setsid(2)`
  restriction on Linux and macOS: a managed command leads its own process
  group, so a program that calls `setsid(2)` itself fails with `EPERM`.
  Descendants may call it. On Linux the util-linux `setsid(1)` utility is
  unaffected, so `cmd: setsid server` keeps working there; macOS ships no
  `setsid(1)`. FreeBSD, OpenBSD and NetBSD are recorded as unsupported: they
  build, but every launch fails there.
- Every command on Windows now runs under `cmd`, including one that uses `&&`,
  `||`, `|`, `>`, `>>`, `<` or `2>&1`, and one with an unbalanced `$(`.
  Nothing is routed to Windows PowerShell 5.1, which has no `&&` or `||`,
  writes a `>` redirection as UTF-16 with a byte-order mark, and cannot parse
  an unbalanced `$(` either. `echo hi > out.txt` now writes `hi \r\n` (cmd's
  `echo` keeps the space before `>`) instead of UTF-16. PowerShell syntax has
  to name `powershell` in the command.
- A merge route whose target is a port number must name a port between 1 and
  65535 and is refused with `invalid_merge` at `merge.routes[<index>]`
  otherwise (`/api=99999` used to load). A target written as a URL is passed
  to merge-port as written.
- `merge.client_process` must name a service: a task or teardown process is
  refused with `invalid_merge` at `merge.client_process`. The `api_prefixes`
  scan and the client API URL injection now pick from the same service
  candidates as the upstream wiring, so a task named like the server is no
  longer scanned and a task named like the client no longer receives the
  injected API URL.
- An invalid `--session` exits with status `2` on every command, with or
  without `--json`, the usage status `verify` and `compare` already used.
  Every other command, including `restart --json` and `reload --json`, exited
  `1`. Other errors keep their exit statuses.
- On Windows, the event journal and a `--log-file` rotated with
  `--max-log-size` stay open in a mode that lets other programs rename or
  delete them, as on Linux and macOS; a removed live file is recreated at the
  next rotation.

## [1.11.0] - 2026-10-19

### Added

- Opt-in `--json` results for `restart` and `reload`, reusing daemon
  acknowledgments while preserving human output and exit codes. Results
  distinguish partial reloads, busy/rejected requests and replacement
  readiness failures without adding daemon operations.

### Fixed

- Corrected runtime-directory fallback and custom log-file documentation;
  clarified JSON parser exceptions and agent/browser session ownership,
  readiness, diagnostics and cleanup guidance. No runtime behavior changed.

## [1.10.0] - 2026-10-18

### Added

- Session-aware probe owners. A `foreign` probe owner now also has a `scope`,
  read from the other sessions in the same runtime directory when the answer
  is judged: `cross_session` when the answer belongs to another live session,
  with `session` naming it (`name`, `run_id`, `config_path`, `worktree`,
  `process`, `port`) and its `evidence` (`holder`: the holder is a member of a
  process group that session recorded, proven by the group leader's start
  time, Linux only; `port_lease`: the answered port is one that session was
  assigned and leases), or `outside_session` when no live session is known to
  hold it. A session counts as live only while its supervisor runs with the
  start time its PID file recorded, so a finished or crashed session never
  does. The fields are added to `readiness_owner` and `health_owner` in
  `status --json`, to the `owner` of `verify` readiness observations, and to
  the journal's state events; the `result` values and existing fields are
  unchanged. Warnings and human `verify` output name the other session.
  Receipts written by earlier versions have no `scope`, and earlier versions
  ignore it. macOS attributes no answers, so none are placed there.
- `verify --allow-cross-session` counts readiness and health answers from
  another live session again (see Changed).

### Changed

- `verify` refuses a readiness or health answer from another live session
  (`scope: cross_session`) by default, the way `foreign_listener: reject`
  refuses a foreign one: readiness keeps waiting for the process's own server
  and, at `ready_timeout`, fails with an error naming the other session, so
  the attempt fails at startup; a health check counts as failed. On Linux
  both `holder` and `port_lease` evidence refuse; on Windows, which records no
  process groups, only `port_lease` does; macOS never refuses. Answers that
  are `outside_session` or `unattributable` are taken as before, and `up`,
  `compare` and the other commands only label such answers. Pass
  `--allow-cross-session` to restore the previous behavior. The
  `examples/environment-report` pilot now expects this refusal for S3c, and
  runs its baseline with `--allow-cross-session` when the baseline binary has
  it.

## [1.9.0] - 2026-10-15

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

[Unreleased]: https://github.com/anivaryam/proc-compose/compare/v1.11.0...HEAD
[1.11.0]: https://github.com/anivaryam/proc-compose/compare/v1.10.0...v1.11.0
[1.10.0]: https://github.com/anivaryam/proc-compose/compare/v1.9.0...v1.10.0
[1.9.0]: https://github.com/anivaryam/proc-compose/compare/v1.8.0...v1.9.0
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
