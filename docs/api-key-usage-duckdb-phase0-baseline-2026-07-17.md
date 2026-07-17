# DuckDB Phase 0 基线记录

Date: 2026-07-17

- Driver: `github.com/duckdb/duckdb-go/v2@v2.10504.0` (DuckDB 1.5.4)
- Platform: Linux amd64, Go 1.24, CGO_ENABLED=1, gcc 14.2
- Binary size before DuckDB link (cmd/ai-proxy without usage import): ~11 MiB (11462077 bytes)
- Binary size after DuckDB Store integration: ~76 MiB (79698632 bytes)
- `go test ./internal/usage` smoke + full store tests: pass
- `make check` (fmt/vet/test): pass

Live validation remaining: deploy config migration, real client traffic, Web manual acceptance, backup/restore drill.
