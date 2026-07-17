#!/usr/bin/env bash
# 仅检查受 Git 管理的 Go 源文件，避免平台相关的目录遍历和未跟踪文件干扰。
set -euo pipefail

unformatted="$({
  while IFS= read -r -d '' file; do
    # 暂存删除后，索引仍会列出文件；只检查工作区中实际存在的源文件。
    [[ -f "$file" ]] && gofmt -l "$file"
  done < <(git ls-files -z -- '*.go')
})"
if [[ -n "$unformatted" ]]; then
  printf '%s\n' "Go files need gofmt:" >&2
  printf '%s\n' "$unformatted" >&2
  exit 1
fi
