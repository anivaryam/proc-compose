# Platform support

## Platform matrix

| Feature | Linux | macOS | Windows |
|---------|-------|-------|---------|
| Process orchestration | Full | Full | Full |
| `merge:` sections | Full | Full | Full (requires `merge-port`) |
| `--survive` / systemd auto-restart | Full | N/A | N/A |
| `uninstall` | Full | N/A | N/A |
| Named pipes IPC | Unix sockets | Unix sockets | Full |
| Graceful stop (`--timeout`) | Full (SIGTERM) | Full (SIGTERM) | N/A (always `taskkill /F`) |
| `init` template | POSIX shell | POSIX shell | PowerShell |
| [Probe owner attribution](configuration.md#who-answered-the-probe) | Full (procfs) | N/A (`unattributable`) | Full (TCP table and Job Object) |
| [Refusing another session's answer](configuration.md#answers-from-another-session-under-verify) (`verify`) | Full (holder in a recorded process group, or a leased port) | N/A (never refuses) | Leased port only (no process groups are recorded) |

## Linux

Full support including `--survive` for systemd user unit auto-restart on reboot.

## macOS

Full process orchestration support. No systemd user services, so `--survive`, `--install`, and `uninstall` are unavailable. Use `launchd` or a similar mechanism for auto-restart on macOS.

## Windows

Full process orchestration support. Named pipes IPC via `go-winio`. Windows Job Objects for process tree termination via `go-winjob`. No `--survive` or `uninstall` (no systemd user units on Windows). `stop --timeout` is ignored; `stop` always uses `taskkill /T /F`.

Shell commands in configs run under `cmd /s /c "<command>"`, and the command reaches `cmd` exactly as written: nothing is quoted or escaped on the way. Quotes therefore work as they do at a `cmd` prompt — `"C:\Program Files\app\app.exe" --name "my app"` runs that program with the argument `my app` — and paths with spaces need them. Every command runs under `cmd`, including one that uses `&&`, `||`, `|`, `>`, `>>`, `<` or `2>&1`: those are `cmd`'s own operators. No command is routed to PowerShell, because Windows PowerShell 5.1 has no `&&` or `||`, writes a `>` redirection as UTF-16 with a byte-order mark, and reads `$(...)` as its own subexpression. A redirection writes the bytes the program produced. `cmd`'s `echo` keeps the space before an operator and ends its line with CRLF, so `echo hi > out.txt` writes `hi \r\n` and `echo hi>out.txt` writes `hi\r\n`. Use cmd environment syntax such as `echo %PORT%`. If you want PowerShell syntax, make it explicit in the command text: `powershell -NoProfile -Command "Write-Output $env:PORT"`.

POSIX syntax such as `$VAR`, `$(...)`, `trap`, `export`, and shell loops is not promised to be portable. Existing `$VAR` translation (`$VAR` and `${VAR}` become `%VAR%`) is best-effort compatibility only; `$(...)` becomes `%(...)%`, which `cmd` prints as written. Windows-native commands may need backslashes. POSIX examples elsewhere in these docs are Linux/macOS examples unless labelled otherwise. The `init --template minimal` on Windows generates a PowerShell-based template.

`sleep N` is translated wherever `cmd` runs it as a command: at the start of the command, or after `&&`, `||`, `&`, `|` or a parenthesis, so `sleep 2 && npm start` waits two seconds and then starts `npm`. `timeout /t` cannot stand in for it, because `timeout` refuses to run unless its input is a console, and a managed process's input is not one. A whole number of seconds becomes `ping -n N+1 127.0.0.1 >nul`, which waits N seconds. `cmd` has no finer wait, so a fraction, such as `sleep 0.5` or `sleep 1.5`, is rounded up to the next whole second (1 and 2 seconds): the wait is never shorter than the time asked. On an idle machine it ends within about a tenth of a second of that whole number of seconds. `cmd` has no `;` operator, so `sleep 1; npm start` is not translated: write `sleep 1 && npm start`. A `sleep` with any other argument — a unit such as `5s`, a variable, or more than one number — is not translated either, and `cmd` reports that `sleep` is not recognized unless a `sleep` program is on `PATH`.

An `sh -c "..."` command is accepted as a best-effort compatibility shim. Its script argument first loses the quoting `sh` would remove: a single-quoted script is taken literally, and in a double-quoted one `\"`, `\\`, `\$` and `` \` `` become `"`, `\`, `$` and `` ` ``. A script with no outer quotes, an unmatched quote, or text after its closing quote is passed on as written. The quoted strings inside the script are then requoted for `cmd`, and the `$VAR` translation applies:

- A quoted string made only of letters, digits and `-_./:=+` loses its quotes, as `sh` would remove them: `sh -c "echo 'x'"` prints `x`, and `sh -c "echo \"hello\""` prints `hello`.
- Any other single-quoted string is put in double quotes, so a program receives it as one argument: in `sh -c "app 'a b' 'c&d'"`, `app` receives `a b` and `c&d`.
- A single-quoted string that contains `"`, `%`, `$` or a line break, or ends in a backslash, is passed on as written, single quotes included: neither `cmd` nor a program treats `'` as a quote. `cmd` still expands `%VAR%` inside it, and so does the `$VAR` translation.
- Any other double-quoted string is kept as written. A program that parses its command line with the usual C runtime rules receives it as `sh` would pass it — `"say \"hi\""` arrives as `say "hi"` — except that the backslash of `\\`, `\$` or `` \` `` is kept, and the `$VAR` translation still applies after `\$`.

Where `cmd` cannot express the `sh` meaning, the output differs from Linux and macOS. Observed with `PORT=3000`:

| Command | Linux/macOS prints | Windows prints |
|---------|--------------------|----------------|
| `sh -c "echo 'a b'"` | `a b` | `"a b"`: `cmd`'s `echo` prints the quotes a program would not receive |
| `sh -c "echo '\$PORT'"` | `$PORT` | `'3000'` |
| `echo a && echo b` | `a` and `b` | `a ` (with the space before `&&`) and `b` |
| `echo $(date)` | the date | `%(date)%` |
| `sleep 1; echo hi` | `hi`, a second later | an error: `'sleep' is not recognized` (`cmd` has no `;`; use `&&`) |

## Other Unix platforms (FreeBSD, OpenBSD, NetBSD)

Unsupported. proc-compose builds on these platforms, but every launch fails:
proc-compose observes a managed command's exit without reaping it, and only
Linux and macOS can do that. Where the observation is unavailable the launch is
refused, because the alternative — terminating a process group the runner
cannot prove it owns — is not safe.

A refused launch leaves nothing running: the command's whole process group is
killed right after the start, while the group is still provably the command's
own. These platforms cannot list a group's members, so the group is never
proven empty: the process ends `failed`, the shutdown verdict reports its
termination as unverified, and it is not restarted, whatever its `restart`
policy.

## `setsid(2)` and the managed process group

On Linux and macOS every managed command is the leader of its own process
group, so the whole tree can be signalled at once. POSIX forbids a
process-group leader from calling `setsid(2)`, so such a call fails with
`EPERM` ("operation not permitted").

That restriction applies to the **leader** only:

- A managed command that calls `setsid(2)` itself fails with `EPERM`. The
  command's own exit status is what proc-compose reports; the cause is the
  bare `EPERM`.
- A **descendant** of a managed command may call `setsid(2)` freely — it is
  not the group leader. It does leave the process group, so proc-compose can
  no longer signal it; see [Stop a running
  daemon](cli.md#stop-a-running-daemon).
- On Linux, the `setsid(1)` **utility** from util-linux is unaffected: it
  forks when its caller is already a process-group leader, so
  `cmd: setsid server` works. macOS ships no `setsid(1)`, so this form is
  Linux-only.

## How it works

```
proc-compose.yml
      │
      ▼
  proc-compose up
      │
      ├── platform shell "npm run dev"         → frontend    │ http://localhost:5173
      ├── platform shell "go run ."            → backend     │ http://localhost:3001
      └── platform shell "merge-port ..."      → merge-port  │ http://localhost:8080  ← merge: section
      │
   Ctrl+C → context cancellation → SIGTERM → SIGKILL (5s) → all processes stop
```

The two tools compose a layered stack:

- **proc-compose** — orchestrates all processes, handles logs, restarts, and shutdown
- **merge-port** — auto-injected from the `merge:` config section; routes `/api` → backend, `/*` → frontend on a single port

Each process runs through the platform shell in its own process group so child trees (npm → node, nodemon → ts-node, etc.) terminate cleanly. Linux/macOS use `sh -c`; Windows uses `cmd /c`. Names are sorted alphabetically for deterministic color assignment.
