package metrics

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestRegistryNilSafe(t *testing.T) {
	var r *Registry
	// 所有 Record* 方法在 r==nil 时必须静默返回,不允许 panic。
	r.RecordRequest("openai", "gpt-4", "chat_completions", 200, 100*time.Millisecond)
	r.RecordTokens("openai", "gpt-4", 10, 5, 0, 0)
	r.RecordUpstreamError("openai", 502)
	r.RecordFallbackAttempt("openai", "deepseek", "502")
	if _, err := r.StatsJSON(); err != nil {
		t.Fatalf("nil registry StatsJSON: %v", err)
	}
	if err := r.WritePrometheus(&strings.Builder{}); err != nil {
		t.Fatalf("nil registry WritePrometheus: %v", err)
	}
}

func TestRegistryCounters(t *testing.T) {
	r := NewRegistry()
	r.RecordRequest("openai", "gpt-4", "chat_completions", 200, 100*time.Millisecond)
	r.RecordRequest("openai", "gpt-4", "chat_completions", 200, 200*time.Millisecond)
	r.RecordRequest("openai", "gpt-4", "chat_completions", 500, 300*time.Millisecond)
	r.RecordTokens("openai", "gpt-4", 100, 50, 30, 5)
	r.RecordTokens("openai", "gpt-4", 200, 100, 0, 0)
	r.RecordUpstreamError("openai", 502)
	r.RecordFallbackAttempt("openai", "deepseek", "502")

	var buf strings.Builder
	if err := r.WritePrometheus(&buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()

	mustContain(t, out, `ai_proxy_requests_total{provider="openai",model="gpt-4",route="chat_completions",status="2xx"} 2`)
	mustContain(t, out, `ai_proxy_requests_total{provider="openai",model="gpt-4",route="chat_completions",status="5xx"} 1`)
	mustContain(t, out, `ai_proxy_input_tokens_total{provider="openai",model="gpt-4"} 300`)
	mustContain(t, out, `ai_proxy_output_tokens_total{provider="openai",model="gpt-4"} 150`)
	mustContain(t, out, `ai_proxy_cached_input_tokens_total{provider="openai",model="gpt-4"} 30`)
	mustContain(t, out, `ai_proxy_cache_creation_input_tokens_total{provider="openai",model="gpt-4"} 5`)
	mustContain(t, out, `ai_proxy_cache_hit_rate{provider="openai",model="gpt-4"} 0.1`)
	mustContain(t, out, `ai_proxy_upstream_errors_total{provider="openai",status_code="502"} 1`)
	mustContain(t, out, `ai_proxy_fallback_attempts_total{from_provider="openai",to_provider="deepseek",reason="502"} 1`)
	mustContain(t, out, "# TYPE ai_proxy_requests_total counter")
	mustContain(t, out, "# EOF")
}

func TestRegistryDurationSummary(t *testing.T) {
	r := NewRegistry()
	r.RecordRequest("p", "m", "chat_completions", 200, 100*time.Millisecond)
	r.RecordRequest("p", "m", "chat_completions", 200, 300*time.Millisecond)
	var buf strings.Builder
	if err := r.WritePrometheus(&buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	mustContain(t, out, `ai_proxy_request_duration_seconds_count{provider="p",model="m",route="chat_completions",status="2xx"} 2`)
	// sum = 0.4s
	if !strings.Contains(out, `ai_proxy_request_duration_seconds_sum{provider="p",model="m",route="chat_completions",status="2xx"} 0.4`) {
		t.Fatalf("expected 0.4 in output: %s", out)
	}
}

func TestRegistryQuantiles(t *testing.T) {
	r := NewRegistry()
	for i := 1; i <= 100; i++ {
		r.RecordRequest("p", "m", "chat_completions", 200, time.Duration(i)*time.Millisecond)
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
	r.RecordRequest("openai", "gpt-4", "chat_completions", 200, 100*time.Millisecond)
	r.RecordRequest("openai", "gpt-4", "chat_completions", 200, 200*time.Millisecond)
	r.RecordRequest("deepseek", "chat", "chat_completions", 200, 50*time.Millisecond)
	r.RecordTokens("openai", "gpt-4", 100, 50, 30, 0)
	r.RecordTokens("openai", "gpt-4", 100, 50, 0, 0)
	r.RecordTokens("deepseek", "chat", 50, 25, 50, 0)
	r.RecordUpstreamError("openai", 502)
	r.RecordUpstreamError("openai", 429)
	r.RecordUpstreamError("deepseek", 500)
	r.RecordFallbackAttempt("openai", "deepseek", "502")

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
	if got.Errors.FallbackTriggered != 1 {
		t.Fatalf("fallback_triggered = %d, want 1", got.Errors.FallbackTriggered)
	}

	if _, ok := got.LatencyMS["openai/gpt-4"]; !ok {
		t.Fatalf("expected latency for openai/gpt-4, got %v", got.LatencyMS)
	}
}

func TestHandlerLoopbackOnly(t *testing.T) {
	r := NewRegistry()
	r.RecordRequest("openai", "gpt-4", "chat_completions", 200, 50*time.Millisecond)
	h := Handler(r, HandlerOptions{AllowRemote: false})

	t.Run("loopback allowed", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
		req.RemoteAddr = "127.0.0.1:51234"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("loopback /metrics status = %d", rec.Code)
		}
		if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/plain") {
			t.Fatalf("metrics content-type = %q", got)
		}
	})

	t.Run("remote denied", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
		req.RemoteAddr = "10.0.0.1:51234"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("remote /metrics status = %d, want 403", rec.Code)
		}
	})

	t.Run("remote allowed via opts", func(t *testing.T) {
		h2 := Handler(r, HandlerOptions{AllowRemote: true})
		req := httptest.NewRequest(http.MethodGet, "/stats", nil)
		req.RemoteAddr = "10.0.0.1:51234"
		rec := httptest.NewRecorder()
		h2.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("remote /stats status = %d, want 200", rec.Code)
		}
		if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
			t.Fatalf("stats content-type = %q", got)
		}
		var snap StatsJSON
		if err := json.NewDecoder(rec.Body).Decode(&snap); err != nil {
			t.Fatalf("decode stats: %v", err)
		}
		if snap.Requests.Total != 1 {
			t.Fatalf("stats requests.total = %d, want 1", snap.Requests.Total)
		}
	})

	t.Run("method not allowed", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/metrics", nil)
		req.RemoteAddr = "127.0.0.1:51234"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("status = %d, want 405", rec.Code)
		}
	})
}

func TestHandlerHead(t *testing.T) {
	r := NewRegistry()
	r.RecordRequest("p", "m", "chat_completions", 200, 50*time.Millisecond)
	h := Handler(r, HandlerOptions{AllowRemote: true})
	req := httptest.NewRequest(http.MethodHead, "/metrics", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("HEAD /metrics status = %d", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("HEAD response should have empty body, got %d bytes", rec.Body.Len())
	}
}

func mustContain(t *testing.T, haystack, needle string) {
	t.Helper()
	if !strings.Contains(haystack, needle) {
		t.Fatalf("expected output to contain %q, got:\n%s", needle, haystack)
	}
}
