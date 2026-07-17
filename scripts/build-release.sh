#!/usr/bin/env bash
# 在与目标一致的原生平台构建可发布归档。DuckDB Go bindings 不支持从
# linux/amd64 可靠交叉编译到 linux/arm64，因此脚本明确拒绝跨平台构建。
set -euo pipefail

VERSION="${1:-${VERSION:-}}"
OUT_DIR="${2:-dist}"
GO_CMD="${GO:-go}"

if [[ -z "$VERSION" ]]; then
  echo "usage: VERSION=vX.Y.Z $0 [version] [output-dir]" >&2
  exit 2
fi
if [[ ! "$VERSION" =~ ^v?[0-9]+\.[0-9]+\.[0-9]+([-.][0-9A-Za-z.-]+)?$ ]]; then
  echo "invalid version: $VERSION (expected vX.Y.Z)" >&2
  exit 2
fi

VERSION="${VERSION#v}"
GOOS="${GOOS:-$($GO_CMD env GOHOSTOS)}"
GOARCH="${GOARCH:-$($GO_CMD env GOHOSTARCH)}"
HOST_OS="$($GO_CMD env GOHOSTOS)"
HOST_ARCH="$($GO_CMD env GOHOSTARCH)"

if [[ "$GOOS/$GOARCH" != "$HOST_OS/$HOST_ARCH" ]]; then
  cat >&2 <<EOF
refusing cross build $GOOS/$GOARCH from $HOST_OS/$HOST_ARCH.
The pinned DuckDB bindings must be built on a native target runner. Use the
matching GitHub Actions matrix runner or run this script on that platform.
EOF
  exit 2
fi

name="ai-proxy_${VERSION}_${GOOS}_${GOARCH}"
stage="$OUT_DIR/.stage/$name"
binary="ai-proxy"
if [[ "$GOOS" == "windows" ]]; then
  binary+=".exe"
fi

rm -rf "$stage"
mkdir -p "$stage"
trap 'rm -rf "$stage"' EXIT

GOOS="$GOOS" GOARCH="$GOARCH" "$GO_CMD" build \
  -trimpath -buildvcs=false \
  -ldflags "-s -w -X main.version=v$VERSION" \
  -o "$stage/$binary" ./cmd/ai-proxy

cp README.md config.example.yaml "$stage/"
archive="$OUT_DIR/$name.tar.gz"
mkdir -p "$OUT_DIR"
tar -C "$(dirname "$stage")" -czf "$archive" "$name"

if command -v sha256sum >/dev/null 2>&1; then
  sha256sum "$archive" >"$archive.sha256"
else
  shasum -a 256 "$archive" >"$archive.sha256"
fi

echo "created $archive"
