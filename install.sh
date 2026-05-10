#!/usr/bin/env bash
# proc-compose installer.
#
# Usage:
#   curl -sSL https://raw.githubusercontent.com/anivaryam/proc-compose/main/install.sh | bash
#
# The script downloads the latest release binary from GitHub for the
# current OS/arch and drops it into one of (in order):
#   - $PROC_COMPOSE_INSTALL_DIR (if set)
#   - ~/.local/bin (created if missing)
#
# If no GitHub release matches the current platform but Go is available, it
# falls back to "go install". Set PROC_COMPOSE_VERSION to pin a specific
# version (e.g. PROC_COMPOSE_VERSION=v1.2.3).

set -euo pipefail

REPO="anivaryam/proc-compose"
BIN="proc-compose"
ALIAS="pc"

# tmp is declared at script scope so the EXIT trap below can reference it
# safely under `set -u`, even when the trap fires before main() has had a
# chance to populate it (e.g. resolve_version aborting early).
tmp=""
trap '[ -n "$tmp" ] && rm -rf "$tmp"' EXIT

err()  { printf 'error: %s\n' "$*" >&2; exit 1; }
note() { printf '%s\n' "$*"; }

detect_platform() {
  local os arch
  os="$(uname -s | tr '[:upper:]' '[:lower:]')"
  arch="$(uname -m)"
  case "$arch" in
    x86_64|amd64) arch=amd64 ;;
    aarch64|arm64) arch=arm64 ;;
    *) err "unsupported architecture: $arch" ;;
  esac
  case "$os" in
    linux|darwin) ;;
    *) err "unsupported OS: $os (run 'go install github.com/$REPO/cmd/proc-compose@latest' instead)" ;;
  esac
  printf '%s_%s' "$os" "$arch"
}

resolve_install_dir() {
  if [ -n "${PROC_COMPOSE_INSTALL_DIR:-}" ]; then
    printf '%s' "$PROC_COMPOSE_INSTALL_DIR"
    return
  fi
  printf '%s' "$HOME/.local/bin"
}

resolve_version() {
  if [ -n "${PROC_COMPOSE_VERSION:-}" ]; then
    printf '%s' "$PROC_COMPOSE_VERSION"
    return
  fi
  # Resolve "latest" via the redirect on /releases/latest. We avoid the JSON
  # API to skip the rate-limited path; the redirect endpoint is unauthenticated.
  local url
  url="$(curl -fsSL -o /dev/null -w '%{url_effective}' "https://github.com/$REPO/releases/latest")"
  printf '%s' "${url##*/}"
}

fallback_go_install() {
  if command -v go >/dev/null 2>&1; then
    note "no release matched this platform — falling back to 'go install'"
    GOBIN="$(resolve_install_dir)" go install "github.com/$REPO/cmd/proc-compose@latest"
    return 0
  fi
  err "no release matched this platform and 'go' is not on PATH"
}

main() {
  local platform version url tmp install_dir
  platform="$(detect_platform)"
  install_dir="$(resolve_install_dir)"
  mkdir -p "$install_dir"

  version="$(resolve_version)"
  if [ -z "$version" ] || [ "$version" = "releases" ]; then
    fallback_go_install
    return
  fi

  url="https://github.com/$REPO/releases/download/$version/proc-compose_${platform}.tar.gz"
  tmp="$(mktemp -d)"  # cleanup handled by the EXIT trap registered at script scope

  note "Downloading $BIN $version for $platform"
  if ! curl -fsSL "$url" -o "$tmp/$BIN.tar.gz"; then
    fallback_go_install
    return
  fi

  tar -xzf "$tmp/$BIN.tar.gz" -C "$tmp"
  install -m 0755 "$tmp/$BIN" "$install_dir/$BIN"
  note "Installed: $install_dir/$BIN"

  # Convenience alias `pc` — only create if it doesn't already exist.
  if [ ! -e "$install_dir/$ALIAS" ]; then
    ln -s "$BIN" "$install_dir/$ALIAS"
    note "Created symlink: $install_dir/$ALIAS -> $BIN"
  fi

  case ":$PATH:" in
    *":$install_dir:"*) ;;
    *)
      note ""
      note "NOTE: $install_dir is not on your PATH. Add this to your shell rc:"
      note "  export PATH=\"$install_dir:\$PATH\""
      ;;
  esac
}

main "$@"
