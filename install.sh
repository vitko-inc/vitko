#!/bin/sh
# Install the vitko command-line tool.
#
#   curl -fsSL https://github.com/vitko-inc/vitko/releases/latest/download/install.sh | sh
#
# Non-interactive. Settings (environment variables):
#   VITKO_VERSION       version to install, for example v0.1.0 (default: latest)
#   VITKO_INSTALL_DIR   where to put the binary (default: ~/.local/bin)
#   VITKO_VERIFY        signature check with cosign: auto (default; when cosign
#                       is installed), always, or never. The SHA-256 checksum is
#                       always checked.
#   VITKO_OUTPUT        json: print the result as one JSON document on stdout
#
# Exit codes: 0 installed, 1 failed, 2 unsupported platform or bad setting.
set -eu

REPO="vitko-inc/vitko"
VERSION="${VITKO_VERSION:-latest}"
INSTALL_DIR="${VITKO_INSTALL_DIR:-$HOME/.local/bin}"
VERIFY="${VITKO_VERIFY:-auto}"
OUTPUT="${VITKO_OUTPUT:-text}"
BASE_URL="${VITKO_DOWNLOAD_URL:-https://github.com/$REPO/releases}"

log() { if [ "$OUTPUT" != "json" ]; then printf '%s\n' "$*" >&2; fi; }

fail() {
	code="$1"
	msg="$2"
	if [ "$OUTPUT" = "json" ]; then
		esc=$(printf '%s' "$msg" | sed 's/\\/\\\\/g; s/"/\\"/g')
		printf '{"schema":"vitko.error/v1","error":{"code":"%s","message":"%s","retryable":false}}\n' "$3" "$esc" >&2
	else
		printf 'vitko install: %s\n' "$msg" >&2
	fi
	exit "$code"
}

case "$VERIFY" in auto | always | never) ;; *) fail 2 "VITKO_VERIFY must be auto, always or never." usage ;; esac

os=$(uname -s)
case "$os" in
Linux) os=linux ;;
Darwin) os=darwin ;;
*) fail 2 "Unsupported operating system: $os. vitko runs on Linux and macOS." usage ;;
esac
arch=$(uname -m)
case "$arch" in
x86_64 | amd64) arch=amd64 ;;
arm64 | aarch64) arch=arm64 ;;
*) fail 2 "Unsupported CPU: $arch. vitko runs on x86-64 and arm64." usage ;;
esac

if [ "$VERSION" = "latest" ]; then
	url="$BASE_URL/latest/download"
else
	case "$VERSION" in v*) ;; *) VERSION="v$VERSION" ;; esac
	url="$BASE_URL/download/$VERSION"
fi

if command -v curl >/dev/null 2>&1; then
	fetch() { curl -fsSL --retry 3 -o "$2" "$1"; }
elif command -v wget >/dev/null 2>&1; then
	fetch() { wget -q -O "$2" "$1"; }
else
	fail 2 "Needs curl or wget." input_required
fi

if command -v sha256sum >/dev/null 2>&1; then
	sha256() { sha256sum "$1" | cut -d' ' -f1; }
elif command -v shasum >/dev/null 2>&1; then
	sha256() { shasum -a 256 "$1" | cut -d' ' -f1; }
else
	fail 2 "Needs sha256sum or shasum to check the download." input_required
fi

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT INT TERM
archive="vitko_${os}_${arch}.tar.gz"

log "Downloading vitko ($VERSION, $os/$arch)..."
fetch "$url/$archive" "$tmp/$archive" || fail 1 "Couldn't download $url/$archive." github_unavailable
fetch "$url/checksums.txt" "$tmp/checksums.txt" || fail 1 "Couldn't download $url/checksums.txt." github_unavailable

want=$(awk -v f="$archive" '$2 == f { print $1 }' "$tmp/checksums.txt")
[ -n "$want" ] || fail 1 "checksums.txt has no entry for $archive." invalid_input
got=$(sha256 "$tmp/$archive")
[ "$want" = "$got" ] || fail 1 "Checksum mismatch for $archive. The download may be corrupt or tampered with." invalid_input

signature=false
if [ "$VERIFY" != "never" ]; then
	if command -v cosign >/dev/null 2>&1; then
		fetch "$url/checksums.txt.sigstore.json" "$tmp/checksums.txt.sigstore.json" ||
			fail 1 "Couldn't download the signature." github_unavailable
		cosign verify-blob \
			--bundle "$tmp/checksums.txt.sigstore.json" \
			--certificate-identity-regexp "^https://github.com/$REPO/\.github/workflows/release\.yml@refs/tags/v" \
			--certificate-oidc-issuer "https://token.actions.githubusercontent.com" \
			"$tmp/checksums.txt" >/dev/null 2>&1 ||
			fail 1 "The signature of checksums.txt doesn't verify. Not installing." invalid_input
		signature=true
	elif [ "$VERIFY" = "always" ]; then
		fail 2 "VITKO_VERIFY=always needs cosign (https://docs.sigstore.dev)." input_required
	fi
fi

tar -xzf "$tmp/$archive" -C "$tmp" vitko
mkdir -p "$INSTALL_DIR"
mv "$tmp/vitko" "$INSTALL_DIR/vitko.new"
chmod 0755 "$INSTALL_DIR/vitko.new"
mv "$INSTALL_DIR/vitko.new" "$INSTALL_DIR/vitko"

installed=$("$INSTALL_DIR/vitko" version --output json | sed -n 's/.*"version": *"\([^"]*\)".*/\1/p' | head -n 1)

on_path=false
case ":$PATH:" in *":$INSTALL_DIR:"*) on_path=true ;; esac

if [ "$OUTPUT" = "json" ]; then
	printf '{"schema":"vitko.install/v1","version":"%s","path":"%s","os":"%s","arch":"%s","checksum_verified":true,"signature_verified":%s,"on_path":%s}\n' \
		"$installed" "$INSTALL_DIR/vitko" "$os" "$arch" "$signature" "$on_path"
else
	log "Installed vitko $installed to $INSTALL_DIR/vitko (checksum verified; signature verified: $signature)."
	if [ "$on_path" = false ]; then
		log "Add $INSTALL_DIR to your PATH, for example: export PATH=\"$INSTALL_DIR:\$PATH\""
	fi
	log "Try: vitko runners pricing"
fi
