package metrics

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestRegistryNilSafe(t *testing.T) {
	var r *Registry
	// 所有 Record* 方法在 r==nil 时必须静默返回,不允许 panic。
	r.RecordRequest("openai", "gpt-4", "chat_completions", 200, 100*time.Millisecond, "success")
	r.RecordTokens("openai", "gpt-4", 10, 5, 0, 0)
	r.RecordUpstreamError("openai", 502)
	if _, err := r.StatsJSON(); err != nil {
		t.Fatalf("nil registry StatsJSON: %v", err)
	}
	if err := r.WritePrometheus(&strings.Builder{}); err != nil {
		t.Fatalf("nil registry WritePrometheus: %v", err)
	}
}

func TestRegistryCounters(t *testing.T) {
	r := NewRegistry()
	r.RecordRequest("openai", "gpt-4", "chat_completions", 200, 100*time.Millisecond, "success")
	r.RecordRequest("openai", "gpt-4", "chat_completions", 200, 200*time.Millisecond, "success")
	r.RecordRequest("openai", "gpt-4", "chat_completions", 500, 300*time.Millisecond, "success")
	r.RecordTokens("openai", "gpt-4", 100, 50, 30, 5)
	r.RecordTokens("openai", "gpt-4", 200, 100, 0, 0)
	r.RecordUpstreamError("openai", 502)

	var buf strings.Builder
	if err := r.WritePrometheus(&buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()

	mustContain(t, out, `ai_proxy_requests_total{provider="openai",model="gpt-4",route="chat_completions",status="2xx",outcome="success",client_endpoint="unknown",upstream_protocol="unknown",upstream_endpoint="unknown",conversion_mode="unknown"} 2`)
	mustContain(t, out, `ai_proxy_requests_total{provider="openai",model="gpt-4",route="chat_completions",status="5xx",outcome="success",client_endpoint="unknown",upstream_protocol="unknown",upstream_endpoint="unknown",conversion_mode="unknown"} 1`)
	mustContain(t, out, `ai_proxy_input_tokens_total{provider="openai",model="gpt-4"} 300`)
	mustContain(t, out, `ai_proxy_output_tokens_total{provider="openai",model="gpt-4"} 150`)
	mustContain(t, out, `ai_proxy_cached_input_tokens_total{provider="openai",model="gpt-4"} 30`)
	mustContain(t, out, `ai_proxy_cache_creation_input_tokens_total{provider="openai",model="gpt-4"} 5`)
	mustContain(t, out, `ai_proxy_cache_hit_rate{provider="openai",model="gpt-4"} 0.1`)
	mustContain(t, out, `ai_proxy_upstream_errors_total{provider="openai",status_code="502"} 1`)
	mustContain(t, out, "# TYPE ai_proxy_requests_total counter")
	mustContain(t, out, "# EOF")
}

func TestClientUsageAndStoreMetrics(t *testing.T) {
	r := NewRegistry()
	r.InitializeClientUsage(map[string]ClientUsage{"default": {Requests: 2, InputTokens: 10, OutputTokens: 5, TotalTokens: 15}})
	r.RecordClientUsage("default", 3, 2)
	r.RecordUsageStoreWriteError("complete")
	r.RecordUsageStoreQuery(20*time.Millisecond, nil, true)
	r.RecordUsageStoreRecovered(4)
	r.RecordUsageStoreCheckpointError()

	var buf strings.Builder
	if err := r.WritePrometheus(&buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	mustContain(t, out, `ai_proxy_client_requests_total{api_key_id="default"} 3`)
	mustContain(t, out, `ai_proxy_client_tokens_total{api_key_id="default"} 20`)
	mustContain(t, out, `ai_proxy_usage_store_write_errors_total{phase="complete"} 1`)
	mustContain(t, out, `ai_proxy_usage_store_recovered_events_total 4`)
	mustContain(t, out, `ai_proxy_usage_store_checkpoint_errors_total 1`)
	mustContain(t, out, `ai_proxy_usage_store_healthy 0`)

	payload, err := r.StatsJSON()
	if err != nil {
		t.Fatal(err)
	}
	var stats StatsJSON
	if err := json.Unmarshal(payload, &stats); err != nil {
		t.Fatal(err)
	}
	if got := stats.Usage.ByAPIKey["default"]; got.Requests != 3 || got.TotalTokens != 20 || stats.Usage.Store.Healthy {
		t.Fatalf("usage stats = %#v", stats.Usage)
	}
}

func TestRegistryDurationSummary(t *testing.T) {
	r := NewRegistry()
	r.RecordRequest("p", "m", "chat_completions", 200, 100*time.Millisecond, "success")
	r.RecordRequest("p", "m", "chat_completions", 200, 300*time.Millisecond, "success")
	var buf strings.Builder
	if err := r.WritePrometheus(&buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	mustContain(t, out, `ai_proxy_request_duration_seconds_count{provider="p",model="m",route="chat_completions",status="2xx",outcome="success",client_endpoint="unknown",upstream_protocol="unknown",upstream_endpoint="unknown",conversion_mode="unknown"} 2`)
	// sum = 0.4s
	if !strings.Contains(out, `ai_proxy_request_duration_seconds_sum{provider="p",model="m",route="chat_completions",status="2xx",outcome="success",client_endpoint="unknown",upstream_protocol="unknown",upstream_endpoint="unknown",conversion_mode="unknown"} 0.4`) {
		t.Fatalf("expected 0.4 in output: %s", out)
	}
}

func TestRegistryQuantiles(t *testing.T) {
	r := NewRegistry()
	for i := 1; i <= 100; i++ {
		r.RecordRequest("p", "m", "chat_completions", 200, time.Duration(i)*time.Millisecond, "success")
	}
	summary := r.computeQuantiles()
	got, ok := summary[latencyKey{Provider: "p", Model: "m"}]
	if !ok {
		t.Fatalf("expected quantiles for p/m, got %#v", summary)
	}
	if got.P50 < 0.04 || got.P50 > 0.06 {
		t.Fatalf("p50 = %v, want ~0.05s", got.P50)
	}
	if got.P99 < 0.09 || got.P99 > 0.11 {
		t.Fatalf("p99 = %v, want ~0.10s", got.P99)
	}
}

func TestStatsJSONShape(t *testing.T) {
	r := NewRegistry()
	r.RecordRequest("openai", "gpt-4", "chat_completions", 200, 100*time.Millisecond, "success")
	r.RecordRequest("openai", "gpt-4", "chat_completions", 200, 200*time.Millisecond, "success")
	r.RecordRequest("deepseek", "chat", "chat_completions", 200, 50*time.Millisecond, "success")
	r.RecordTokens("openai", "gpt-4", 100, 50, 30, 0)
	r.RecordTokens("openai", "gpt-4", 100, 50, 0, 0)
	r.RecordTokens("deepseek", "chat", 50, 25, 50, 0)
	r.RecordUpstreamError("openai", 502)
	r.RecordUpstreamError("openai", 429)
	r.RecordUpstreamError("deepseek", 500)

	payload, err := r.StatsJSON()
	if err != nil {
		t.Fatal(err)
	}
	var got StatsJSON
	if err := json.Unmarshal(payload, &got); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, payload)
	}

	if got.Requests.Total != 3 {
		t.Fatalf("total = %d, want 3", got.Requests.Total)
	}
	if got.Requests.ByProvider["openai"] != 2 {
		t.Fatalf("openai count = %d, want 2", got.Requests.ByProvider["openai"])
	}
	if got.Requests.ByStatus["2xx"] != 3 {
		t.Fatalf("2xx count = %d, want 3", got.Requests.ByStatus["2xx"])
	}

	openai := got.Cache.ByProvider["openai"]
	if openai.Hit != 1 || openai.Miss != 1 {
		t.Fatalf("openai cache hit/miss = %d/%d, want 1/1", openai.Hit, openai.Miss)
	}
	if openai.HitRate != 0.5 {
		t.Fatalf("openai hit_rate = %v, want 0.5", openai.HitRate)
	}
	if openai.AvgCachedTokens != 30 {
		t.Fatalf("openai avg_cached_tokens = %v, want 30", openai.AvgCachedTokens)
	}
	deepseek := got.Cache.ByProvider["deepseek"]
	if deepseek.HitRate != 1 {
		t.Fatalf("deepseek hit_rate = %v, want 1.0", deepseek.HitRate)
	}

	if got.Errors.Upstream5xx != 2 {
		t.Fatalf("upstream_5xx = %d, want 2", got.Errors.Upstream5xx)
	}
	if got.Errors.UpstreamRateLimit != 1 {
		t.Fatalf("upstream_rate_limit = %d, want 1", got.Errors.UpstreamRateLimit)
	}
	if _, ok := got.LatencyMS["openai/gpt-4"]; !ok {
		t.Fatalf("expected latency for openai/gpt-4, got %v", got.LatencyMS)
	}
}

func mustContain(t *testing.T, haystack, needle string) {
	t.Helper()
	if !strings.Contains(haystack, needle) {
		t.Fatalf("expected output to contain %q, got:\n%s", needle, haystack)
	}
}

func TestModelLabelCardinalityCapped(t *testing.T) {
	reg := NewRegistry()
	// 写入超过上限的 model,后续应归为 _other。
	for i := 0; i < maxModelsPerProvider+10; i++ {
		model := "gpt-model-" + strconv.Itoa(i)
		reg.RecordRequest("openai", model, "chat_completions", 200, time.Millisecond, "success")
		reg.RecordTokens("openai", model, 1, 1, 0, 0)
	}
	// 通过 Stats 或内部快照验证 knownModels 大小与 _other 存在。
	reg.mu.Lock()
	defer reg.mu.Unlock()
	if got := len(reg.knownModels["openai"]); got != maxModelsPerProvider {
		t.Fatalf("known models = %d, want %d", got, maxModelsPerProvider)
	}
	// 应存在 _other 维度的请求计数
	foundOther := false
	for k := range reg.requestCount {
		if k.Provider == "openai" && k.Model == otherModelLabel {
			foundOther = true
			break
		}
	}
	if !foundOther {
		t.Fatal("expected _other model label after cardinality overflow")
	}
}

func TestUpstreamAttemptLatencySeparateFromCompleted(t *testing.T) {
	reg := NewRegistry()
	reg.RecordUpstreamAttempt("p", 500*time.Millisecond, AttemptHeader)
	reg.RecordRequest("p", "m", "chat_completions", 200, 5*time.Millisecond, "success")
	reg.mu.Lock()
	defer reg.mu.Unlock()
	if len(reg.upstreamHeaderLatency["p"]) != 1 {
		t.Fatalf("upstream samples = %d", len(reg.upstreamHeaderLatency["p"]))
	}
	// 完成请求延迟不应写入 attempt map
	latKey := latencyKey{Provider: "p", Model: "m"}
	if len(reg.latencySamples[latKey]) != 1 {
		t.Fatalf("completed latency samples = %d", len(reg.latencySamples[latKey]))
	}
	// 空 model key 不应被 attempt 占用
	empty := latencyKey{Provider: "p", Model: ""}
	if len(reg.latencySamples[empty]) != 0 {
		t.Fatalf("empty-model latency should be unused, got %d", len(reg.latencySamples[empty]))
	}
}
