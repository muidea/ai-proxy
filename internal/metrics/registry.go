// Package metrics 提供 ai-proxy 进程内的轻量级指标聚合。
// 不引入 prometheus client_golang,直接手写 minimal exposition format 与
// /stats JSON 序列化。所有方法并发安全(单 mutex 保护 map)。
package metrics

import (
	"sort"
	"strconv"
	"sync"
	"time"
)

// 延迟样本每个 (provider, model) 组合的容量上限;超过会触发降采样。
const latencySamplesCap = 2048

// requestKey 是请求计数/直方图的复合 label。
type requestKey struct {
	Provider, Model, Route, Status string
}

// tokenKey 是 token 计数器的复合 label。
type tokenKey struct {
	Provider, Model string
}

// errorKey 是 upstream 错误计数的复合 label。
type errorKey struct {
	Provider, StatusCode string
}

// fallbackKey 是 fallback 触发的复合 label。
type fallbackKey struct {
	From, To, Reason string
}

// latencyKey 是延迟样本的复合 label。
type latencyKey struct {
	Provider, Model string
}

// Registry 是 metrics 聚合中心。所有记录方法都接收 nil-safe,r == nil 时静默返回。
type Registry struct {
	mu        sync.Mutex
	startedAt time.Time

	requestCount         map[requestKey]uint64
	requestDurationSum   map[requestKey]float64
	requestDurationCount map[requestKey]uint64
	requestDurationMinMS map[requestKey]float64
	requestDurationMaxMS map[requestKey]float64

	latencySamples map[latencyKey][]float64

	inputTokens         map[tokenKey]uint64
	outputTokens        map[tokenKey]uint64
	cachedInputTokens   map[tokenKey]uint64
	cacheCreationTokens map[tokenKey]uint64
	cacheHits           map[tokenKey]uint64
	cacheMisses         map[tokenKey]uint64
	cachedTokenSumHits  map[tokenKey]uint64

	upstreamErrors map[errorKey]uint64

	fallbackAttempts map[fallbackKey]uint64
}

// NewRegistry 构造初始化的 Registry,启动时间设为当前时刻。
func NewRegistry() *Registry {
	return &Registry{
		startedAt:            time.Now(),
		requestCount:         map[requestKey]uint64{},
		requestDurationSum:   map[requestKey]float64{},
		requestDurationCount: map[requestKey]uint64{},
		requestDurationMinMS: map[requestKey]float64{},
		requestDurationMaxMS: map[requestKey]float64{},
		latencySamples:       map[latencyKey][]float64{},
		inputTokens:          map[tokenKey]uint64{},
		outputTokens:         map[tokenKey]uint64{},
		cachedInputTokens:    map[tokenKey]uint64{},
		cacheCreationTokens:  map[tokenKey]uint64{},
		cacheHits:            map[tokenKey]uint64{},
		cacheMisses:          map[tokenKey]uint64{},
		cachedTokenSumHits:   map[tokenKey]uint64{},
		upstreamErrors:       map[errorKey]uint64{},
		fallbackAttempts:     map[fallbackKey]uint64{},
	}
}

// RecordRequest 记录一次完成的请求(包含 duration)。status 经过 statusBucket 归一为 2xx/3xx/4xx/5xx。
func (r *Registry) RecordRequest(provider, model, route string, status int, duration time.Duration) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	key := requestKey{Provider: provider, Model: model, Route: route, Status: statusBucket(status)}
	r.requestCount[key]++
	seconds := duration.Seconds()
	r.requestDurationSum[key] += seconds
	r.requestDurationCount[key]++
	durMS := float64(duration.Milliseconds())
	if existing, ok := r.requestDurationMinMS[key]; !ok || durMS < existing {
		r.requestDurationMinMS[key] = durMS
	}
	if existing, ok := r.requestDurationMaxMS[key]; !ok || durMS > existing {
		r.requestDurationMaxMS[key] = durMS
	}

	latKey := latencyKey{Provider: provider, Model: model}
	samples := r.latencySamples[latKey]
	if len(samples) >= latencySamplesCap {
		// 简单降采样:丢掉最旧的一半,保留尾部供分位数估算。
		samples = samples[latencySamplesCap/2:]
	}
	r.latencySamples[latKey] = append(samples, seconds)
}

// RecordTokens 累计 token 用量,并按 cached_input_tokens>0 判定 cache hit/miss。
// input <= 0 时不计入 hit/miss(避免零值干扰 hit rate)。
func (r *Registry) RecordTokens(provider, model string, input, output, cached, cacheCreation int) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	key := tokenKey{Provider: provider, Model: model}
	if input > 0 {
		r.inputTokens[key] += uint64(input)
	}
	if output > 0 {
		r.outputTokens[key] += uint64(output)
	}
	if cached > 0 {
		r.cachedInputTokens[key] += uint64(cached)
	}
	if cacheCreation > 0 {
		r.cacheCreationTokens[key] += uint64(cacheCreation)
	}
	if input > 0 {
		if cached > 0 {
			r.cacheHits[key]++
			r.cachedTokenSumHits[key] += uint64(cached)
		} else {
			r.cacheMisses[key]++
		}
	}
}

// RecordUpstreamError 累计一次上游错误响应,status_code 保留原始值。
func (r *Registry) RecordUpstreamError(provider string, status int) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.upstreamErrors[errorKey{Provider: provider, StatusCode: strconv.Itoa(status)}]++
}

// RecordFallbackAttempt 累计一次 fallback 触发。reason 可为 status_code 或 "network"。
func (r *Registry) RecordFallbackAttempt(from, to, reason string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.fallbackAttempts[fallbackKey{From: from, To: to, Reason: reason}]++
}

// StartedAt 返回注册表创建时间;r 为 nil 时返回零值。
func (r *Registry) StartedAt() time.Time {
	if r == nil {
		return time.Time{}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.startedAt
}

// quantileSummary 描述一组延迟样本的关键分位数。
type quantileSummary struct {
	P50 float64 `json:"p50"`
	P75 float64 `json:"p75"`
	P90 float64 `json:"p90"`
	P95 float64 `json:"p95"`
	P99 float64 `json:"p99"`
}

// computeQuantiles 对每个 (provider, model) 计算 p50/p75/p90/p95/p99。
// 使用最近 latencySamplesCap 个样本,线性插值式选点(不插值,取 floor 索引)。
func (r *Registry) computeQuantiles() map[latencyKey]quantileSummary {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[latencyKey]quantileSummary, len(r.latencySamples))
	for key, samples := range r.latencySamples {
		if len(samples) == 0 {
			continue
		}
		sorted := make([]float64, len(samples))
		copy(sorted, samples)
		sort.Float64s(sorted)
		out[key] = quantileSummary{
			P50: percentile(sorted, 0.50),
			P75: percentile(sorted, 0.75),
			P90: percentile(sorted, 0.90),
			P95: percentile(sorted, 0.95),
			P99: percentile(sorted, 0.99),
		}
	}
	return out
}

func percentile(sorted []float64, q float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	if len(sorted) == 1 {
		return sorted[0]
	}
	idx := int(float64(len(sorted)-1) * q)
	idx = max(idx, 0)
	idx = min(idx, len(sorted)-1)
	return sorted[idx]
}

// statusBucket 把 HTTP 状态码归一为类,避免 label 基数爆炸。
func statusBucket(status int) string {
	switch {
	case status >= 200 && status < 300:
		return "2xx"
	case status >= 300 && status < 400:
		return "3xx"
	case status >= 400 && status < 500:
		return "4xx"
	case status >= 500 && status < 600:
		return "5xx"
	default:
		return "other"
	}
}

// ContextLike 是 SLO evaluator 周期巡检使用的最小 ctx 抽象。
// 避免直接依赖 context 包,以便单元测试可注入。
type ContextLike interface {
	Done() <-chan struct{}
}

// sloProviderSnapshot 描述单个 provider 的 SLO 评估用快照。
type sloProviderSnapshot struct {
	hits     int64
	misses   int64
	errors   int64
	requests int64
	p99MS    float64
	samples  int
}

// sloSnapshot 是 SLO evaluator 一次评估所需的全部输入。
type sloSnapshot struct {
	byProvider map[string]sloProviderSnapshot
}

// snapshotForSLO 在锁内构造 SLO evaluator 所需快照,完成后立即释放锁。
func (r *Registry) snapshotForSLO() sloSnapshot {
	if r == nil {
		return sloSnapshot{}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	samples := make(map[latencyKey][]float64, len(r.latencySamples))
	for k, v := range r.latencySamples {
		cp := make([]float64, len(v))
		copy(cp, v)
		samples[k] = cp
	}
	requests := map[tokenKey]uint64{}
	for k, v := range r.requestCount {
		// 上游错误与请求都按 (provider, model) 聚合,但 tokenKey 不含 status,
		// 这里用 tokenKey 提供 provider/model 维度近似即可。
		tk := tokenKey{Provider: k.Provider, Model: k.Model}
		requests[tk] += v
	}
	errors := map[tokenKey]uint64{}
	for k, v := range r.upstreamErrors {
		errors[tokenKey{Provider: k.Provider, Model: ""}] += v
	}
	byProvider := map[string]sloProviderSnapshot{}
	for tk, reqs := range requests {
		hits := int64(r.cacheHits[tk])
		misses := int64(r.cacheMisses[tk])
		errs := int64(errors[tokenKey{Provider: tk.Provider, Model: ""}])
		latKey := latencyKey{Provider: tk.Provider, Model: tk.Model}
		latSamples := samples[latKey]
		var p99 float64
		if len(latSamples) > 0 {
			sorted := make([]float64, len(latSamples))
			copy(sorted, latSamples)
			sort.Float64s(sorted)
			p99 = percentile(sorted, 0.99) * 1000
		}
		byProvider[tk.Provider] = sloProviderSnapshot{
			hits:     hits,
			misses:   misses,
			errors:   errs,
			requests: int64(reqs),
			p99MS:    p99,
			samples:  len(latSamples),
		}
	}
	return sloSnapshot{byProvider: byProvider}
}
