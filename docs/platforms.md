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

Shell commands in configs use `cmd /c`. Use cmd environment syntax such as `echo %PORT%`. If you want PowerShell syntax, make it explicit in the command text: `powershell -NoProfile -Command "Write-Output $env:PORT"`. POSIX syntax such as `$VAR`, `$(...)`, `trap`, `export`, and shell loops is not promised to be portable. Existing `$VAR` translation is best-effort compatibility only. Paths with spaces need quoting, and Windows-native commands may need backslashes. POSIX examples elsewhere in these docs are Linux/macOS examples unless labelled otherwise. The `init --template minimal` on Windows generates a PowerShell-based template.

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
