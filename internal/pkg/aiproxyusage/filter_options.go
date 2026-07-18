package usage

import (
	"context"
	"database/sql"
	"strconv"
	"strings"
	"sync"
	"time"
)

// filterOptionsCache 是 FilterOptions 短 TTL + LRU 缓存（仅缓存 usage 半结果）。
type filterOptionsCache struct {
	ttl     time.Duration
	maxSize int
	mu      sync.Mutex
	items   map[string]*filterOptionsCacheEntry
	order   []string
}

type filterOptionsCacheEntry struct {
	value     FilterOptionsResult
	expiresAt time.Time
}

func newFilterOptionsCache(seconds, maxSize int) *filterOptionsCache {
	if seconds <= 0 || maxSize <= 0 {
		return &filterOptionsCache{ttl: 0, maxSize: 0, items: make(map[string]*filterOptionsCacheEntry)}
	}
	return &filterOptionsCache{
		ttl:     time.Duration(seconds) * time.Second,
		maxSize: maxSize,
		items:   make(map[string]*filterOptionsCacheEntry, maxSize),
		order:   make([]string, 0, maxSize),
	}
}

func (c *filterOptionsCache) get(key string) (FilterOptionsResult, bool) {
	if c == nil || c.ttl <= 0 {
		return FilterOptionsResult{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.items[key]
	if !ok {
		return FilterOptionsResult{}, false
	}
	if time.Now().After(e.expiresAt) {
		delete(c.items, key)
		c.removeOrder(key)
		return FilterOptionsResult{}, false
	}
	c.touchOrder(key)
	return e.value, true
}

func (c *filterOptionsCache) put(key string, value FilterOptionsResult) {
	if c == nil || c.ttl <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.items[key]; !ok && len(c.items) >= c.maxSize {
		if len(c.order) > 0 {
			old := c.order[0]
			c.order = c.order[1:]
			delete(c.items, old)
		}
	}
	c.items[key] = &filterOptionsCacheEntry{value: value, expiresAt: time.Now().Add(c.ttl)}
	c.touchOrder(key)
}

func (c *filterOptionsCache) touchOrder(key string) {
	c.removeOrder(key)
	c.order = append(c.order, key)
}

func (c *filterOptionsCache) removeOrder(key string) {
	for i, k := range c.order {
		if k == key {
			c.order = append(c.order[:i], c.order[i+1:]...)
			return
		}
	}
}

func filterOptionsCacheKey(from, to time.Time, allTime bool) string {
	return strings.Join([]string{
		from.UTC().Format(time.RFC3339Nano),
		to.UTC().Format(time.RFC3339Nano),
		strconv.FormatBool(allTime),
	}, "|")
}

// FilterOptions 在时间窗内 DISTINCT 提取 api_key_id / provider / model。
func (s *DuckDBStore) FilterOptions(ctx context.Context, q FilterOptionsQuery) (FilterOptionsResult, error) {
	if s.closed.Load() {
		return FilterOptionsResult{}, ErrStoreClosed
	}
	from, to, err := ResolveFilterOptionsRange(q)
	if err != nil {
		return FilterOptionsResult{}, err
	}
	// 缓存键用解析后的窗口；保留 allTime 以区分查询语义。
	key := filterOptionsCacheKey(from, to, q.AllTime)
	if cached, ok := s.optionsCache.get(key); ok {
		return cached, nil
	}

	out := FilterOptionsResult{From: from, To: to}
	out.APIKeyIDs, out.Truncated.APIKeyIDs, err = s.queryDistinctColumn(ctx, "api_key_id", from, to)
	if err != nil {
		return FilterOptionsResult{}, err
	}
	out.Providers, out.Truncated.Providers, err = s.queryDistinctColumn(ctx, "provider", from, to)
	if err != nil {
		return FilterOptionsResult{}, err
	}
	out.Models, out.Truncated.Models, err = s.queryDistinctColumn(ctx, "model", from, to)
	if err != nil {
		return FilterOptionsResult{}, err
	}

	s.optionsCache.put(key, out)
	return out, nil
}

// allowedDistinctColumns 白名单，禁止拼接任意列名。
var allowedDistinctColumns = map[string]struct{}{
	"api_key_id": {},
	"provider":   {},
	"model":      {},
}

func (s *DuckDBStore) queryDistinctColumn(ctx context.Context, column string, from, to time.Time) ([]string, bool, error) {
	if _, ok := allowedDistinctColumns[column]; !ok {
		return nil, false, ErrStoreUnavailable
	}
	// LIMIT 201：取 201 条用于截断检测，返回最多 200。
	limit := MaxFilterOptionValues + 1
	q := `
SELECT DISTINCT ` + column + ` AS value
FROM usage_events
WHERE started_at >= ? AND started_at < ?
  AND ` + column + ` IS NOT NULL AND ` + column + ` <> ''
ORDER BY value
LIMIT ` + strconv.Itoa(limit)

	rows, err := s.db.QueryContext(ctx, q, from.UTC(), to.UTC())
	if err != nil {
		return nil, false, ErrStoreUnavailable
	}
	defer rows.Close()

	values := make([]string, 0, MaxFilterOptionValues)
	for rows.Next() {
		var v sql.NullString
		if err := rows.Scan(&v); err != nil {
			return nil, false, ErrStoreUnavailable
		}
		if !v.Valid || v.String == "" {
			continue
		}
		values = append(values, v.String)
	}
	if err := rows.Err(); err != nil {
		return nil, false, ErrStoreUnavailable
	}
	truncated := len(values) > MaxFilterOptionValues
	if truncated {
		values = values[:MaxFilterOptionValues]
	}
	return values, truncated, nil
}
