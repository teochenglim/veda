#!/bin/sh
# Veda installer — wraps the GitHub release archives that CI publishes.
#
#   curl -fsSL https://raw.githubusercontent.com/teochenglim/veda/main/install.sh | sh
#   sh install.sh --version v0.8.0 --dir ~/.local/bin
#
# Env overrides: VEDA_VERSION, VEDA_INSTALL_DIR, VEDA_REPO.
# Windows is served by Scoop (packaging/scoop/), not this script.
set -eu

REPO="${VEDA_REPO:-teochenglim/veda}"
VERSION="${VEDA_VERSION:-}"
DEST="${VEDA_INSTALL_DIR:-}"

usage() {
	echo "usage: install.sh [--version vX.Y.Z] [--dir ~/.local/bin]"
	exit 2
}

while [ $# -gt 0 ]; do
	case "$1" in
		--version) VERSION="$2"; shift 2 ;;
		--dir) DEST="$2"; shift 2 ;;
		*) usage ;;
	esac
done

http_get() {
	if command -v curl >/dev/null 2>&1; then
		curl -fsSL "$1"
	elif command -v wget >/dev/null 2>&1; then
		wget -qO- "$1"
	else
		echo "veda install: need curl or wget" >&2
		exit 1
	fi
}

download() {
	if command -v curl >/dev/null 2>&1; then
		curl -fsSL "$1" -o "$2"
	elif command -v wget >/dev/null 2>&1; then
		wget -qO "$2" "$1"
	else
		echo "veda install: need curl or wget" >&2
		exit 1
	fi
}

case "$(uname -s)" in
	Darwin) veda_os=darwin ;;
	Linux) veda_os=linux ;;
	*) echo "veda install: $(uname -s) is not covered here — use Scoop on Windows" >&2; exit 1 ;;
esac
case "$(uname -m)" in
	x86_64|amd64) veda_arch=amd64 ;;
	aarch64|arm64) veda_arch=arm64 ;;
	*) echo "veda install: unsupported architecture $(uname -m)" >&2; exit 1 ;;
esac

if [ -z "$VERSION" ]; then
	VERSION=$(http_get "https://api.github.com/repos/$REPO/releases/latest" |
		sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -n 1)
	[ -n "$VERSION" ] || { echo "veda install: could not resolve the latest release" >&2; exit 1; }
fi
case "$VERSION" in v*) ;; *) VERSION="v$VERSION" ;; esac

archive="veda-$VERSION-$veda_os-$veda_arch.tar.gz"
base="https://github.com/$REPO/releases/download/$VERSION"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

echo "veda install: downloading $archive"
download "$base/$archive" "$tmp/$archive"
download "$base/SHA256SUMS" "$tmp/SHA256SUMS"

expected=$(awk -v a="$archive" '$2 == a { print $1 }' "$tmp/SHA256SUMS")
[ -n "$expected" ] || { echo "veda install: $archive missing from SHA256SUMS" >&2; exit 1; }
if command -v sha256sum >/dev/null 2>&1; then
	actual=$(sha256sum "$tmp/$archive" | awk '{print $1}')
else
	actual=$(shasum -a 256 "$tmp/$archive" | awk '{print $1}')
fi
[ "$actual" = "$expected" ] || { echo "veda install: checksum mismatch for $archive" >&2; exit 1; }

tar -xzf "$tmp/$archive" -C "$tmp"

if [ -z "$DEST" ]; then
	DEST=/usr/local/bin
	[ -w "$DEST" ] || DEST="$HOME/.local/bin"
fi
mkdir -p "$DEST"
install -m 0755 "$tmp/veda" "$DEST/veda"
echo "veda install: installed $VERSION ($veda_os/$veda_arch) to $DEST/veda"

case ":$PATH:" in
	*":$DEST:"*) ;;
	*) echo "veda install: note — $DEST is not on your PATH" ;;
esac
"$DEST/veda" version
