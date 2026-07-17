#!/usr/bin/env bash
# 仅检查受 Git 管理的 Go 源文件，避免平台相关的目录遍历和未跟踪文件干扰。
set -euo pipefail

unformatted="$(git ls-files -z -- '*.go' | xargs -0 gofmt -l)"
if [[ -n "$unformatted" ]]; then
  printf '%s\n' "Go files need gofmt:" >&2
  printf '%s\n' "$unformatted" >&2
  exit 1
fi
