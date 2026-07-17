package usage

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	_ "github.com/duckdb/duckdb-go/v2"
)

// Phase 0 技术门禁:验证固定版本 driver 能 open/migrate/insert/update/query/close。
func TestDuckDBSmokeOpenMigrateCRUD(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "smoke.duckdb")

	db, err := sql.Open("duckdb", dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	if err := db.Ping(); err != nil {
		t.Fatalf("ping: %v", err)
	}

	// 资源边界设置(与方案一致的安全默认)。
	for _, stmt := range []string{
		"SET memory_limit = '256MB'",
		"SET threads = 2",
		"SET enable_external_access = false",
		"SET autoinstall_known_extensions = false",
		"SET autoload_known_extensions = false",
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("set %q: %v", stmt, err)
		}
	}

	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := tx.Exec(`
CREATE TABLE IF NOT EXISTS schema_migrations (
    version     INTEGER PRIMARY KEY,
    name        VARCHAR NOT NULL,
    applied_at  TIMESTAMPTZ NOT NULL
)`); err != nil {
		_ = tx.Rollback()
		t.Fatalf("create migrations: %v", err)
	}
	if _, err := tx.Exec(`
CREATE TABLE IF NOT EXISTS usage_events (
    event_id     VARCHAR PRIMARY KEY,
    started_at   TIMESTAMPTZ NOT NULL,
    usage_date   DATE NOT NULL,
    api_key_id   VARCHAR NOT NULL,
    input_tokens BIGINT NOT NULL DEFAULT 0,
    output_tokens BIGINT NOT NULL DEFAULT 0,
    total_tokens BIGINT NOT NULL DEFAULT 0,
    state        VARCHAR NOT NULL,
    CHECK (state IN ('started', 'completed'))
)`); err != nil {
		_ = tx.Rollback()
		t.Fatalf("create usage_events: %v", err)
	}
	if _, err := tx.Exec(
		`INSERT INTO schema_migrations(version, name, applied_at) VALUES (?, ?, ?)`,
		1, "init", time.Now().UTC(),
	); err != nil {
		_ = tx.Rollback()
		t.Fatalf("insert migration: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit migrate: %v", err)
	}

	started := time.Now().UTC()
	if _, err := db.Exec(
		`INSERT INTO usage_events(event_id, started_at, usage_date, api_key_id, state)
		 VALUES (?, ?, ?, ?, 'started')`,
		"evt-1", started, started.Format("2006-01-02"), "default",
	); err != nil {
		t.Fatalf("insert started: %v", err)
	}

	res, err := db.Exec(
		`UPDATE usage_events
		 SET input_tokens = ?, output_tokens = ?, total_tokens = ?, state = 'completed'
		 WHERE event_id = ? AND state = 'started'`,
		10, 5, 15, "evt-1",
	)
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		t.Fatalf("rows affected: %v", err)
	}
	if n != 1 {
		t.Fatalf("rows affected = %d, want 1", n)
	}

	var total int64
	var keyID string
	if err := db.QueryRow(
		`SELECT total_tokens, api_key_id FROM usage_events WHERE event_id = ?`,
		"evt-1",
	).Scan(&total, &keyID); err != nil {
		t.Fatalf("query: %v", err)
	}
	if total != 15 || keyID != "default" {
		t.Fatalf("got total=%d key=%q", total, keyID)
	}

	// 重复 event_id 必须失败。
	if _, err := db.Exec(
		`INSERT INTO usage_events(event_id, started_at, usage_date, api_key_id, state)
		 VALUES (?, ?, ?, ?, 'started')`,
		"evt-1", started, started.Format("2006-01-02"), "default",
	); err == nil {
		t.Fatal("expected duplicate event_id to fail")
	}

	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// 重启后数据仍在。
	db2, err := sql.Open("duckdb", dbPath)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer db2.Close()
	var count int
	if err := db2.QueryRow(`SELECT count(*) FROM usage_events`).Scan(&count); err != nil {
		t.Fatalf("recount: %v", err)
	}
	if count != 1 {
		t.Fatalf("count after reopen = %d", count)
	}
}
