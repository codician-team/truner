#!/bin/sh
# Installs the truner CLI.
#   curl -fsSL https://raw.githubusercontent.com/codician-team/truner/main/install.sh | sh
# Options (environment): TRUNER_VERSION=v0.1.0  TRUNER_INSTALL_DIR=/usr/local/bin  TRUNER_REPO=owner/name
set -eu

REPO="${TRUNER_REPO:-codician-team/truner}"
VERSION="${TRUNER_VERSION:-latest}"

os=$(uname -s | tr '[:upper:]' '[:lower:]')
case "$os" in
  linux|darwin) ;;
  *) echo "unsupported OS: $os (use install.ps1 on Windows)" >&2; exit 1 ;;
esac
arch=$(uname -m)
case "$arch" in
  x86_64|amd64) arch=amd64 ;;
  aarch64|arm64) arch=arm64 ;;
  *) echo "unsupported architecture: $arch" >&2; exit 1 ;;
esac

if [ "$VERSION" = "latest" ]; then
  base="https://github.com/$REPO/releases/latest/download"
else
  base="https://github.com/$REPO/releases/download/$VERSION"
fi
bin="truner_${os}_${arch}"

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
echo "Downloading $bin ($VERSION)..."
curl -fsSL "$base/$bin" -o "$tmp/truner"
curl -fsSL "$base/checksums.txt" -o "$tmp/checksums.txt"
want=$(grep " $bin\$" "$tmp/checksums.txt" | cut -d' ' -f1)
if command -v sha256sum >/dev/null 2>&1; then
  got=$(sha256sum "$tmp/truner" | cut -d' ' -f1)
else
  got=$(shasum -a 256 "$tmp/truner" | cut -d' ' -f1)
fi
if [ -z "$want" ] || [ "$want" != "$got" ]; then
  echo "checksum mismatch for $bin" >&2
  exit 1
fi
chmod +x "$tmp/truner"

dir="${TRUNER_INSTALL_DIR:-}"
if [ -z "$dir" ]; then
  if [ -w /usr/local/bin ]; then dir=/usr/local/bin; else dir="$HOME/.local/bin"; fi
fi
mkdir -p "$dir"
mv "$tmp/truner" "$dir/truner"
echo "Installed $("$dir/truner" version) to $dir/truner"
case ":$PATH:" in
  *":$dir:"*)
    # Another truner earlier on the PATH would run instead, with its version.
    found=$(command -v truner || true)
    if [ -n "$found" ] && [ "$found" != "$dir/truner" ]; then
      echo "Warning: \`truner\` runs $found ($("$found" version 2>/dev/null || echo "unknown version")), which comes first on your PATH." >&2
      echo "Remove it, or put $dir before $(dirname "$found") in your PATH." >&2
    fi
    ;;
  *) echo "Add $dir to your PATH to use it." ;;
esac
echo "Next: truner install, or truner upgrade for an existing installation"
