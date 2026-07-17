#!/usr/bin/env bash
# 本地发布入口：执行完整门禁、原生平台打包；PUBLISH=1 时用 gh 创建 GitHub Release。
set -euo pipefail

VERSION="${1:-${VERSION:-}}"
if [[ -z "$VERSION" ]]; then
  echo "usage: PUBLISH=1 $0 vX.Y.Z" >&2
  exit 2
fi
VERSION="${VERSION#v}"
TAG="v$VERSION"

if [[ ! "$VERSION" =~ ^[0-9]+\.[0-9]+\.[0-9]+([-.][0-9A-Za-z.-]+)?$ ]]; then
  echo "invalid version: $TAG" >&2
  exit 2
fi
if ! git diff --quiet || ! git diff --cached --quiet; then
  echo "refusing release from a dirty worktree" >&2
  exit 2
fi

make check
"$(dirname "$0")/build-release.sh" "$TAG" dist

if [[ "${PUBLISH:-0}" != "1" ]]; then
  echo "local release package is ready in dist/ (set PUBLISH=1 to create $TAG)"
  exit 0
fi
if ! command -v gh >/dev/null 2>&1; then
  echo "gh CLI is required when PUBLISH=1" >&2
  exit 2
fi
if git rev-parse "$TAG" >/dev/null 2>&1; then
  echo "tag $TAG already exists; create releases from the tag workflow instead" >&2
  exit 2
fi

git tag -a "$TAG" -m "Release $TAG"
git push origin "$TAG"
gh release create "$TAG" dist/*.tar.gz dist/*.sha256 --generate-notes --title "$TAG"
