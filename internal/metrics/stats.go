package metrics

import (
	"encoding/json"
	"math"
	"time"
)

// StatsJSON 是 /stats 端点返回的 JSON 负载。
type StatsJSON struct {
	UptimeSeconds int64                      `json:"uptime_seconds"`
	Requests      StatsRequests              `json:"requests"`
	Cache         StatsCache                 `json:"cache"`
	LatencyMS     map[string]quantileSummary `json:"latency_ms"`
	Errors        StatsErrors                `json:"errors"`
}

// StatsRequests 汇总请求计数。
type StatsRequests struct {
	Total      int64            `json:"total"`
	ByProvider map[string]int64 `json:"by_provider"`
	ByStatus   map[string]int64 `json:"by_status"`
}

// StatsCache 汇总缓存命中统计。
type StatsCache struct {
	ByProvider map[string]StatsCacheProvider `json:"by_provider"`
}

// StatsCacheProvider 是单个 provider 的 cache 统计。
type StatsCacheProvider struct {
	Hit             int64   `json:"hit"`
	Miss            int64   `json:"miss"`
	HitRate         float64 `json:"hit_rate"`
	AvgCachedTokens float64 `json:"avg_cached_tokens"`
}

// StatsErrors 汇总错误统计。
type StatsErrors struct {
	Upstream5xx       int64            `json:"upstream_5xx"`
	UpstreamTimeout   int64            `json:"upstream_timeout"`
	UpstreamRateLimit int64            `json:"upstream_rate_limit"`
	FallbackTriggered int64            `json:"fallback_triggered"`
	ByStatusCode      map[string]int64 `json:"upstream_by_status_code"`
}

// StatsJSON 返回当前 Registry 快照的 JSON 字节数组。r 为 nil 时返回 null。
func (r *Registry) StatsJSON() ([]byte, error) {
	if r == nil {
		return []byte("null"), nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	snapshot := buildStatsLocked(r)
	return json.Marshal(snapshot)
}

func buildStatsLocked(r *Registry) StatsJSON {
	requests := StatsRequests{
		ByProvider: map[string]int64{},
		ByStatus:   map[string]int64{},
	}
	for k, v := range r.requestCount {
		requests.Total += int64(v)
		requests.ByProvider[k.Provider] += int64(v)
		requests.ByStatus[k.Status] += int64(v)
	}

	cache := StatsCache{ByProvider: map[string]StatsCacheProvider{}}
	hitsByProvider := map[string]int64{}
	missesByProvider := map[string]int64{}
	cachedSumByProvider := map[string]uint64{}
	for k, hits := range r.cacheHits {
		hitsByProvider[k.Provider] += int64(hits)
	}
	for k, misses := range r.cacheMisses {
		missesByProvider[k.Provider] += int64(misses)
	}
	for k, sumHits := range r.cachedTokenSumHits {
		cachedSumByProvider[k.Provider] += sumHits
	}
	allProviders := map[string]struct{}{}
	for p := range hitsByProvider {
		allProviders[p] = struct{}{}
	}
	for p := range missesByProvider {
		allProviders[p] = struct{}{}
	}
	for p := range cachedSumByProvider {
		allProviders[p] = struct{}{}
	}
	for provider := range allProviders {
		hit := hitsByProvider[provider]
		miss := missesByProvider[provider]
		sumHits := cachedSumByProvider[provider]
		bucket := StatsCacheProvider{Hit: hit, Miss: miss}
		if total := hit + miss; total > 0 {
			bucket.HitRate = roundTo(float64(hit)/float64(total), 4)
		}
		if hit > 0 {
			bucket.AvgCachedTokens = roundTo(float64(sumHits)/float64(hit), 4)
		}
		cache.ByProvider[provider] = bucket
	}

	errors := StatsErrors{ByStatusCode: map[string]int64{}}
	for k, v := range r.upstreamErrors {
		count := int64(v)
		errors.ByStatusCode[k.StatusCode] += count
		switch k.StatusCode {
		case "500", "501", "502", "503", "504", "505", "506", "507", "508", "510", "511":
			errors.Upstream5xx += count
		case "408":
			errors.UpstreamTimeout += count
		case "429":
			errors.UpstreamRateLimit += count
		}
	}
	for k, v := range r.fallbackAttempts {
		errors.FallbackTriggered += int64(v)
		_ = k
	}

	latency := make(map[string]quantileSummary, len(r.latencySamples))
	for key, samples := range r.latencySamples {
		if len(samples) == 0 {
			continue
		}
		sorted := make([]float64, len(samples))
		copy(sorted, samples)
		// 复制后直接排序(短暂释放锁仅发生在 sort 期间,这里保持锁内安全)
		// 性能上 samples 量级 2048,sort 成本可接受。
		sortFloats(sorted)
		latency[latencyLabel(key)] = quantileSummary{
			P50: roundTo(percentile(sorted, 0.50)*1000, 3),
			P75: roundTo(percentile(sorted, 0.75)*1000, 3),
			P90: roundTo(percentile(sorted, 0.90)*1000, 3),
			P95: roundTo(percentile(sorted, 0.95)*1000, 3),
			P99: roundTo(percentile(sorted, 0.99)*1000, 3),
		}
	}

	uptime := int64(0)
	if !r.startedAt.IsZero() {
		uptime = int64(time.Since(r.startedAt).Seconds())
	}

	return StatsJSON{
		UptimeSeconds: uptime,
		Requests:      requests,
		Cache:         cache,
		LatencyMS:     latency,
		Errors:        errors,
	}
}

func latencyLabel(k latencyKey) string {
	if k.Model == "" {
		return k.Provider
	}
	return k.Provider + "/" + k.Model
}

func roundTo(v float64, digits int) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	mult := math.Pow10(digits)
	return math.Round(v*mult) / mult
}

// sortFloats 是 sort.Float64s 的内联版,避免引入 sort 包名。
func sortFloats(s []float64) {
	// 用插入排序:2048 规模下足够快,且更可预测。
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}
