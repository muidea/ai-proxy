package usage

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

const currentSchemaVersion = 2

// migrate 在事务内应用尚未执行的 schema 版本;若库中存在更高未知版本则 fail-fast。
func migrate(ctx context.Context, db *sql.DB) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin migration: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS schema_migrations (
    version     INTEGER PRIMARY KEY,
    name        VARCHAR NOT NULL,
    applied_at  TIMESTAMPTZ NOT NULL
)`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	var maxVersion sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT max(version) FROM schema_migrations`).Scan(&maxVersion); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	applied := int64(0)
	if maxVersion.Valid {
		applied = maxVersion.Int64
	}
	if applied > currentSchemaVersion {
		return fmt.Errorf("usage store schema version %d is newer than supported %d", applied, currentSchemaVersion)
	}

	if applied < 1 {
		if err := applyV1(ctx, tx); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO schema_migrations(version, name, applied_at) VALUES (?, ?, ?)`,
			1, "usage_events_v1", time.Now().UTC(),
		); err != nil {
			return fmt.Errorf("record migration v1: %w", err)
		}
	}
	if applied < 2 {
		if err := applyV2(ctx, tx); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO schema_migrations(version, name, applied_at) VALUES (?, ?, ?)`,
			2, "usage_events_completed_fields_v2", time.Now().UTC(),
		); err != nil {
			return fmt.Errorf("record migration v2: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit migration: %w", err)
	}
	return nil
}

func applyV1(ctx context.Context, tx *sql.Tx) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS usage_events (
    event_id                    VARCHAR PRIMARY KEY,
    round_id                    BIGINT,
    started_at                  TIMESTAMPTZ NOT NULL,
    completed_at                TIMESTAMPTZ,
    usage_date                  DATE NOT NULL,

    api_key_id                  VARCHAR NOT NULL,
    provider                    VARCHAR,
    model                       VARCHAR,
    operation                   VARCHAR,
    route                       VARCHAR,
    client_endpoint             VARCHAR,
    client_protocol             VARCHAR,
    upstream_protocol           VARCHAR,
    upstream_endpoint           VARCHAR,
    conversion_mode             VARCHAR,

    input_tokens                BIGINT NOT NULL DEFAULT 0,
    output_tokens               BIGINT NOT NULL DEFAULT 0,
    total_tokens                BIGINT NOT NULL DEFAULT 0,
    cached_input_tokens         BIGINT NOT NULL DEFAULT 0,
    cache_creation_input_tokens BIGINT NOT NULL DEFAULT 0,

    http_status                 INTEGER,
    outcome                     VARCHAR,
    error_code                  VARCHAR,
    duration_ms                 BIGINT,
    upstream_duration_ms        BIGINT,
    stream                      BOOLEAN NOT NULL DEFAULT FALSE,
    estimated                   BOOLEAN NOT NULL DEFAULT FALSE,
    state                       VARCHAR NOT NULL,

    CHECK (state IN ('started', 'completed')),
    CHECK (state <> 'completed' OR (completed_at IS NOT NULL AND http_status IS NOT NULL AND outcome IS NOT NULL AND length(outcome) > 0)),
    CHECK (input_tokens >= 0),
    CHECK (output_tokens >= 0),
    CHECK (total_tokens >= 0),
    CHECK (cached_input_tokens >= 0),
    CHECK (cache_creation_input_tokens >= 0)
)`,
		`CREATE INDEX IF NOT EXISTS idx_usage_events_started_at ON usage_events(started_at)`,
		`CREATE INDEX IF NOT EXISTS idx_usage_events_key_time ON usage_events(api_key_id, started_at)`,
		`CREATE INDEX IF NOT EXISTS idx_usage_events_date_key ON usage_events(usage_date, api_key_id)`,
		`CREATE INDEX IF NOT EXISTS idx_usage_events_provider_model ON usage_events(provider, model)`,
	}
	for _, s := range stmts {
		if _, err := tx.ExecContext(ctx, s); err != nil {
			return fmt.Errorf("apply v1: %w", err)
		}
	}
	return nil
}

func applyV2(ctx context.Context, tx *sql.Tx) error {
	// DuckDB 尚不支持 ALTER TABLE ADD CONSTRAINT，因此事务内重建表。旧 started
	// 行在迁移时按崩溃恢复规则结算，确保新约束对每条 completed 行成立。
	if _, err := tx.ExecContext(ctx, `CREATE TABLE usage_events_v2 (
    event_id VARCHAR PRIMARY KEY, round_id BIGINT, started_at TIMESTAMPTZ NOT NULL,
    completed_at TIMESTAMPTZ, usage_date DATE NOT NULL, api_key_id VARCHAR NOT NULL,
    provider VARCHAR, model VARCHAR, operation VARCHAR, route VARCHAR,
    client_endpoint VARCHAR, client_protocol VARCHAR, upstream_protocol VARCHAR,
    upstream_endpoint VARCHAR, conversion_mode VARCHAR,
    input_tokens BIGINT NOT NULL DEFAULT 0, output_tokens BIGINT NOT NULL DEFAULT 0,
    total_tokens BIGINT NOT NULL DEFAULT 0, cached_input_tokens BIGINT NOT NULL DEFAULT 0,
    cache_creation_input_tokens BIGINT NOT NULL DEFAULT 0, http_status INTEGER,
    outcome VARCHAR, error_code VARCHAR, duration_ms BIGINT, upstream_duration_ms BIGINT,
    stream BOOLEAN NOT NULL DEFAULT FALSE, estimated BOOLEAN NOT NULL DEFAULT FALSE,
    state VARCHAR NOT NULL,
    CHECK (state IN ('started', 'completed')),
    CHECK (state <> 'completed' OR (completed_at IS NOT NULL AND http_status IS NOT NULL AND outcome IS NOT NULL AND length(outcome) > 0)),
    CHECK (input_tokens >= 0), CHECK (output_tokens >= 0), CHECK (total_tokens >= 0),
    CHECK (cached_input_tokens >= 0), CHECK (cache_creation_input_tokens >= 0)
)`); err != nil {
		return fmt.Errorf("create v2 usage events: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO usage_events_v2
SELECT event_id, round_id, started_at,
       CASE WHEN state = 'started' THEN now() ELSE completed_at END,
       usage_date, api_key_id, provider, model, operation, route, client_endpoint,
       client_protocol, upstream_protocol, upstream_endpoint, conversion_mode,
       input_tokens, output_tokens, total_tokens, cached_input_tokens,
       cache_creation_input_tokens,
       CASE WHEN state = 'started' THEN 500 ELSE http_status END,
       CASE WHEN state = 'started' THEN 'process_interrupted' ELSE outcome END,
       CASE WHEN state = 'started' THEN 'process_interrupted' ELSE error_code END,
       duration_ms, upstream_duration_ms, stream, estimated,
       CASE WHEN state = 'started' THEN 'completed' ELSE state END
FROM usage_events`); err != nil {
		return fmt.Errorf("copy v2 usage events: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DROP TABLE usage_events`); err != nil {
		return fmt.Errorf("drop v1 usage events: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `ALTER TABLE usage_events_v2 RENAME TO usage_events`); err != nil {
		return fmt.Errorf("rename v2 usage events: %w", err)
	}
	for _, stmt := range []string{
		`CREATE INDEX idx_usage_events_started_at ON usage_events(started_at)`,
		`CREATE INDEX idx_usage_events_key_time ON usage_events(api_key_id, started_at)`,
		`CREATE INDEX idx_usage_events_date_key ON usage_events(usage_date, api_key_id)`,
		`CREATE INDEX idx_usage_events_provider_model ON usage_events(provider, model)`,
	} {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("create v2 index: %w", err)
		}
	}
	return nil
}
