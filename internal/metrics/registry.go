// Package metrics 提供 ai-proxy 进程内的轻量级指标聚合。
// 不引入 prometheus client_golang,直接手写 minimal exposition format 与
// /stats JSON 序列化。所有方法并发安全(单 mutex 保护 map)。
package metrics

import (
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// 延迟样本每个 (provider, model) 组合的容量上限;超过会触发降采样。
const latencySamplesCap = 2048

// maxModelsPerProvider 限制每个 provider 下独立 model label 数量。
// 超出后新 model 归一为 otherModelLabel,防止客户端通过通配路由刷爆 map。
const maxModelsPerProvider = 64

// otherModelLabel 是超出基数上限后的聚合标签。
const otherModelLabel = "_other"

// requestKey 是请求计数/直方图的复合 label。
// Outcome 描述业务结果(完整枚举):
//
//	success | client_canceled | idle_timeout | limit_exceeded |
//	upstream_truncated | upstream_failed | incomplete |
//	client_write | conversion | protocol | error
//
// 流式首包 200 后中途失败时 Status 仍可能是 2xx,Outcome 用于区分真实成败。
// 计入 upstream error rate 的: upstream_truncated, upstream_failed, idle_timeout, protocol(上游损坏)。
type requestKey struct {
	Provider, Model, Route, Status, Outcome string
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

	// upstreamHeaderLatency 非流式/仅响应头时间的 attempt 延迟。
	upstreamHeaderLatency map[string][]float64
	// upstreamFirstEventLatency 流式含首行探测的 attempt 延迟;SLO p99 优先使用。
	upstreamFirstEventLatency map[string][]float64

	inputTokens         map[tokenKey]uint64
	outputTokens        map[tokenKey]uint64
	cachedInputTokens   map[tokenKey]uint64
	cacheCreationTokens map[tokenKey]uint64
	cacheHits           map[tokenKey]uint64
	cacheMisses         map[tokenKey]uint64
	cachedTokenSumHits  map[tokenKey]uint64

	upstreamErrors   map[errorKey]uint64
	upstreamAttempts map[string]uint64 // provider -> total attempts

	fallbackAttempts map[fallbackKey]uint64

	// knownModels 记录每个 provider 已见过的 model label(不含 _other),用于基数限制。
	knownModels map[string]map[string]struct{} // provider -> set(model)

	// slo 可选挂接:用于把 webhook 队列/投递指标暴露到 /metrics。
	// 用 atomic.Pointer 避免与 metrics 记录路径争用同一把锁。
	slo atomic.Pointer[SLOEvaluator]
}

// NewRegistry 构造初始化的 Registry,启动时间设为当前时刻。
func NewRegistry() *Registry {
	return &Registry{
		startedAt:                 time.Now(),
		requestCount:              map[requestKey]uint64{},
		requestDurationSum:        map[requestKey]float64{},
		requestDurationCount:      map[requestKey]uint64{},
		requestDurationMinMS:      map[requestKey]float64{},
		requestDurationMaxMS:      map[requestKey]float64{},
		latencySamples:            map[latencyKey][]float64{},
		upstreamHeaderLatency:     map[string][]float64{},
		upstreamFirstEventLatency: map[string][]float64{},
		inputTokens:               map[tokenKey]uint64{},
		outputTokens:              map[tokenKey]uint64{},
		cachedInputTokens:         map[tokenKey]uint64{},
		cacheCreationTokens:       map[tokenKey]uint64{},
		cacheHits:                 map[tokenKey]uint64{},
		cacheMisses:               map[tokenKey]uint64{},
		cachedTokenSumHits:        map[tokenKey]uint64{},
		upstreamErrors:            map[errorKey]uint64{},
		upstreamAttempts:          map[string]uint64{},
		fallbackAttempts:          map[fallbackKey]uint64{},
		knownModels:               map[string]map[string]struct{}{},
	}
}

// normalizeModelLabel 在已持锁前提下限制 model label 基数。
// 空 model 保持为空;已登记 model 原样返回;新 model 在未超限时登记,否则归为 _other。
func (r *Registry) normalizeModelLabel(provider, model string) string {
	if model == "" || model == otherModelLabel {
		return model
	}
	set := r.knownModels[provider]
	if set == nil {
		set = map[string]struct{}{}
		r.knownModels[provider] = set
	}
	if _, ok := set[model]; ok {
		return model
	}
	if len(set) >= maxModelsPerProvider {
		return otherModelLabel
	}
	set[model] = struct{}{}
	return model
}

// ReserveModels 预登记应优先占用 model label 槽位的模型(catalog / 精确 models)。
// 在接受动态通配流量前调用,避免随机 model 先占满 64 槽。
func (r *Registry) ReserveModels(provider string, models []string) {
	if r == nil || provider == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	set := r.knownModels[provider]
	if set == nil {
		set = map[string]struct{}{}
		r.knownModels[provider] = set
	}
	for _, model := range models {
		model = strings.TrimSpace(model)
		if model == "" || model == otherModelLabel {
			continue
		}
		// 通配模式不预占槽位。
		if strings.Contains(model, "*") {
			continue
		}
		if _, ok := set[model]; ok {
			continue
		}
		if len(set) >= maxModelsPerProvider {
			return
		}
		set[model] = struct{}{}
	}
}

// RecordRequest 记录一次完成的请求(包含 duration)。
// status 归一为 2xx/3xx/4xx/5xx;outcome 描述业务结果(空则 success)。
func (r *Registry) RecordRequest(provider, model, route string, status int, duration time.Duration, outcome string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	model = r.normalizeModelLabel(provider, model)
	if outcome == "" {
		outcome = "success"
	}
	key := requestKey{Provider: provider, Model: model, Route: route, Status: statusBucket(status), Outcome: outcome}
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

	// 完成请求延迟按 (provider, model) 记录,与 attempt 延迟分离。
	latKey := latencyKey{Provider: provider, Model: model}
	samples := r.latencySamples[latKey]
	if len(samples) >= latencySamplesCap {
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
	model = r.normalizeModelLabel(provider, model)
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

// AttemptLatencyKind 区分 attempt 延迟语义。
type AttemptLatencyKind string

const (
	// AttemptHeader 仅到响应头(非流式 body 下载前 / 流式未含首包)。
	AttemptHeader AttemptLatencyKind = "header"
	// AttemptFirstEvent 到首个 SSE 行(含探测),流式成功路径使用;SLO p99 优先此项。
	AttemptFirstEvent AttemptLatencyKind = "first_event"
)

// RecordUpstreamAttempt 累计一次上游尝试,并按 kind 写入对应延迟样本。
// SLO p99 优先 first_event,否则 header;不与完成请求 latency 混用。
func (r *Registry) RecordUpstreamAttempt(provider string, duration time.Duration, kind AttemptLatencyKind) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.upstreamAttempts[provider]++
	var bucket map[string][]float64
	switch kind {
	case AttemptFirstEvent:
		bucket = r.upstreamFirstEventLatency
	default:
		bucket = r.upstreamHeaderLatency
	}
	samples := bucket[provider]
	if len(samples) >= latencySamplesCap {
		samples = samples[latencySamplesCap/2:]
	}
	bucket[provider] = append(samples, duration.Seconds())
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

// AttachSLO 挂接 SLOEvaluator,使其 webhook 队列/投递计数暴露到 /metrics。
// e 为 nil 时清除挂接。可重复调用。
func (r *Registry) AttachSLO(e *SLOEvaluator) {
	if r == nil {
		return
	}
	r.slo.Store(e)
}

// SLO 返回当前挂接的 evaluator(可能为 nil)。
func (r *Registry) SLO() *SLOEvaluator {
	if r == nil {
		return nil
	}
	return r.slo.Load()
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
// 按 provider 聚合所有 model 的请求、缓存与延迟样本,避免不同 model 互相覆盖。
func (r *Registry) snapshotForSLO() sloSnapshot {
	if r == nil {
		return sloSnapshot{}
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	// SLO p99:每个 provider 优先 first_event,其次 header,再回退完成请求延迟。
	samplesByProvider := map[string][]float64{}
	copySamples := func(src map[string][]float64) {
		for p, v := range src {
			if len(v) == 0 || len(samplesByProvider[p]) > 0 {
				continue
			}
			cp := make([]float64, len(v))
			copy(cp, v)
			samplesByProvider[p] = cp
		}
	}
	copySamples(r.upstreamFirstEventLatency)
	copySamples(r.upstreamHeaderLatency)
	// 按 provider 回退完成请求延迟(仅该 provider 尚无 attempt 样本时)。
	completedByProv := map[string][]float64{}
	for k, v := range r.latencySamples {
		if len(v) == 0 {
			continue
		}
		cp := make([]float64, len(v))
		copy(cp, v)
		completedByProv[k.Provider] = append(completedByProv[k.Provider], cp...)
	}
	for p, v := range completedByProv {
		if len(samplesByProvider[p]) == 0 && len(v) > 0 {
			samplesByProvider[p] = v
		}
	}

	// 完成请求计数(最终返回)按 provider 汇总。
	completedByProvider := map[string]uint64{}
	for k, v := range r.requestCount {
		completedByProvider[k.Provider] += v
	}

	// 上游 attempt 总数作为错误率分母;若尚未记录 attempt 则回退到完成请求数。
	requestsByProvider := map[string]uint64{}
	for p, v := range r.upstreamAttempts {
		requestsByProvider[p] = v
	}
	for p, v := range completedByProvider {
		if requestsByProvider[p] == 0 {
			requestsByProvider[p] = v
		}
	}

	// 上游错误按 provider 汇总
	errorsByProvider := map[string]uint64{}
	for k, v := range r.upstreamErrors {
		errorsByProvider[k.Provider] += v
	}

	// 缓存命中/未命中按 provider 汇总
	hitsByProvider := map[string]uint64{}
	missesByProvider := map[string]uint64{}
	for k, v := range r.cacheHits {
		hitsByProvider[k.Provider] += v
	}
	for k, v := range r.cacheMisses {
		missesByProvider[k.Provider] += v
	}

	// 收集所有出现过的 provider 名
	providers := map[string]struct{}{}
	for p := range requestsByProvider {
		providers[p] = struct{}{}
	}
	for p := range r.upstreamAttempts {
		providers[p] = struct{}{}
	}
	for p := range errorsByProvider {
		providers[p] = struct{}{}
	}
	for p := range hitsByProvider {
		providers[p] = struct{}{}
	}
	for p := range missesByProvider {
		providers[p] = struct{}{}
	}
	for p := range samplesByProvider {
		providers[p] = struct{}{}
	}

	byProvider := map[string]sloProviderSnapshot{}
	for provider := range providers {
		latSamples := samplesByProvider[provider]
		var p99 float64
		if len(latSamples) > 0 {
			sorted := make([]float64, len(latSamples))
			copy(sorted, latSamples)
			sort.Float64s(sorted)
			p99 = percentile(sorted, 0.99) * 1000
		}
		byProvider[provider] = sloProviderSnapshot{
			hits:     int64(hitsByProvider[provider]),
			misses:   int64(missesByProvider[provider]),
			errors:   int64(errorsByProvider[provider]),
			requests: int64(requestsByProvider[provider]),
			p99MS:    p99,
			samples:  len(latSamples),
		}
	}
	return sloSnapshot{byProvider: byProvider}
}
