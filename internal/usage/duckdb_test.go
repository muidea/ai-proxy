package usage

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/csv"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"ai-proxy/internal/config"

	_ "github.com/duckdb/duckdb-go/v2"
)

func testCfg(path string) config.UsageStoreConfig {
	return config.UsageStoreConfig{
		Path:              path,
		MemoryLimit:       "256MB",
		Threads:           2,
		QueryCacheSeconds: 0, // 测试默认关缓存,避免干扰
	}
}

func openTestStore(t *testing.T) *DuckDBStore {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "test.duckdb")
	s, err := OpenDuckDB(testCfg(path))
	if err != nil {
		t.Fatalf("OpenDuckDB: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestMigrationFirstAndIdempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mig.duckdb")
	cfg := testCfg(path)

	s1, err := OpenDuckDB(cfg)
	if err != nil {
		t.Fatalf("open1: %v", err)
	}
	if err := s1.Close(); err != nil {
		t.Fatalf("close1: %v", err)
	}

	s2, err := OpenDuckDB(cfg)
	if err != nil {
		t.Fatalf("open2 (idempotent): %v", err)
	}
	defer s2.Close()

	var ver int
	if err := s2.db.QueryRow(`SELECT version FROM schema_migrations WHERE version = 1`).Scan(&ver); err != nil {
		t.Fatalf("read migration: %v", err)
	}
	if ver != 1 {
		t.Fatalf("version = %d", ver)
	}
	if err := s2.db.QueryRow(`SELECT version FROM schema_migrations WHERE version = 2`).Scan(&ver); err != nil || ver != 2 {
		t.Fatalf("migration v2 = %d, err=%v", ver, err)
	}
}

func TestUnknownHigherSchemaVersionFailFast(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "future.duckdb")

	// 先创建带更高版本的库。
	db, err := sql.Open("duckdb", path)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	if _, err := db.Exec(`
CREATE TABLE schema_migrations (
    version INTEGER PRIMARY KEY,
    name VARCHAR NOT NULL,
    applied_at TIMESTAMPTZ NOT NULL
)`); err != nil {
		t.Fatalf("create mig: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO schema_migrations(version, name, applied_at) VALUES (99, 'future', ?)`,
		time.Now().UTC(),
	); err != nil {
		t.Fatalf("insert future: %v", err)
	}
	_ = db.Close()

	_, err = OpenDuckDB(testCfg(path))
	if err == nil {
		t.Fatal("expected fail-fast on unknown higher schema version")
	}
}

func TestStartInsertAndDuplicateReject(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 17, 10, 0, 0, 0, time.UTC)

	rec := StartRecord{
		EventID: "evt-1", RoundID: 1, StartedAt: now, APIKeyID: "default",
		Operation: "chat_completions", ClientEndpoint: "/v1/chat/completions",
	}
	if err := s.Start(ctx, rec); err != nil {
		t.Fatalf("start: %v", err)
	}
	err := s.Start(ctx, rec)
	if !errors.Is(err, ErrDuplicateEvent) {
		t.Fatalf("dup start err = %v, want ErrDuplicateEvent", err)
	}
	if !s.Healthy() {
		// 重复是业务错误,但我们在 Start 里对 unique 也 markDegraded;
		// 规范要求写失败降级。这里至少不应 panic。
	}
}

func TestCompleteOnlyUpdatesStarted(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 17, 10, 0, 0, 0, time.UTC)

	if err := s.Start(ctx, StartRecord{
		EventID: "evt-c", StartedAt: now, APIKeyID: "codex",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := s.Complete(ctx, CompleteRecord{
		EventID: "evt-c", CompletedAt: now.Add(time.Second),
		Provider: "deepseek", Model: "flash",
		InputTokens: 10, OutputTokens: 5,
		HTTPStatus: 200, Outcome: "success",
		Duration: 100 * time.Millisecond,
	}); err != nil {
		t.Fatalf("complete: %v", err)
	}

	var state string
	var total int64
	var provider string
	if err := s.db.QueryRow(
		`SELECT state, total_tokens, provider FROM usage_events WHERE event_id = ?`, "evt-c",
	).Scan(&state, &total, &provider); err != nil {
		t.Fatalf("query: %v", err)
	}
	if state != StateCompleted || total != 15 || provider != "deepseek" {
		t.Fatalf("state=%s total=%d provider=%s", state, total, provider)
	}
}

func TestCompleteRequiresStatusAndOutcome(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	if err := s.Start(ctx, StartRecord{EventID: "evt-required", APIKeyID: "default"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Complete(ctx, CompleteRecord{EventID: "evt-required", HTTPStatus: 0}); err == nil {
		t.Fatal("completed event without status/outcome must be rejected")
	}
}

func TestDuplicateCompleteError(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	if err := s.Start(ctx, StartRecord{EventID: "evt-d", StartedAt: now, APIKeyID: "default"}); err != nil {
		t.Fatalf("start: %v", err)
	}
	cr := CompleteRecord{
		EventID: "evt-d", CompletedAt: now, InputTokens: 1, OutputTokens: 1,
		HTTPStatus: 200, Outcome: "success",
	}
	if err := s.Complete(ctx, cr); err != nil {
		t.Fatalf("complete1: %v", err)
	}
	err := s.Complete(ctx, cr)
	if !errors.Is(err, ErrEventNotStarted) {
		t.Fatalf("dup complete err = %v, want ErrEventNotStarted", err)
	}
}

func TestTotalEqualsInputPlusOutput(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	if err := s.Start(ctx, StartRecord{EventID: "evt-t", StartedAt: now, APIKeyID: "default"}); err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := s.Complete(ctx, CompleteRecord{
		EventID: "evt-t", CompletedAt: now, InputTokens: 100, OutputTokens: 23,
		HTTPStatus: 200, Outcome: "success",
	}); err != nil {
		t.Fatalf("complete: %v", err)
	}
	var in, out, total int64
	if err := s.db.QueryRow(
		`SELECT input_tokens, output_tokens, total_tokens FROM usage_events WHERE event_id = ?`, "evt-t",
	).Scan(&in, &out, &total); err != nil {
		t.Fatalf("query: %v", err)
	}
	if total != in+out || total != 123 {
		t.Fatalf("tokens in=%d out=%d total=%d", in, out, total)
	}
}

func TestReopenPersistence(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "persist.duckdb")
	cfg := testCfg(path)
	ctx := context.Background()
	now := time.Date(2026, 7, 16, 8, 0, 0, 0, time.UTC)

	s1, err := OpenDuckDB(cfg)
	if err != nil {
		t.Fatalf("open1: %v", err)
	}
	if err := s1.Start(ctx, StartRecord{EventID: "evt-p", StartedAt: now, APIKeyID: "workorch"}); err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := s1.Complete(ctx, CompleteRecord{
		EventID: "evt-p", CompletedAt: now.Add(time.Second),
		InputTokens: 7, OutputTokens: 3, HTTPStatus: 200, Outcome: "success",
	}); err != nil {
		t.Fatalf("complete: %v", err)
	}
	if err := s1.Close(); err != nil {
		t.Fatalf("close1: %v", err)
	}

	s2, err := OpenDuckDB(cfg)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()
	var total int64
	var key string
	if err := s2.db.QueryRow(
		`SELECT total_tokens, api_key_id FROM usage_events WHERE event_id = ?`, "evt-p",
	).Scan(&total, &key); err != nil {
		t.Fatalf("query after reopen: %v", err)
	}
	if total != 10 || key != "workorch" {
		t.Fatalf("got total=%d key=%q", total, key)
	}
}

func TestRecoverInterrupted(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "recover.duckdb")
	cfg := testCfg(path)
	ctx := context.Background()
	now := time.Now().UTC()

	s1, err := OpenDuckDB(cfg)
	if err != nil {
		t.Fatalf("open1: %v", err)
	}
	if err := s1.Start(ctx, StartRecord{EventID: "evt-r1", StartedAt: now, APIKeyID: "default"}); err != nil {
		t.Fatalf("start: %v", err)
	}
	// 不 Complete,直接关闭(模拟崩溃)。
	// 手动关闭连接跳过 Recover;用 db.Close 前不 checkpoint 也行。
	s1.closed.Store(true)
	_ = s1.db.Close()

	s2, err := OpenDuckDB(cfg)
	if err != nil {
		t.Fatalf("reopen with recover: %v", err)
	}
	defer s2.Close()

	var outcome, state string
	var status int
	if err := s2.db.QueryRow(
		`SELECT outcome, state, http_status FROM usage_events WHERE event_id = ?`, "evt-r1",
	).Scan(&outcome, &state, &status); err != nil {
		t.Fatalf("query: %v", err)
	}
	if outcome != OutcomeProcessInterrupted || state != StateCompleted || status != 500 {
		t.Fatalf("outcome=%s state=%s status=%d", outcome, state, status)
	}
}

func TestDashboardAggregates(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	day1 := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	day2 := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)

	seed := func(id, key string, at time.Time, in, out int64, outcome string) {
		t.Helper()
		if err := s.Start(ctx, StartRecord{EventID: id, StartedAt: at, APIKeyID: key, Provider: "p", Model: "m"}); err != nil {
			t.Fatalf("start %s: %v", id, err)
		}
		if err := s.Complete(ctx, CompleteRecord{
			EventID: id, CompletedAt: at.Add(time.Millisecond),
			Provider: "p", Model: "m",
			InputTokens: in, OutputTokens: out,
			HTTPStatus: 200, Outcome: outcome,
		}); err != nil {
			t.Fatalf("complete %s: %v", id, err)
		}
	}
	seed("a1", "codex", day1, 10, 5, "success")
	seed("a2", "codex", day1, 20, 0, "error")
	seed("b1", "workorch", day2, 100, 50, "success")

	dash, err := s.Dashboard(ctx, UsageFilter{
		From: time.Date(2026, 7, 10, 0, 0, 0, 0, time.UTC),
		To:   time.Date(2026, 7, 12, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("dashboard: %v", err)
	}
	if dash.Summary.Requests != 3 {
		t.Fatalf("requests = %d", dash.Summary.Requests)
	}
	if dash.Summary.SuccessRequests != 2 {
		t.Fatalf("success = %d", dash.Summary.SuccessRequests)
	}
	if dash.Summary.TotalTokens != 185 {
		t.Fatalf("total_tokens = %d", dash.Summary.TotalTokens)
	}
	// 缺失日补 0: 7-10, 7-11 两天。
	if len(dash.Daily) != 2 {
		t.Fatalf("daily len = %d, want 2", len(dash.Daily))
	}
	if dash.Daily[0].Date != "2026-07-10" || dash.Daily[0].Requests != 2 {
		t.Fatalf("daily[0] = %+v", dash.Daily[0])
	}
	if dash.Daily[1].Date != "2026-07-11" || dash.Daily[1].TotalTokens != 150 {
		t.Fatalf("daily[1] = %+v", dash.Daily[1])
	}
	if len(dash.ByAPIKey) != 2 {
		t.Fatalf("by_key len = %d", len(dash.ByAPIKey))
	}
	// workorch total 150 > codex 35
	if dash.ByAPIKey[0].APIKeyID != "workorch" {
		t.Fatalf("top key = %s", dash.ByAPIKey[0].APIKeyID)
	}

	// 按 key 筛选。
	dash2, err := s.Dashboard(ctx, UsageFilter{
		From:     time.Date(2026, 7, 10, 0, 0, 0, 0, time.UTC),
		To:       time.Date(2026, 7, 12, 0, 0, 0, 0, time.UTC),
		APIKeyID: "codex",
	})
	if err != nil {
		t.Fatalf("dashboard filter: %v", err)
	}
	if dash2.Summary.Requests != 2 || dash2.Summary.TotalTokens != 35 {
		t.Fatalf("filtered summary = %+v", dash2.Summary)
	}
}

func TestCursorPaginationNoDupMiss(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	base := time.Date(2026, 7, 15, 0, 0, 0, 0, time.UTC)

	// 插入 5 条,按时间递增。
	for i := 0; i < 5; i++ {
		id := fmt.Sprintf("evt-%d", i)
		at := base.Add(time.Duration(i) * time.Minute)
		if err := s.Start(ctx, StartRecord{EventID: id, StartedAt: at, APIKeyID: "default"}); err != nil {
			t.Fatalf("start %s: %v", id, err)
		}
		if err := s.Complete(ctx, CompleteRecord{
			EventID: id, CompletedAt: at.Add(time.Second),
			InputTokens: 1, OutputTokens: 1, HTTPStatus: 200, Outcome: "success",
		}); err != nil {
			t.Fatalf("complete %s: %v", id, err)
		}
	}

	filterBase := EventFilter{
		UsageFilter: UsageFilter{
			From: base,
			To:   base.Add(24 * time.Hour),
		},
		PageSize: 2,
	}

	seen := map[string]bool{}
	cursor := ""
	pages := 0
	for {
		f := filterBase
		f.Cursor = cursor
		page, err := s.Events(ctx, f)
		if err != nil {
			t.Fatalf("events page %d: %v", pages, err)
		}
		pages++
		for _, e := range page.Events {
			if seen[e.EventID] {
				t.Fatalf("duplicate event %s across pages", e.EventID)
			}
			seen[e.EventID] = true
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
		if pages > 10 {
			t.Fatal("too many pages")
		}
	}
	if len(seen) != 5 {
		t.Fatalf("seen %d events, want 5: %v", len(seen), seen)
	}
	// 顺序应为倒序: evt-4, evt-3, ...
	page1, err := s.Events(ctx, filterBase)
	if err != nil {
		t.Fatalf("page1: %v", err)
	}
	if len(page1.Events) != 2 || page1.Events[0].EventID != "evt-4" || page1.Events[1].EventID != "evt-3" {
		t.Fatalf("page1 order = %v", eventIDs(page1.Events))
	}
}

func eventIDs(events []Event) []string {
	out := make([]string, len(events))
	for i, e := range events {
		out[i] = e.EventID
	}
	return out
}

func TestExportCSV(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 17, 9, 0, 0, 0, time.UTC)
	if err := s.Start(ctx, StartRecord{
		EventID: "evt-x", StartedAt: now, APIKeyID: "codex",
		Operation: "chat_completions",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := s.Complete(ctx, CompleteRecord{
		EventID: "evt-x", CompletedAt: now.Add(time.Second),
		Provider: "openai", Model: "gpt",
		InputTokens: 3, OutputTokens: 4,
		HTTPStatus: 200, Outcome: "success",
	}); err != nil {
		t.Fatalf("complete: %v", err)
	}

	var buf bytes.Buffer
	if err := s.ExportCSV(ctx, UsageFilter{
		From: now.Add(-time.Hour),
		To:   now.Add(time.Hour),
	}, &buf); err != nil {
		t.Fatalf("export: %v", err)
	}
	r := csv.NewReader(bytes.NewReader(buf.Bytes()))
	rows, err := r.ReadAll()
	if err != nil {
		t.Fatalf("parse csv: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2 (header+1)", len(rows))
	}
	if rows[0][0] != "event_id" {
		t.Fatalf("header[0] = %s", rows[0][0])
	}
	if rows[1][0] != "evt-x" {
		t.Fatalf("row event_id = %s", rows[1][0])
	}
	// 不包含密钥类字段名。
	for _, h := range rows[0] {
		if h == "api_key" || h == "authorization" || h == "body" {
			t.Fatalf("export leaked column %q", h)
		}
	}
}

func TestConcurrentStartCompleteConsistency(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	const n = 50
	var wg sync.WaitGroup
	errs := make(chan error, n*2)
	base := time.Now().UTC()

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := fmt.Sprintf("c-%d", i)
			at := base.Add(time.Duration(i) * time.Millisecond)
			if err := s.Start(ctx, StartRecord{EventID: id, StartedAt: at, APIKeyID: "default"}); err != nil {
				errs <- fmt.Errorf("start %s: %w", id, err)
				return
			}
			if err := s.Complete(ctx, CompleteRecord{
				EventID: id, CompletedAt: at.Add(time.Millisecond),
				InputTokens: 2, OutputTokens: 3,
				HTTPStatus: 200, Outcome: "success",
			}); err != nil {
				errs <- fmt.Errorf("complete %s: %w", id, err)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}

	var count int
	var total int64
	if err := s.db.QueryRow(`SELECT count(*), coalesce(sum(total_tokens),0) FROM usage_events WHERE state = 'completed'`).
		Scan(&count, &total); err != nil {
		t.Fatalf("aggregate: %v", err)
	}
	if count != n {
		t.Fatalf("count = %d, want %d", count, n)
	}
	if total != int64(n*5) {
		t.Fatalf("total_tokens = %d, want %d", total, n*5)
	}
	if !s.Healthy() {
		t.Fatal("store should be healthy after successful writes")
	}
}

func TestAllTimeByKey(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	events := []struct {
		id, key string
		in, out int64
	}{
		{"e1", "k1", 10, 1},
		{"e2", "k1", 5, 5},
		{"e3", "k2", 100, 0},
	}
	for _, e := range events {
		if err := s.Start(ctx, StartRecord{EventID: e.id, StartedAt: now, APIKeyID: e.key}); err != nil {
			t.Fatalf("start: %v", err)
		}
		if err := s.Complete(ctx, CompleteRecord{
			EventID: e.id, CompletedAt: now, InputTokens: e.in, OutputTokens: e.out,
			HTTPStatus: 200, Outcome: "success",
		}); err != nil {
			t.Fatalf("complete: %v", err)
		}
	}
	m, err := s.AllTimeByKey(ctx)
	if err != nil {
		t.Fatalf("AllTimeByKey: %v", err)
	}
	if m["k1"].Requests != 2 || m["k1"].TotalTokens != 21 {
		t.Fatalf("k1 = %+v", m["k1"])
	}
	if m["k2"].Requests != 1 || m["k2"].TotalTokens != 100 {
		t.Fatalf("k2 = %+v", m["k2"])
	}
}

func TestFilterValidation(t *testing.T) {
	f := &UsageFilter{
		From: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		To:   time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}
	if err := ValidateUsageFilter(f); err == nil {
		t.Fatal("expected from < to")
	}
	f.To = f.From.Add(400 * 24 * time.Hour)
	if err := ValidateUsageFilter(f); err == nil {
		t.Fatal("expected max span error")
	}
	f.To = f.From.Add(10 * 24 * time.Hour)
	f.APIKeyID = string(make([]byte, 300))
	if err := ValidateUsageFilter(f); err == nil {
		t.Fatal("expected length error")
	}
	f.APIKeyID = "ok"
	if err := ValidateUsageFilter(f); err != nil {
		t.Fatalf("valid filter: %v", err)
	}

	ef := &EventFilter{UsageFilter: *f, PageSize: 200}
	if err := ValidateEventFilter(ef); err == nil {
		t.Fatal("expected page_size max error")
	}
	ef.PageSize = 0
	if err := ValidateEventFilter(ef); err != nil {
		t.Fatalf("default page: %v", err)
	}
	if ef.PageSize != 50 {
		t.Fatalf("page_size default = %d", ef.PageSize)
	}
}

func TestMemoryStoreBasic(t *testing.T) {
	s := NewMemoryStore()
	ctx := context.Background()
	now := time.Now().UTC()
	if err := s.Start(ctx, StartRecord{EventID: "m1", StartedAt: now, APIKeyID: "default"}); err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := s.Complete(ctx, CompleteRecord{
		EventID: "m1", CompletedAt: now, InputTokens: 1, OutputTokens: 2,
		HTTPStatus: 200, Outcome: "success",
	}); err != nil {
		t.Fatalf("complete: %v", err)
	}
	if err := s.Start(ctx, StartRecord{EventID: "m1", StartedAt: now, APIKeyID: "default"}); !errors.Is(err, ErrDuplicateEvent) {
		t.Fatalf("dup: %v", err)
	}
	n, err := s.RecoverInterrupted(ctx, now)
	if err != nil || n != 0 {
		t.Fatalf("recover: n=%d err=%v", n, err)
	}
	// 未完成再恢复。
	if err := s.Start(ctx, StartRecord{EventID: "m2", StartedAt: now, APIKeyID: "x"}); err != nil {
		t.Fatalf("start2: %v", err)
	}
	n, err = s.RecoverInterrupted(ctx, now)
	if err != nil || n != 1 {
		t.Fatalf("recover2: n=%d err=%v", n, err)
	}
}
