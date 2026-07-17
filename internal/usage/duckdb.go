package usage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"ai-proxy/internal/config"

	_ "github.com/duckdb/duckdb-go/v2"
)

// 稳定内部错误:不向客户端暴露 SQL/路径/密钥。
var (
	ErrDuplicateEvent   = errors.New("duplicate usage event")
	ErrEventNotStarted  = errors.New("usage event not in started state")
	ErrInvalidTokens    = errors.New("invalid token counts")
	ErrStoreClosed      = errors.New("usage store closed")
	ErrStoreUnavailable = errors.New("usage store unavailable")
)

// DuckDBStore 是基于进程内嵌 DuckDB 的用量权威实现。
// 写入经单一 mutex 串行;读取可并发但受连接池限制。
type DuckDBStore struct {
	db     *sql.DB
	path   string
	cfg    config.UsageStoreConfig
	write  sync.Mutex
	closed atomic.Bool
	// healthy 为 1 表示写入路径可用;失败降级为 0,成功写入可恢复。
	healthy   atomic.Int32
	recovered atomic.Int64

	cache *dashboardCache
}

// OpenDuckDB 打开(或创建)DuckDB 文件,应用安全设置,执行 migration,并恢复遗留 started 行。
func OpenDuckDB(cfg config.UsageStoreConfig) (*DuckDBStore, error) {
	path := strings.TrimSpace(cfg.Path)
	if path == "" {
		path = "usage.duckdb"
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("create usage store directory: %w", err)
		}
	}

	db, err := sql.Open("duckdb", path)
	if err != nil {
		return nil, fmt.Errorf("open usage store: %w", err)
	}
	// 单写者 + 有限读并发;避免无界连接。
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(2)
	db.SetConnMaxLifetime(0)

	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping usage store: %w", err)
	}

	// 文件权限尽量收紧为 0600(已存在文件也尝试)。
	_ = os.Chmod(path, 0o600)

	if err := applyRuntimeSettings(db, cfg); err != nil {
		_ = db.Close()
		return nil, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := migrate(ctx, db); err != nil {
		_ = db.Close()
		return nil, err
	}

	s := &DuckDBStore{
		db:    db,
		path:  path,
		cfg:   cfg,
		cache: newDashboardCache(cfg.QueryCacheSeconds, 64),
	}
	s.healthy.Store(1)

	if recovered, err := s.RecoverInterrupted(ctx, time.Now().UTC()); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("recover interrupted: %w", err)
	} else {
		s.recovered.Store(recovered)
	}
	return s, nil
}

// RecoveredEvents 返回本次进程启动期间结算的遗留 started 事件数。
func (s *DuckDBStore) RecoveredEvents() int64 {
	if s == nil {
		return 0
	}
	return s.recovered.Load()
}

func applyRuntimeSettings(db *sql.DB, cfg config.UsageStoreConfig) error {
	memSQL, err := config.UsageStoreMemoryLimitSQL(cfg)
	if err != nil {
		return fmt.Errorf("usage_store.memory_limit: %w", err)
	}
	threads := cfg.Threads
	if threads < 1 {
		threads = 2
	}
	stmts := []string{
		fmt.Sprintf("SET memory_limit = '%s'", memSQL),
		fmt.Sprintf("SET threads = %d", threads),
		"SET enable_external_access = false",
		"SET autoinstall_known_extensions = false",
		"SET autoload_known_extensions = false",
	}
	for _, stmt := range stmts {
		if _, err := db.Exec(stmt); err != nil {
			return fmt.Errorf("apply duckdb setting: %w", err)
		}
	}
	return nil
}

// Healthy 报告写入路径健康状态。
func (s *DuckDBStore) Healthy() bool {
	if s == nil || s.closed.Load() {
		return false
	}
	return s.healthy.Load() == 1
}

func (s *DuckDBStore) markDegraded() {
	s.healthy.Store(0)
}

func (s *DuckDBStore) markHealthy() {
	s.healthy.Store(1)
}

// Start 插入 state=started 行;event_id 冲突返回 ErrDuplicateEvent。
func (s *DuckDBStore) Start(ctx context.Context, rec StartRecord) error {
	if s.closed.Load() {
		return ErrStoreClosed
	}
	if rec.EventID == "" {
		return fmt.Errorf("event_id is required")
	}
	if rec.APIKeyID == "" {
		return fmt.Errorf("api_key_id is required")
	}
	startedAt := rec.StartedAt.UTC()
	if startedAt.IsZero() {
		startedAt = time.Now().UTC()
	}
	usageDate := startedAt.Format("2006-01-02")

	s.write.Lock()
	defer s.write.Unlock()

	_, err := s.db.ExecContext(ctx, `
INSERT INTO usage_events (
    event_id, round_id, started_at, usage_date, api_key_id,
    operation, route, client_endpoint, client_protocol,
    provider, model, state
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		rec.EventID,
		nullInt64(rec.RoundID),
		startedAt,
		usageDate,
		rec.APIKeyID,
		nullString(rec.Operation),
		nullString(rec.Route),
		nullString(rec.ClientEndpoint),
		nullString(rec.ClientProtocol),
		nullString(rec.Provider),
		nullString(rec.Model),
		StateStarted,
	)
	if err != nil {
		if isUniqueViolation(err) {
			// 业务一致性错误,非存储不可用。
			return ErrDuplicateEvent
		}
		s.markDegraded()
		return ErrStoreUnavailable
	}
	s.markHealthy()
	return nil
}

// Complete 仅更新 state=started 的行;受影响行必须为 1。
func (s *DuckDBStore) Complete(ctx context.Context, rec CompleteRecord) error {
	if s.closed.Load() {
		return ErrStoreClosed
	}
	if rec.EventID == "" {
		return fmt.Errorf("event_id is required")
	}
	if rec.InputTokens < 0 || rec.OutputTokens < 0 ||
		rec.CachedInputTokens < 0 || rec.CacheCreationInputTokens < 0 {
		return ErrInvalidTokens
	}
	if rec.HTTPStatus < 100 || rec.HTTPStatus > 599 || strings.TrimSpace(rec.Outcome) == "" {
		return fmt.Errorf("completed usage event requires http_status and outcome")
	}
	total := rec.InputTokens + rec.OutputTokens
	completedAt := rec.CompletedAt.UTC()
	if completedAt.IsZero() {
		completedAt = time.Now().UTC()
	}

	s.write.Lock()
	defer s.write.Unlock()

	res, err := s.db.ExecContext(ctx, `
UPDATE usage_events
SET
    completed_at = ?,
    provider = COALESCE(NULLIF(?, ''), provider),
    model = COALESCE(NULLIF(?, ''), model),
    upstream_protocol = ?,
    upstream_endpoint = ?,
    conversion_mode = ?,
    input_tokens = ?,
    output_tokens = ?,
    total_tokens = ?,
    cached_input_tokens = ?,
    cache_creation_input_tokens = ?,
    http_status = ?,
    outcome = ?,
    error_code = ?,
    duration_ms = ?,
    upstream_duration_ms = ?,
    stream = ?,
    estimated = ?,
    state = ?
WHERE event_id = ?
  AND state = ?`,
		completedAt,
		rec.Provider,
		rec.Model,
		nullString(rec.UpstreamProtocol),
		nullString(rec.UpstreamEndpoint),
		nullString(rec.ConversionMode),
		rec.InputTokens,
		rec.OutputTokens,
		total,
		rec.CachedInputTokens,
		rec.CacheCreationInputTokens,
		rec.HTTPStatus,
		nullString(rec.Outcome),
		nullString(rec.ErrorCode),
		rec.Duration.Milliseconds(),
		rec.UpstreamDuration.Milliseconds(),
		rec.Stream,
		rec.Estimated,
		StateCompleted,
		rec.EventID,
		StateStarted,
	)
	if err != nil {
		s.markDegraded()
		return ErrStoreUnavailable
	}
	n, err := res.RowsAffected()
	if err != nil {
		s.markDegraded()
		return ErrStoreUnavailable
	}
	if n != 1 {
		// 缺失或重复 Complete:不降级健康(业务一致性问题),但返回错误。
		return ErrEventNotStarted
	}
	s.markHealthy()
	return nil
}

// RecoverInterrupted 将遗留 started 行结算为 process_interrupted。
func (s *DuckDBStore) RecoverInterrupted(ctx context.Context, at time.Time) (int64, error) {
	if s.closed.Load() {
		return 0, ErrStoreClosed
	}
	if at.IsZero() {
		at = time.Now().UTC()
	} else {
		at = at.UTC()
	}

	s.write.Lock()
	defer s.write.Unlock()

	res, err := s.db.ExecContext(ctx, `
UPDATE usage_events
SET
    completed_at = ?,
    http_status = 500,
    outcome = ?,
    error_code = ?,
    state = ?
WHERE state = ?`,
		at,
		OutcomeProcessInterrupted,
		OutcomeProcessInterrupted,
		StateCompleted,
		StateStarted,
	)
	if err != nil {
		s.markDegraded()
		return 0, ErrStoreUnavailable
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, ErrStoreUnavailable
	}
	s.markHealthy()
	return n, nil
}

// Checkpoint 执行 CHECKPOINT,刷新 WAL。
func (s *DuckDBStore) Checkpoint(ctx context.Context) error {
	if s.closed.Load() {
		return ErrStoreClosed
	}
	s.write.Lock()
	defer s.write.Unlock()
	if _, err := s.db.ExecContext(ctx, `CHECKPOINT`); err != nil {
		return ErrStoreUnavailable
	}
	return nil
}

// Close 先 checkpoint 再关闭连接。
func (s *DuckDBStore) Close() error {
	if s == nil {
		return nil
	}
	if s.closed.Swap(true) {
		return nil
	}
	s.write.Lock()
	defer s.write.Unlock()
	_, _ = s.db.Exec(`CHECKPOINT`)
	if err := s.db.Close(); err != nil {
		return err
	}
	return nil
}

func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullInt64(v int64) any {
	if v == 0 {
		return nil
	}
	return v
}

func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	// DuckDB 约束冲突信息通常包含 Constraint/duplicate/unique 等关键字。
	msg := strings.ToLower(err.Error())
	for _, key := range []string{"constraint", "duplicate", "unique", "primary key"} {
		if strings.Contains(msg, key) {
			return true
		}
	}
	return false
}
