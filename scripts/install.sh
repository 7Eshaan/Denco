#!/bin/sh
# Installs Denco on macOS or Linux (DECISIONS D67):
#
#   curl -fsSL https://raw.githubusercontent.com/7Eshaan/Denco/main/scripts/install.sh | sh
#
# It downloads the latest release for this OS and CPU from GitHub, checks
# it against the release's SHA256SUMS, and runs `denco setup`, which
# installs it to ~/.local/bin and puts that on your PATH (no password).
# Read it first if you like: it does nothing else.
#
# DENCO_VERSION=v0.3.1 picks a version instead of the latest.
# (DENCO_REPO_URL points elsewhere; the tests use it.)
#
# Keep this file ASCII: in UTF-8 locales macOS's sh counts the bytes of
# characters like an ellipsis as letters, so "$arch" followed by one was
# read as a longer variable name (CI caught it).
set -eu

repo=${DENCO_REPO_URL:-https://github.com/7Eshaan/Denco}

fail() {
	echo "denco install: $*" >&2
	exit 1
}

case $(uname -s) in
Darwin) os=darwin ;;
Linux) os=linux ;;
*) fail "this script is for macOS and Linux; on Windows use install.ps1 (see the README)" ;;
esac
case $(uname -m) in
x86_64 | amd64) arch=amd64 ;;
arm64 | aarch64) arch=arm64 ;;
*) fail "no prebuilt Denco for $(uname -m) CPUs; build it from source (see the README)" ;;
esac

if command -v curl >/dev/null 2>&1; then
	fetch() { curl -fsSL -o "$2" "$1"; }
	latest() { curl -fsSL -o /dev/null -w '%{url_effective}' "$1"; }
elif command -v wget >/dev/null 2>&1; then
	fetch() { wget -q -O "$2" "$1"; }
	latest() { wget -q -O /dev/null --server-response "$1" 2>&1 | sed -n 's/^ *[Ll]ocation: *//p' | tail -n 1 | tr -d '\r'; }
else
	fail "needs curl or wget"
fi

if command -v sha256sum >/dev/null 2>&1; then
	sha256() { sha256sum "$1" | cut -d ' ' -f 1; }
elif command -v shasum >/dev/null 2>&1; then
	sha256() { shasum -a 256 "$1" | cut -d ' ' -f 1; }
else
	fail "needs sha256sum or shasum to check the download"
fi

# The latest version: /releases/latest redirects to /releases/tag/<tag>.
# (No GitHub API: it limits requests per address.)
version=${DENCO_VERSION:-}
if [ -z "$version" ]; then
	url=$(latest "$repo/releases/latest") || fail "couldn't reach $repo"
	version=${url##*/}
	case $version in
	v[0-9]*) ;;
	*) fail "couldn't find the latest release (got $url)" ;;
	esac
fi

name="denco_${version}_${os}_${arch}"
tmp=$(mktemp -d 2>/dev/null || mktemp -d -t denco)
# Clean up on the way out. Any exit before the end is a failure: after
# some errors (an unbound variable) macOS's bash 3.2 exits 0 and even $?
# in this trap is 0, so success is marked explicitly instead.
done=
trap 'rm -rf "$tmp"; [ -n "$done" ] || exit 1' EXIT
trap 'exit 1' INT TERM

echo "Downloading Denco ${version} for ${os}/${arch}..."
fetch "$repo/releases/download/$version/$name.tar.gz" "$tmp/$name.tar.gz" ||
	fail "download failed: $repo/releases/download/$version/$name.tar.gz"
fetch "$repo/releases/download/$version/SHA256SUMS" "$tmp/SHA256SUMS" ||
	fail "download failed: SHA256SUMS"

want=$(awk -v f="$name.tar.gz" '$2 == f || $2 == "*" f { print $1 }' "$tmp/SHA256SUMS")
[ -n "$want" ] || fail "$name.tar.gz isn't listed in SHA256SUMS"
got=$(sha256 "$tmp/$name.tar.gz")
[ "$got" = "$want" ] || fail "checksum mismatch for $name.tar.gz (expected $want, got $got): not installing"
echo "Checksum OK."

tar -xzf "$tmp/$name.tar.gz" -C "$tmp" || fail "couldn't unpack $name.tar.gz"
[ -x "$tmp/$name/denco" ] || fail "no denco in $name.tar.gz"

# denco setup copies it to ~/.local/bin and sets up PATH. Downloaded with
# curl/wget it carries no macOS quarantine flag, so Gatekeeper doesn't
# block it.
"$tmp/$name/denco" setup </dev/null ||
	fail "denco setup failed (Denco $version may be older than 'denco setup': use v0.3.1 or newer)"
done=1
