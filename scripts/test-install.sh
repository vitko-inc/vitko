#!/bin/sh
# Test install.sh against a local snapshot build (goreleaser release --snapshot).
# Usage: scripts/test-install.sh [dist-dir]
set -eu
dist="${1:-dist}"
root=$(mktemp -d)
trap 'rm -rf "$root"' EXIT
mkdir -p "$root/download/v0.0.0"
cp "$dist"/*.tar.gz "$dist/checksums.txt" "$root/download/v0.0.0/"

out=$(VITKO_DOWNLOAD_URL="file://$root" VITKO_VERSION=v0.0.0 VITKO_INSTALL_DIR="$root/bin" VITKO_VERIFY=never VITKO_OUTPUT=json sh ./install.sh)
echo "$out"
echo "$out" | grep -q '"schema":"vitko.install/v1"'
echo "$out" | grep -q '"checksum_verified":true'
"$root/bin/vitko" version --output json | grep -q '"schema": "vitko.version/v1"'

# A tampered archive must be refused.
os=$(uname -s | tr '[:upper:]' '[:lower:]')
arch=$(uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/')
printf 'tampered' >>"$root/download/v0.0.0/vitko_${os}_${arch}.tar.gz"
if VITKO_DOWNLOAD_URL="file://$root" VITKO_VERSION=v0.0.0 VITKO_INSTALL_DIR="$root/bin2" VITKO_VERIFY=never sh ./install.sh 2>/dev/null; then
	echo "install.sh accepted a tampered archive" >&2
	exit 1
fi
echo "install.sh: ok"
