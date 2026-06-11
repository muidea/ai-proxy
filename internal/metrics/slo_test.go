package metrics

import (
	"sync"
	"testing"
	"time"
)

func TestSLOEvaluatorNilSafe(t *testing.T) {
	var e *SLOEvaluator
	if v := e.Violations(); v != nil {
		t.Fatalf("nil evaluator should return nil violations, got %v", v)
	}
	e.Reset()
	if v := e.CheckNow(); v != nil {
		t.Fatalf("nil evaluator CheckNow should return nil, got %v", v)
	}
	if w := e.Webhook(); w != "" {
		t.Fatalf("nil evaluator Webhook should be empty, got %q", w)
	}
}

func TestSLOEvaluatorDetectsCacheHitRateViolation(t *testing.T) {
	reg := NewRegistry()
	// 20 次请求,只有 1 次 cache 命中,命中率 5%。阈值 0.20 必触发。
	for i := 0; i < 20; i++ {
		reg.RecordRequest("openai", "gpt-4", "chat_completions", 200, time.Millisecond)
		cached := 0
		if i == 0 {
			cached = 10
		}
		reg.RecordTokens("openai", "gpt-4", 100, 50, cached, 0)
	}

	var observed []SLOViolation
	var mu sync.Mutex
	e := NewSLOEvaluator(reg, SLOConfig{
		CacheHitRateMin: 0.20,
	}, "", func(v SLOViolation) {
		mu.Lock()
		observed = append(observed, v)
		mu.Unlock()
	})

	violations := e.CheckNow()
	if len(violations) == 0 {
		t.Fatalf("expected violation, got none")
	}
	if violations[0].Rule != "cache_hit_rate_min" {
		t.Fatalf("rule = %s, want cache_hit_rate_min", violations[0].Rule)
	}
	if violations[0].Provider != "openai" {
		t.Fatalf("provider = %s, want openai", violations[0].Provider)
	}
	if violations[0].Observed >= violations[0].Threshold {
		t.Fatalf("observed %v should be below threshold %v", violations[0].Observed, violations[0].Threshold)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(observed) != len(violations) {
		t.Fatalf("listener fired %d times, want %d", len(observed), len(violations))
	}
}

func TestSLOEvaluatorRespectsErrorRate(t *testing.T) {
	reg := NewRegistry()
	for i := 0; i < 100; i++ {
		status := 200
		if i < 10 {
			status = 500
		}
		reg.RecordRequest("deepseek", "chat", "chat_completions", status, time.Millisecond)
		if status >= 500 {
			reg.RecordUpstreamError("deepseek", 500)
		}
		reg.RecordTokens("deepseek", "chat", 1, 1, 0, 0)
	}

	e := NewSLOEvaluator(reg, SLOConfig{
		UpstreamErrorRateMax: 0.05,
	}, "", nil)
	violations := e.CheckNow()
	if len(violations) == 0 {
		t.Fatalf("expected error rate violation")
	}
	if violations[0].Rule != "upstream_error_rate_max" {
		t.Fatalf("rule = %s, want upstream_error_rate_max", violations[0].Rule)
	}
}

func TestSLOEvaluatorSkipsBelowSampleSize(t *testing.T) {
	reg := NewRegistry()
	for i := 0; i < 5; i++ {
		reg.RecordRequest("openai", "gpt-4", "chat_completions", 500, time.Millisecond)
		reg.RecordUpstreamError("openai", 500)
		reg.RecordTokens("openai", "gpt-4", 1, 1, 0, 0)
	}
	// 5 个样本不达 10 阈值,即便 100% 错误也不应触发。
	e := NewSLOEvaluator(reg, SLOConfig{UpstreamErrorRateMax: 0.05}, "", nil)
	if v := e.CheckNow(); len(v) != 0 {
		t.Fatalf("expected no violation below sample threshold, got %v", v)
	}
}

func TestSLOEvaluatorRespectsP99Latency(t *testing.T) {
	reg := NewRegistry()
	// 注入 20 个样本,p99 约 200ms。
	for i := 1; i <= 20; i++ {
		reg.RecordRequest("anthropic", "claude", "messages", 200, time.Duration(i*10)*time.Millisecond)
	}
	e := NewSLOEvaluator(reg, SLOConfig{P99LatencyMaxMS: 100}, "", nil)
	violations := e.CheckNow()
	if len(violations) == 0 {
		t.Fatalf("expected p99 latency violation")
	}
	if violations[0].Rule != "p99_latency_max_ms" {
		t.Fatalf("rule = %s, want p99_latency_max_ms", violations[0].Rule)
	}
}
