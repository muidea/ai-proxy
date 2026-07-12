package metrics

import (
	"fmt"
	"io"
	"maps"
	"sort"
	"strconv"
	"strings"
)

// PrometheusContentType 是 /metrics 端点返回的 Content-Type。
const PrometheusContentType = "text/plain; version=0.0.4; charset=utf-8"

// WritePrometheus 按 minimal exposition format 把 Registry 当前快照写入 w。
// 调用方负责设置 Content-Type。返回的 error 透传 io.Writer 失败。
func (r *Registry) WritePrometheus(w io.Writer) error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	snapshot := prometheusSnapshot{
		requestCount:         copyRequestKeys(r.requestCount),
		requestDurationSum:   copyFloatKeys(r.requestDurationSum),
		requestDurationCount: copyUintKeys(r.requestDurationCount),
		inputTokens:          copyTokenKeys(r.inputTokens),
		outputTokens:         copyTokenKeys(r.outputTokens),
		cachedInputTokens:    copyTokenKeys(r.cachedInputTokens),
		cacheCreationTokens:  copyTokenKeys(r.cacheCreationTokens),
		cacheHitRate:         computeCacheHitRateLocked(r),
		upstreamErrors:       copyErrorKeys(r.upstreamErrors),
		fallbackAttempts:     copyFallbackKeys(r.fallbackAttempts),
	}
	r.mu.Unlock()

	writeCounter(w, "ai_proxy_requests_total",
		"Total number of LLM requests by provider, model, route, HTTP status class, and outcome.",
		snapshot.requestCount, requestKeyLabels)

	writeCounterFloat(w, "ai_proxy_request_duration_seconds_sum",
		"Sum of request durations in seconds, by provider, model, and route.",
		snapshot.requestDurationSum, requestKeyLabels)

	writeCounter(w, "ai_proxy_request_duration_seconds_count",
		"Number of request duration observations, by provider, model, and route.",
		snapshot.requestDurationCount, requestKeyLabels)

	writeCounter(w, "ai_proxy_input_tokens_total",
		"Total input tokens consumed, by provider and model.",
		snapshot.inputTokens, tokenKeyLabels)

	writeCounter(w, "ai_proxy_output_tokens_total",
		"Total output tokens produced, by provider and model.",
		snapshot.outputTokens, tokenKeyLabels)

	writeCounter(w, "ai_proxy_cached_input_tokens_total",
		"Total cached input tokens read, by provider and model.",
		snapshot.cachedInputTokens, tokenKeyLabels)

	writeCounter(w, "ai_proxy_cache_creation_input_tokens_total",
		"Total cache-creation input tokens, by provider and model.",
		snapshot.cacheCreationTokens, tokenKeyLabels)

	writeGauge(w, "ai_proxy_cache_hit_rate",
		"Cache hit rate per provider and model (cached_input_tokens / input_tokens).",
		snapshot.cacheHitRate, tokenKeyLabels)

	writeCounter(w, "ai_proxy_upstream_errors_total",
		"Total upstream error responses, by provider and HTTP status code.",
		snapshot.upstreamErrors, errorKeyLabels)

	writeCounter(w, "ai_proxy_fallback_attempts_total",
		"Total fallback attempts, by from-provider, to-provider, and reason.",
		snapshot.fallbackAttempts, fallbackKeyLabels)

	writeSLOWebhookMetrics(w, r.SLO())

	_, err := io.WriteString(w, "# EOF\n")
	return err
}

// writeSLOWebhookMetrics 输出 SLO webhook 队列/投递指标(evaluator 未挂接时写 0)。
func writeSLOWebhookMetrics(w io.Writer, e *SLOEvaluator) {
	var dropped, ok, errN, non2xx, canceled uint64
	var queue int
	if e != nil {
		dropped = e.WebhookDropped()
		queue = e.WebhookQueueLength()
		ok = e.WebhookRequestCount("ok")
		errN = e.WebhookRequestCount("error")
		non2xx = e.WebhookRequestCount("non_2xx")
		canceled = e.WebhookRequestCount("canceled")
	}

	emitType(w, "ai_proxy_slo_webhook_dropped_total", "counter",
		"Total SLO webhook batches dropped because the queue was full or the evaluator was closed.")
	_, _ = fmt.Fprintf(w, "ai_proxy_slo_webhook_dropped_total %d\n", dropped)

	emitType(w, "ai_proxy_slo_webhook_queue_length", "gauge",
		"Current number of SLO webhook batches waiting in the send queue.")
	_, _ = fmt.Fprintf(w, "ai_proxy_slo_webhook_queue_length %d\n", queue)

	emitType(w, "ai_proxy_slo_webhook_requests_total", "counter",
		"Total SLO webhook delivery attempts by result (ok, error, non_2xx, canceled).")
	_, _ = fmt.Fprintf(w, "ai_proxy_slo_webhook_requests_total{result=\"ok\"} %d\n", ok)
	_, _ = fmt.Fprintf(w, "ai_proxy_slo_webhook_requests_total{result=\"error\"} %d\n", errN)
	_, _ = fmt.Fprintf(w, "ai_proxy_slo_webhook_requests_total{result=\"non_2xx\"} %d\n", non2xx)
	_, _ = fmt.Fprintf(w, "ai_proxy_slo_webhook_requests_total{result=\"canceled\"} %d\n", canceled)
}

type prometheusSnapshot struct {
	requestCount         map[requestKey]uint64
	requestDurationSum   map[requestKey]float64
	requestDurationCount map[requestKey]uint64
	inputTokens          map[tokenKey]uint64
	outputTokens         map[tokenKey]uint64
	cachedInputTokens    map[tokenKey]uint64
	cacheCreationTokens  map[tokenKey]uint64
	cacheHitRate         map[tokenKey]float64
	upstreamErrors       map[errorKey]uint64
	fallbackAttempts     map[fallbackKey]uint64
}

func computeCacheHitRateLocked(r *Registry) map[tokenKey]float64 {
	out := make(map[tokenKey]float64, len(r.inputTokens))
	for k, in := range r.inputTokens {
		cached := r.cachedInputTokens[k]
		if in == 0 {
			continue
		}
		out[k] = float64(cached) / float64(in)
	}
	return out
}

func writeCounter[K comparable](w io.Writer, name, help string, values map[K]uint64, labels func(K) string) {
	emitType(w, name, "counter", help)
	if len(values) == 0 {
		// 仍输出 type/hint,符合 Prometheus 期望
		return
	}
	keys := sortedKeys(values)
	for _, k := range keys {
		_, _ = fmt.Fprintf(w, "%s%s %d\n", name, labels(k), values[k])
	}
}

func writeCounterFloat[K comparable](w io.Writer, name, help string, values map[K]float64, labels func(K) string) {
	emitType(w, name, "counter", help)
	if len(values) == 0 {
		return
	}
	keys := sortedFloatKeys(values)
	for _, k := range keys {
		_, _ = fmt.Fprintf(w, "%s%s %s\n", name, labels(k), formatFloat(values[k]))
	}
}

func writeGauge[K comparable](w io.Writer, name, help string, values map[K]float64, labels func(K) string) {
	emitType(w, name, "gauge", help)
	if len(values) == 0 {
		return
	}
	keys := sortedFloatKeys(values)
	for _, k := range keys {
		_, _ = fmt.Fprintf(w, "%s%s %s\n", name, labels(k), formatFloat(values[k]))
	}
}

func emitType(w io.Writer, name, mtype, help string) {
	_, _ = fmt.Fprintf(w, "# HELP %s %s\n", name, help)
	_, _ = fmt.Fprintf(w, "# TYPE %s %s\n", name, mtype)
}

func requestKeyLabels(k requestKey) string {
	return formatLabels(
		"provider", k.Provider,
		"model", k.Model,
		"route", k.Route,
		"status", k.Status,
		"outcome", k.Outcome,
	)
}

func tokenKeyLabels(k tokenKey) string {
	return formatLabels("provider", k.Provider, "model", k.Model)
}

func errorKeyLabels(k errorKey) string {
	return formatLabels("provider", k.Provider, "status_code", k.StatusCode)
}

func fallbackKeyLabels(k fallbackKey) string {
	return formatLabels("from_provider", k.From, "to_provider", k.To, "reason", k.Reason)
}

func formatLabels(kv ...string) string {
	if len(kv) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteByte('{')
	for i := 0; i+1 < len(kv); i += 2 {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(kv[i])
		b.WriteString(`="`)
		b.WriteString(escapeLabelValue(kv[i+1]))
		b.WriteByte('"')
	}
	b.WriteByte('}')
	return b.String()
}

func escapeLabelValue(value string) string {
	value = strings.ReplaceAll(value, `\`, `\\`)
	value = strings.ReplaceAll(value, `"`, `\"`)
	value = strings.ReplaceAll(value, "\n", `\n`)
	return value
}

func formatFloat(v float64) string {
	if v == 0 {
		return "0"
	}
	abs := v
	if abs < 0 {
		abs = -abs
	}
	// 保留 6 位有效数字,避免长尾 0 污染输出。
	return strconv.FormatFloat(v, 'g', 6, 64)
}

func sortedKeys[K comparable, V any](m map[K]V) []K {
	keys := make([]K, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sortKeys(keys)
	return keys
}

func sortedFloatKeys[K comparable](m map[K]float64) []K {
	keys := make([]K, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sortKeys(keys)
	return keys
}

type sortable interface {
	comparable
	// 依赖类型字符串排序;复合 key 需提供 String()。
}

func sortKeys[K comparable](keys []K) {
	// 用 fmt.Sprint 退化为字符串排序,牺牲一点性能换取简单实现。
	strs := make([]string, len(keys))
	idx := make([]int, len(keys))
	for i, k := range keys {
		strs[i] = fmt.Sprint(k)
		idx[i] = i
	}
	sort.Slice(idx, func(i, j int) bool { return strs[idx[i]] < strs[idx[j]] })
	sorted := make([]K, len(keys))
	for i, j := range idx {
		sorted[i] = keys[j]
	}
	copy(keys, sorted)
}

func copyRequestKeys(src map[requestKey]uint64) map[requestKey]uint64 {
	out := make(map[requestKey]uint64, len(src))
	maps.Copy(out, src)
	return out
}

func copyFloatKeys(src map[requestKey]float64) map[requestKey]float64 {
	out := make(map[requestKey]float64, len(src))
	maps.Copy(out, src)
	return out
}

func copyUintKeys(src map[requestKey]uint64) map[requestKey]uint64 {
	out := make(map[requestKey]uint64, len(src))
	maps.Copy(out, src)
	return out
}

func copyTokenKeys(src map[tokenKey]uint64) map[tokenKey]uint64 {
	out := make(map[tokenKey]uint64, len(src))
	maps.Copy(out, src)
	return out
}

func copyErrorKeys(src map[errorKey]uint64) map[errorKey]uint64 {
	out := make(map[errorKey]uint64, len(src))
	maps.Copy(out, src)
	return out
}

func copyFallbackKeys(src map[fallbackKey]uint64) map[fallbackKey]uint64 {
	out := make(map[fallbackKey]uint64, len(src))
	maps.Copy(out, src)
	return out
}
