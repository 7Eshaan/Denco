#!/usr/bin/env bash
# Builds the release archives into dist/: one per OS/architecture, plus
# SHA256SUMS. Used by .github/workflows/release.yml; also runs locally:
#
#   scripts/release-build.sh v0.1.0
set -euo pipefail

version=${1:?usage: scripts/release-build.sh <version, e.g. v0.1.0>}
targets="linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64"

cd "$(dirname "$0")/.."
rm -rf dist
mkdir -p dist

for target in $targets; do
  goos=${target%/*}
  goarch=${target#*/}
  name="denco_${version}_${goos}_${goarch}"
  exe=denco
  [ "$goos" = windows ] && exe=denco.exe

  echo "== $name"
  mkdir -p "dist/$name"
  # Static, reproducible binaries: no cgo, no local paths, no debug info.
  CGO_ENABLED=0 GOOS=$goos GOARCH=$goarch go build -trimpath \
    -ldflags "-s -w -X github.com/7Eshaan/Denco/internal/cli.Version=$version" \
    -o "dist/$name/$exe" ./cmd/denco
  cp README.md LICENSE* "dist/$name/" 2>/dev/null || true

  (
    cd dist
    if [ "$goos" = windows ]; then
      zip -qr "$name.zip" "$name"
    else
      tar -czf "$name.tar.gz" "$name"
    fi
    rm -rf "$name"
  )
done

cd dist
if command -v sha256sum >/dev/null; then
  sha256sum -- *.tar.gz *.zip > SHA256SUMS
else
  shasum -a 256 -- *.tar.gz *.zip > SHA256SUMS # macOS
fi
cat SHA256SUMS
