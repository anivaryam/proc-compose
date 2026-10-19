# Contributing

Bug reports, fixes, and improvements are welcome.

## Reporting issues

Open an issue at <https://github.com/anivaryam/proc-compose/issues> with:

- the output of `proc-compose --version` and your operating system
- a minimal `proc-compose.yml` that reproduces the problem
- what you expected and what happened instead

## Development setup

You need Go (see `go.mod`; releases are built with Go 1.26.9) and `make`.

```sh
git clone https://github.com/anivaryam/proc-compose.git
cd proc-compose
make build      # builds bin/proc-compose
make test       # go test ./... -race -count=1
make install    # copies the binary to ~/.local/bin/
```

## Checks

CI runs these on every pull request. Run them locally before opening one:

```sh
go test ./... -race -count=1
golangci-lint run
```

Tests run on Linux, macOS, and Windows in CI. If a change touches process
management, signals, IPC, or paths, consider each platform; see
[Platform support](docs/platforms.md).

## Pull requests

- Keep each pull request focused on one change.
- Add or update tests for any behavior change.
- Update the relevant page under `docs/` and add an entry under
  `[Unreleased]` in [CHANGELOG.md](CHANGELOG.md).
- When you add, rename, or remove a command or config field, update the agent
  skill in `skills/proc-compose`. The `TestSkill_` tests fail until you do.
- Use [Conventional Commits](https://www.conventionalcommits.org/) for commit
  messages, for example `fix: report escaped descendants on stop`.

## Releases

Pushing a `v*` tag builds binaries for all platforms and publishes a GitHub
release. Before tagging, move the `[Unreleased]` changelog entries under the
new version.
