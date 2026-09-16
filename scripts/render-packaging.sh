#!/bin/sh
# Renders packaging/veda.rb and packaging/scoop/veda.json from a published
# release's SHA256SUMS asset. Usage: render-packaging.sh x.y.z
set -eu

[ $# -eq 1 ] || { echo "usage: render-packaging.sh x.y.z" >&2; exit 2; }
V="$1"
TAG="v$V"
REPO="${VEDA_REPO:-teochenglim/veda}"
root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
curl -fsSL "https://github.com/$REPO/releases/download/$TAG/SHA256SUMS" -o "$tmp/SHA256SUMS"

sum() {
	awk -v a="$1" '$2 == a { print $1; found = 1 } END { if (!found) exit 1 }' "$tmp/SHA256SUMS" ||
		{ echo "render-packaging: $1 missing from SHA256SUMS" >&2; exit 1; }
}

DARWIN_ARM64_SHA=$(sum "veda-$TAG-darwin-arm64.tar.gz")
DARWIN_AMD64_SHA=$(sum "veda-$TAG-darwin-amd64.tar.gz")
LINUX_AMD64_SHA=$(sum "veda-$TAG-linux-amd64.tar.gz")
LINUX_ARM64_SHA=$(sum "veda-$TAG-linux-arm64.tar.gz")
WINDOWS_AMD64_SHA=$(sum "veda-$TAG-windows-amd64.zip")
WINDOWS_ARM64_SHA=$(sum "veda-$TAG-windows-arm64.zip")

render() {
	sed \
		-e "s/@VERSION@/$V/g" \
		-e "s/@DARWIN_ARM64_SHA@/$DARWIN_ARM64_SHA/g" \
		-e "s/@DARWIN_AMD64_SHA@/$DARWIN_AMD64_SHA/g" \
		-e "s/@LINUX_AMD64_SHA@/$LINUX_AMD64_SHA/g" \
		-e "s/@LINUX_ARM64_SHA@/$LINUX_ARM64_SHA/g" \
		-e "s/@WINDOWS_AMD64_SHA@/$WINDOWS_AMD64_SHA/g" \
		-e "s/@WINDOWS_ARM64_SHA@/$WINDOWS_ARM64_SHA/g" \
		"$1" > "$2"
}

render "$root/packaging/veda.rb.in" "$root/packaging/veda.rb"
render "$root/packaging/scoop/veda.json.in" "$root/packaging/scoop/veda.json"
echo "render-packaging: wrote packaging/veda.rb and packaging/scoop/veda.json for $TAG"
