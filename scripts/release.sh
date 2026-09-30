#!/usr/bin/env bash
# Cross-compiles dg, dg-engine and demo-app and packages one archive per platform.
#
#   VERSION=v0.2.0 scripts/release.sh      → dist/iddc_v0.2.0_<os>_<arch>.tar.gz (+ .zip on Windows)
#                                             dist/checksums.txt
# Used by .github/workflows/release.yml; also runs locally (make release-local).
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"
VERSION="${VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}"
COMMIT="$(git rev-parse --short HEAD 2>/dev/null || echo unknown)"
DATE="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
PKG=github.com/SAQLAINAP/Intent-Driven-Degradation-Contract/internal/version
LDFLAGS="-s -w -X $PKG.Version=$VERSION -X $PKG.Commit=$COMMIT -X $PKG.Date=$DATE"
PLATFORMS="${PLATFORMS:-linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64}"

rm -rf dist && mkdir -p dist
for platform in $PLATFORMS; do
  os="${platform%/*}" arch="${platform#*/}" ext=""
  [ "$os" = windows ] && ext=".exe"
  name="iddc_${VERSION}_${os}_${arch}"
  stage="dist/$name"
  mkdir -p "$stage/config" "$stage/examples"
  for cmd in dg dg-engine demo-app; do
    CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" go build -trimpath -ldflags "$LDFLAGS" -o "$stage/$cmd$ext" "./cmd/$cmd"
  done
  cp LICENSE README.md CHANGELOG.md "$stage/"
  cp config/*.yaml "$stage/config/"
  cp -R examples/demo-set "$stage/examples/"
  if [ "$os" = windows ]; then
    (cd dist && zip -qr "$name.zip" "$name")
  else
    tar -C dist -czf "dist/$name.tar.gz" "$name"
  fi
  rm -rf "$stage"
  echo "built $name"
done

sums="$(cd dist && { command -v sha256sum >/dev/null && sha256sum ./* || shasum -a 256 ./*; } | sed 's#\./##')"
printf '%s\n' "$sums" > dist/checksums.txt
echo "checksums → dist/checksums.txt"
