package usage

import (
	"context"
	"database/sql"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

// dashboardCache 是 Dashboard 短 TTL + LRU 缓存。
type dashboardCache struct {
	ttl     time.Duration
	maxSize int
	mu      sync.Mutex
	items   map[string]*dashCacheEntry
	order   []string // 简单 LRU:尾部最新
}

type dashCacheEntry struct {
	value     Dashboard
	expiresAt time.Time
}

func newDashboardCache(seconds, maxSize int) *dashboardCache {
	if seconds <= 0 || maxSize <= 0 {
		return &dashboardCache{ttl: 0, maxSize: 0, items: make(map[string]*dashCacheEntry)}
	}
	return &dashboardCache{
		ttl:     time.Duration(seconds) * time.Second,
		maxSize: maxSize,
		items:   make(map[string]*dashCacheEntry, maxSize),
		order:   make([]string, 0, maxSize),
	}
}

func (c *dashboardCache) get(key string) (Dashboard, bool) {
	if c == nil || c.ttl <= 0 {
		return Dashboard{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.items[key]
	if !ok {
		return Dashboard{}, false
	}
	if time.Now().After(e.expiresAt) {
		delete(c.items, key)
		c.removeOrder(key)
		return Dashboard{}, false
	}
	// 触碰 LRU。
	c.touchOrder(key)
	return e.value, true
}

func (c *dashboardCache) put(key string, value Dashboard) {
	if c == nil || c.ttl <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.items[key]; !ok && len(c.items) >= c.maxSize {
		// 淘汰最旧。
		if len(c.order) > 0 {
			old := c.order[0]
			c.order = c.order[1:]
			delete(c.items, old)
		}
	}
	c.items[key] = &dashCacheEntry{value: value, expiresAt: time.Now().Add(c.ttl)}
	c.touchOrder(key)
}

func (c *dashboardCache) touchOrder(key string) {
	c.removeOrder(key)
	c.order = append(c.order, key)
}

func (c *dashboardCache) removeOrder(key string) {
	for i, k := range c.order {
		if k == key {
			c.order = append(c.order[:i], c.order[i+1:]...)
			return
		}
	}
}

func dashboardCacheKey(f UsageFilter) string {
	est := "nil"
	if f.Estimated != nil {
		if *f.Estimated {
			est = "1"
		} else {
			est = "0"
		}
	}
	return strings.Join([]string{
		f.From.UTC().Format(time.RFC3339Nano),
		f.To.UTC().Format(time.RFC3339Nano),
		f.APIKeyID,
		f.Provider,
		f.Model,
		f.Outcome,
		est,
		strconv.FormatBool(f.AllTime),
	}, "|")
}

// Dashboard 聚合 summary / daily / by_api_key。
func (s *DuckDBStore) Dashboard(ctx context.Context, filter UsageFilter) (Dashboard, error) {
	if s.closed.Load() {
		return Dashboard{}, ErrStoreClosed
	}
	if err := ValidateUsageFilter(&filter); err != nil {
		return Dashboard{}, err
	}
	key := dashboardCacheKey(filter)
	if cached, ok := s.cache.get(key); ok {
		return cached, nil
	}

	from, to := filter.From.UTC(), filter.To.UTC()
	if filter.AllTime {
		// all-time:用极宽窗口,仍走同一 SQL 模板。
		if from.IsZero() {
			from = time.Unix(0, 0).UTC()
		}
		if to.IsZero() {
			to = time.Now().UTC().Add(24 * time.Hour)
		}
	}

	where, args := buildFilterWhere(filter, from, to)

	summary, err := s.querySummary(ctx, where, args)
	if err != nil {
		return Dashboard{}, err
	}
	daily, err := s.queryDaily(ctx, where, args)
	if err != nil {
		return Dashboard{}, err
	}
	byKey, err := s.queryByAPIKey(ctx, where, args)
	if err != nil {
		return Dashboard{}, err
	}

	if !filter.AllTime {
		daily = fillMissingDays(from, to, daily)
	}

	out := Dashboard{
		Scope: ScopeInfo{
			From:     from,
			To:       to,
			Timezone: "UTC",
		},
		Summary:  summary,
		Daily:    daily,
		ByAPIKey: byKey,
	}
	s.cache.put(key, out)
	return out, nil
}

// Count 返回匹配筛选的事件数，供管理端在 CSV 导出前实施资源上限。
func (s *DuckDBStore) Count(ctx context.Context, filter UsageFilter) (int64, error) {
	if s.closed.Load() {
		return 0, ErrStoreClosed
	}
	if err := ValidateUsageFilter(&filter); err != nil {
		return 0, err
	}
	from, to := filter.From.UTC(), filter.To.UTC()
	if filter.AllTime {
		if from.IsZero() {
			from = time.Unix(0, 0).UTC()
		}
		if to.IsZero() {
			to = time.Now().UTC().Add(24 * time.Hour)
		}
	}
	where, args := buildFilterWhere(filter, from, to)
	var count int64
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM usage_events WHERE `+where, args...).Scan(&count); err != nil {
		return 0, ErrStoreUnavailable
	}
	return count, nil
}

func (s *DuckDBStore) querySummary(ctx context.Context, where string, args []any) (Summary, error) {
	q := `
SELECT
    count(*) AS requests,
    count(*) FILTER (WHERE outcome = 'success') AS success_requests,
    count(*) FILTER (WHERE state = 'completed' AND outcome IS DISTINCT FROM 'success') AS failed_requests,
    coalesce(sum(input_tokens), 0) AS input_tokens,
    coalesce(sum(output_tokens), 0) AS output_tokens,
    coalesce(sum(total_tokens), 0) AS total_tokens
FROM usage_events
WHERE ` + where
	var sum Summary
	err := s.db.QueryRowContext(ctx, q, args...).Scan(
		&sum.Requests,
		&sum.SuccessRequests,
		&sum.FailedRequests,
		&sum.InputTokens,
		&sum.OutputTokens,
		&sum.TotalTokens,
	)
	if err != nil {
		return Summary{}, ErrStoreUnavailable
	}
	fillSummaryRates(&sum)
	return sum, nil
}

func (s *DuckDBStore) queryDaily(ctx context.Context, where string, args []any) ([]DailyBucket, error) {
	q := `
SELECT
    cast(usage_date AS VARCHAR) AS usage_date,
    count(*) AS requests,
    coalesce(sum(input_tokens), 0) AS input_tokens,
    coalesce(sum(output_tokens), 0) AS output_tokens,
    coalesce(sum(total_tokens), 0) AS total_tokens
FROM usage_events
WHERE ` + where + `
GROUP BY usage_date
ORDER BY usage_date`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, ErrStoreUnavailable
	}
	defer rows.Close()

	var out []DailyBucket
	for rows.Next() {
		var b DailyBucket
		if err := rows.Scan(&b.Date, &b.Requests, &b.InputTokens, &b.OutputTokens, &b.TotalTokens); err != nil {
			return nil, ErrStoreUnavailable
		}
		// 规范化日期字符串。
		if len(b.Date) > 10 {
			b.Date = b.Date[:10]
		}
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, ErrStoreUnavailable
	}
	return out, nil
}

func (s *DuckDBStore) queryByAPIKey(ctx context.Context, where string, args []any) ([]KeySummary, error) {
	q := `
SELECT
    api_key_id,
    count(*) AS requests,
    count(*) FILTER (WHERE outcome = 'success') AS success_requests,
    count(*) FILTER (WHERE state = 'completed' AND outcome IS DISTINCT FROM 'success') AS failed_requests,
    coalesce(sum(input_tokens), 0) AS input_tokens,
    coalesce(sum(output_tokens), 0) AS output_tokens,
    coalesce(sum(total_tokens), 0) AS total_tokens,
    max(started_at) AS last_used_at
FROM usage_events
WHERE ` + where + `
GROUP BY api_key_id
ORDER BY total_tokens DESC, api_key_id ASC`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, ErrStoreUnavailable
	}
	defer rows.Close()

	var out []KeySummary
	for rows.Next() {
		var k KeySummary
		var last sql.NullTime
		if err := rows.Scan(
			&k.APIKeyID,
			&k.Requests,
			&k.SuccessRequests,
			&k.FailedRequests,
			&k.InputTokens,
			&k.OutputTokens,
			&k.TotalTokens,
			&last,
		); err != nil {
			return nil, ErrStoreUnavailable
		}
		if last.Valid {
			t := last.Time.UTC()
			k.LastUsedAt = &t
		}
		out = append(out, k)
	}
	if err := rows.Err(); err != nil {
		return nil, ErrStoreUnavailable
	}
	return out, nil
}

// Events 使用 (started_at, event_id) cursor 倒序分页。
func (s *DuckDBStore) Events(ctx context.Context, filter EventFilter) (EventPage, error) {
	if s.closed.Load() {
		return EventPage{}, ErrStoreClosed
	}
	if err := ValidateEventFilter(&filter); err != nil {
		return EventPage{}, err
	}
	from, to := filter.From.UTC(), filter.To.UTC()
	if filter.AllTime {
		if from.IsZero() {
			from = time.Unix(0, 0).UTC()
		}
		if to.IsZero() {
			to = time.Now().UTC().Add(24 * time.Hour)
		}
	}

	where, args := buildFilterWhere(filter.UsageFilter, from, to)

	// cursor: base64("rfc3339nano|event_id")
	if filter.Cursor != "" {
		curAt, curID, err := decodeEventCursor(filter.Cursor)
		if err != nil {
			return EventPage{}, fmt.Errorf("invalid cursor")
		}
		where += ` AND (started_at < ? OR (started_at = ? AND event_id < ?))`
		args = append(args, curAt, curAt, curID)
	}

	limit := filter.PageSize
	// 多取 1 条判断 has_more。
	q := `
SELECT
    event_id, coalesce(round_id, 0), started_at, completed_at,
    cast(usage_date AS VARCHAR), api_key_id,
    coalesce(provider, ''), coalesce(model, ''),
    coalesce(operation, ''), coalesce(route, ''),
    coalesce(client_endpoint, ''), coalesce(client_protocol, ''),
    coalesce(upstream_protocol, ''), coalesce(upstream_endpoint, ''),
    coalesce(conversion_mode, ''),
    input_tokens, output_tokens, total_tokens,
    cached_input_tokens, cache_creation_input_tokens,
    http_status, coalesce(outcome, ''), coalesce(error_code, ''),
    duration_ms, upstream_duration_ms,
    stream, estimated, state
FROM usage_events
WHERE ` + where + `
ORDER BY started_at DESC, event_id DESC
LIMIT ?`
	args = append(args, limit+1)

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return EventPage{}, ErrStoreUnavailable
	}
	defer rows.Close()

	var events []Event
	for rows.Next() {
		var e Event
		var completedAt sql.NullTime
		var httpStatus sql.NullInt64
		var durationMS, upstreamMS sql.NullInt64
		var usageDate string
		if err := rows.Scan(
			&e.EventID, &e.RoundID, &e.StartedAt, &completedAt,
			&usageDate, &e.APIKeyID,
			&e.Provider, &e.Model,
			&e.Operation, &e.Route,
			&e.ClientEndpoint, &e.ClientProtocol,
			&e.UpstreamProtocol, &e.UpstreamEndpoint,
			&e.ConversionMode,
			&e.InputTokens, &e.OutputTokens, &e.TotalTokens,
			&e.CachedInputTokens, &e.CacheCreationInputTokens,
			&httpStatus, &e.Outcome, &e.ErrorCode,
			&durationMS, &upstreamMS,
			&e.Stream, &e.Estimated, &e.State,
		); err != nil {
			return EventPage{}, ErrStoreUnavailable
		}
		e.StartedAt = e.StartedAt.UTC()
		if completedAt.Valid {
			t := completedAt.Time.UTC()
			e.CompletedAt = &t
		}
		if httpStatus.Valid {
			e.HTTPStatus = int(httpStatus.Int64)
		}
		if durationMS.Valid {
			e.DurationMS = durationMS.Int64
		}
		if upstreamMS.Valid {
			e.UpstreamDurationMS = upstreamMS.Int64
		}
		events = append(events, e)
	}
	if err := rows.Err(); err != nil {
		return EventPage{}, ErrStoreUnavailable
	}

	page := EventPage{}
	if len(events) > limit {
		page.Events = events[:limit]
		last := page.Events[len(page.Events)-1]
		page.NextCursor = encodeEventCursor(last.StartedAt, last.EventID)
	} else {
		page.Events = events
	}
	if page.Events == nil {
		page.Events = []Event{}
	}
	return page, nil
}

// AllTimeByKey 返回全量按 api_key_id 汇总,供 metrics bootstrap。
func (s *DuckDBStore) AllTimeByKey(ctx context.Context) (map[string]Summary, error) {
	if s.closed.Load() {
		return nil, ErrStoreClosed
	}
	q := `
SELECT
    api_key_id,
    count(*) AS requests,
    count(*) FILTER (WHERE outcome = 'success') AS success_requests,
    count(*) FILTER (WHERE state = 'completed' AND outcome IS DISTINCT FROM 'success') AS failed_requests,
    coalesce(sum(input_tokens), 0) AS input_tokens,
    coalesce(sum(output_tokens), 0) AS output_tokens,
    coalesce(sum(total_tokens), 0) AS total_tokens
FROM usage_events
GROUP BY api_key_id`
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, ErrStoreUnavailable
	}
	defer rows.Close()

	out := make(map[string]Summary)
	for rows.Next() {
		var id string
		var sum Summary
		if err := rows.Scan(
			&id,
			&sum.Requests,
			&sum.SuccessRequests,
			&sum.FailedRequests,
			&sum.InputTokens,
			&sum.OutputTokens,
			&sum.TotalTokens,
		); err != nil {
			return nil, ErrStoreUnavailable
		}
		fillSummaryRates(&sum)
		out[id] = sum
	}
	if err := rows.Err(); err != nil {
		return nil, ErrStoreUnavailable
	}
	return out, nil
}

// buildFilterWhere 构造固定模板 WHERE 与参数(不拼接用户列名)。
func buildFilterWhere(f UsageFilter, from, to time.Time) (string, []any) {
	parts := []string{"started_at >= ?", "started_at < ?"}
	args := []any{from, to}
	if f.APIKeyID != "" {
		parts = append(parts, "api_key_id = ?")
		args = append(args, f.APIKeyID)
	}
	if f.Provider != "" {
		parts = append(parts, "provider = ?")
		args = append(args, f.Provider)
	}
	if f.Model != "" {
		parts = append(parts, "model = ?")
		args = append(args, f.Model)
	}
	if f.Outcome != "" {
		parts = append(parts, "outcome = ?")
		args = append(args, f.Outcome)
	}
	if f.Estimated != nil {
		parts = append(parts, "estimated = ?")
		args = append(args, *f.Estimated)
	}
	return strings.Join(parts, " AND "), args
}

func encodeEventCursor(at time.Time, eventID string) string {
	raw := at.UTC().Format(time.RFC3339Nano) + "|" + eventID
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func decodeEventCursor(cursor string) (time.Time, string, error) {
	b, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		// 兼容标准 padding 编码。
		b, err = base64.URLEncoding.DecodeString(cursor)
		if err != nil {
			return time.Time{}, "", err
		}
	}
	parts := strings.SplitN(string(b), "|", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return time.Time{}, "", fmt.Errorf("bad cursor")
	}
	at, err := time.Parse(time.RFC3339Nano, parts[0])
	if err != nil {
		at, err = time.Parse(time.RFC3339, parts[0])
		if err != nil {
			return time.Time{}, "", err
		}
	}
	return at.UTC(), parts[1], nil
}
