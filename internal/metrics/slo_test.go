package metrics

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// syncBuffer 保护并发日志写入,避免 race detector 在异步 webhook 测试中误报。
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

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
		reg.RecordRequest("openai", "gpt-4", "chat_completions", 200, time.Millisecond, "success")
		cached := 0
		if i == 0 {
			cached = 10
		}
		reg.RecordTokens("openai", "gpt-4", 100, 50, cached, 0)
	}

	var observed []SLOStateChange
	var mu sync.Mutex
	e := NewSLOEvaluator(reg, SLOConfig{
		CacheHitRateMin: 0.20,
	}, "", func(ev SLOStateChange) {
		mu.Lock()
		observed = append(observed, ev)
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
	for _, ev := range observed {
		if ev.State != SLOStateEntered {
			t.Fatalf("listener state = %q, want %q", ev.State, SLOStateEntered)
		}
	}
}

func TestSLOEvaluatorRespectsErrorRate(t *testing.T) {
	reg := NewRegistry()
	for i := 0; i < 100; i++ {
		status := 200
		if i < 10 {
			status = 500
		}
		reg.RecordRequest("deepseek", "chat", "chat_completions", status, time.Millisecond, "success")
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
		reg.RecordRequest("openai", "gpt-4", "chat_completions", 500, time.Millisecond, "success")
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
		reg.RecordRequest("anthropic", "claude", "messages", 200, time.Duration(i*10)*time.Millisecond, "success")
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

func TestSnapshotForSLOAggregatesModels(t *testing.T) {
	reg := NewRegistry()
	// 同一 provider 两个 model:合计 20 请求,10 次 cache hit。
	for i := 0; i < 10; i++ {
		reg.RecordRequest("openai", "gpt-a", "chat_completions", 200, time.Millisecond, "success")
		reg.RecordTokens("openai", "gpt-a", 10, 1, 5, 0)
	}
	for i := 0; i < 10; i++ {
		reg.RecordRequest("openai", "gpt-b", "chat_completions", 200, time.Millisecond, "success")
		reg.RecordTokens("openai", "gpt-b", 10, 1, 0, 0)
	}
	snap := reg.snapshotForSLO()
	s, ok := snap.byProvider["openai"]
	if !ok {
		t.Fatalf("missing openai snapshot: %#v", snap.byProvider)
	}
	if s.requests != 20 {
		t.Fatalf("requests = %d, want 20", s.requests)
	}
	if s.hits != 10 || s.misses != 10 {
		t.Fatalf("hits=%d misses=%d", s.hits, s.misses)
	}
	// 命中率 0.5,阈值 0.6 应触发
	e := NewSLOEvaluator(reg, SLOConfig{CacheHitRateMin: 0.6}, "", nil)
	v := e.CheckNow()
	if len(v) == 0 {
		t.Fatalf("expected cache hit rate violation after multi-model aggregation")
	}
}

func TestSLOUsesUpstreamAttemptsAsDenominator(t *testing.T) {
	reg := NewRegistry()
	// 构造 primary 失败与 backup 成功的独立 attempt 样本，验证分母按 provider 统计。
	for i := 0; i < 20; i++ {
		reg.RecordUpstreamAttempt("primary", 100*time.Millisecond, AttemptHeader)
		reg.RecordUpstreamError("primary", 500)
		reg.RecordUpstreamAttempt("backup", 10*time.Millisecond, AttemptHeader)
		reg.RecordRequest("backup", "m", "chat_completions", 200, time.Millisecond, "success")
		reg.RecordTokens("backup", "m", 1, 1, 0, 0)
	}
	e := NewSLOEvaluator(reg, SLOConfig{UpstreamErrorRateMax: 0.1}, "", nil)
	violations := e.CheckNow()
	found := false
	for _, v := range violations {
		if v.Provider == "primary" && v.Rule == "upstream_error_rate_max" {
			found = true
			if v.Observed < 0.99 {
				t.Fatalf("primary observed error rate = %v, want ~1.0", v.Observed)
			}
		}
	}
	if !found {
		t.Fatalf("expected primary attempt-based error rate violation, got %#v", violations)
	}
}

func TestSLOP99UsesAttemptLatency(t *testing.T) {
	reg := NewRegistry()
	// 构造 primary 慢失败与 backup 快成功的独立 attempt 样本，验证 p99 按 provider 统计。
	for i := 0; i < 20; i++ {
		reg.RecordUpstreamAttempt("primary", 500*time.Millisecond, AttemptHeader)
		reg.RecordUpstreamError("primary", 504)
		reg.RecordUpstreamAttempt("backup", 5*time.Millisecond, AttemptHeader)
		reg.RecordRequest("backup", "m", "chat_completions", 200, 5*time.Millisecond, "success")
	}
	e := NewSLOEvaluator(reg, SLOConfig{P99LatencyMaxMS: 100}, "", nil)
	violations := e.CheckNow()
	found := false
	for _, v := range violations {
		if v.Provider == "primary" && v.Rule == "p99_latency_max_ms" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected primary p99 violation from attempt latency, got %#v", violations)
	}
}

func TestSLOViolationHistoryBounded(t *testing.T) {
	reg := NewRegistry()
	e := NewSLOEvaluator(reg, SLOConfig{UpstreamErrorRateMax: 0.01}, "", nil)
	// 状态变化语义下每个 provider 只记一次进入;填满历史上限。
	// 先批量写入所有 provider 指标,再逐个 CheckNow 触发 enter(仍 O(n) evaluate)。
	n := maxViolationHistory + 5
	for i := 0; i < n; i++ {
		p := "provider-" + strconv.Itoa(i)
		for j := 0; j < 10; j++ {
			reg.RecordRequest(p, "m", "chat_completions", 500, time.Millisecond, "error")
			reg.RecordUpstreamAttempt(p, time.Millisecond, AttemptHeader)
			reg.RecordUpstreamError(p, 500)
		}
	}
	for i := 0; i < n; i++ {
		// 每次 CheckNow 会把所有当前违规视为 active;为得到多条 history,
		// 需要逐个 provider 首次进入。这里通过临时只保留一个新 provider 的方式不现实。
		// 直接调用:先 CheckNow 一次会把所有 provider 同时 enter。
		break
	}
	entered := e.CheckNow()
	if len(entered) < maxViolationHistory {
		t.Fatalf("entered = %d, want >= %d", len(entered), maxViolationHistory)
	}
	if got := len(e.Violations()); got > maxViolationHistory {
		t.Fatalf("violations = %d, want <= %d", got, maxViolationHistory)
	}
	if got := len(e.Violations()); got != maxViolationHistory {
		t.Fatalf("violations = %d, want exactly cap %d", got, maxViolationHistory)
	}
}

func TestSLOWebhookPostsJSON(t *testing.T) {
	var hits atomic.Int32
	var gotEntered atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.Method != http.MethodPost {
			t.Errorf("method = %s", r.Method)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("content-type = %s", ct)
		}
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		var payload SLOWebhookPayload
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Errorf("json: %v body=%s", err, body)
		}
		if len(payload.Entered) == 0 {
			t.Errorf("empty entered: %#v", payload)
		} else {
			gotEntered.Store(int32(len(payload.Entered)))
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	reg := NewRegistry()
	for i := 0; i < 20; i++ {
		reg.RecordRequest("openai", "m", "chat_completions", 500, time.Millisecond, "error")
		reg.RecordUpstreamAttempt("openai", time.Millisecond, AttemptHeader)
		reg.RecordUpstreamError("openai", 500)
		reg.RecordTokens("openai", "m", 1, 1, 0, 0)
	}
	e := NewSLOEvaluator(reg, SLOConfig{UpstreamErrorRateMax: 0.01}, srv.URL, nil)
	defer e.Close()
	violations := e.CheckNow()
	if len(violations) == 0 {
		t.Fatal("expected entered violations")
	}
	// 持续违规不应再 webhook
	if second := e.CheckNow(); len(second) != 0 {
		t.Fatalf("second check should not re-enter: %v", second)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if hits.Load() >= 1 {
			if gotEntered.Load() < 1 {
				t.Fatal("payload entered empty")
			}
			// 第二次 CheckNow 不应再投递
			time.Sleep(50 * time.Millisecond)
			if hits.Load() != 1 {
				t.Fatalf("expected exactly 1 webhook for sustained violation, got %d", hits.Load())
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("webhook hits = %d, want >= 1", hits.Load())
}

func TestRedactWebhookURL(t *testing.T) {
	got := redactWebhookURL("https://hooks.slack.com/services/T00/B00/SECRET?x=1")
	if got != "https://hooks.slack.com" {
		t.Fatalf("redacted = %q", got)
	}
	if redactWebhookURL("not a url") != "<invalid-webhook-url>" {
		t.Fatal("invalid")
	}
	if redactWebhookURL("") != "" {
		t.Fatal("empty")
	}
}

func TestSanitizeWebhookError(t *testing.T) {
	// *url.Error 形态:完整 URL 在 Error() 中,但 sanitize 后不应残留 secret
	raw := &url.Error{
		Op:  "Post",
		URL: "https://hooks.example/services/SECRET?token=xxx",
		Err: context.DeadlineExceeded,
	}
	got := sanitizeWebhookError(raw)
	if strings.Contains(got, "SECRET") || strings.Contains(got, "token=xxx") || strings.Contains(got, "hooks.example/services") {
		t.Fatalf("secret leaked in sanitized error: %q", got)
	}
	if !strings.Contains(got, "Post") || !strings.Contains(got, "deadline") {
		t.Fatalf("expected op+underlying, got %q", got)
	}
	// 普通错误原样返回
	if sanitizeWebhookError(io.EOF) != "EOF" {
		t.Fatalf("eof = %q", sanitizeWebhookError(io.EOF))
	}
}

func TestSLOWebhookFailureLogDoesNotLeakSecret(t *testing.T) {
	var buf syncBuffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(prev)

	// 先 listen 再立刻关闭,得到 connection refused(立即失败,不必等满 3s timeout)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	secretURL := "http://" + addr + "/services/T00/B00/SUPER_SECRET_TOKEN?x=1"

	reg := NewRegistry()
	for i := 0; i < 20; i++ {
		reg.RecordRequest("openai", "m", "chat_completions", 500, time.Millisecond, "error")
		reg.RecordUpstreamAttempt("openai", time.Millisecond, AttemptHeader)
		reg.RecordUpstreamError("openai", 500)
		reg.RecordTokens("openai", "m", 1, 1, 0, 0)
	}
	e := NewSLOEvaluator(reg, SLOConfig{UpstreamErrorRateMax: 0.01}, secretURL, nil)
	defer e.Close()
	if len(e.CheckNow()) == 0 {
		t.Fatal("expected violations")
	}
	// 等待异步 webhook 失败日志
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(buf.String(), "slo webhook post failed") {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	logs := buf.String()
	if !strings.Contains(logs, "slo webhook post failed") {
		t.Fatalf("expected post failed log, got: %s", logs)
	}
	if strings.Contains(logs, "SUPER_SECRET_TOKEN") || strings.Contains(logs, "/services/") {
		t.Fatalf("secret leaked in logs: %s", logs)
	}
	// 应有脱敏 host(scheme://host),不应含 path
	if !strings.Contains(logs, "127.0.0.1") {
		t.Fatalf("expected redacted host in logs: %s", logs)
	}
}

func TestSLOWebhookDoesNotFollowRedirect(t *testing.T) {
	var finalHits, redirectHits atomic.Int32
	final := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		finalHits.Add(1)
		// 若被跟随且改成 GET,body 为空
		body, _ := io.ReadAll(r.Body)
		if r.Method != http.MethodPost {
			t.Errorf("final method = %s (redirect may have rewritten POST)", r.Method)
		}
		if len(body) == 0 {
			t.Error("final received empty body")
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer final.Close()

	var redirectStatus = http.StatusFound // 302
	redir := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redirectHits.Add(1)
		http.Redirect(w, r, final.URL+"/sink", redirectStatus)
	}))
	defer redir.Close()

	reg := NewRegistry()
	for i := 0; i < 20; i++ {
		reg.RecordRequest("openai", "m", "chat_completions", 500, time.Millisecond, "error")
		reg.RecordUpstreamAttempt("openai", time.Millisecond, AttemptHeader)
		reg.RecordUpstreamError("openai", 500)
		reg.RecordTokens("openai", "m", 1, 1, 0, 0)
	}
	e := NewSLOEvaluator(reg, SLOConfig{UpstreamErrorRateMax: 0.01}, redir.URL, nil)
	defer e.Close()
	if len(e.CheckNow()) == 0 {
		t.Fatal("expected violations")
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if redirectHits.Load() >= 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	// 给一点时间看是否被错误跟随
	time.Sleep(100 * time.Millisecond)
	if redirectHits.Load() < 1 {
		t.Fatal("redirect endpoint not hit")
	}
	if finalHits.Load() != 0 {
		t.Fatalf("webhook followed redirect to final server (hits=%d); CheckRedirect should block", finalHits.Load())
	}
}

func TestSLOWebhookNon2xxLogged(t *testing.T) {
	var buf syncBuffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(prev)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("nope"))
	}))
	defer srv.Close()

	reg := NewRegistry()
	for i := 0; i < 20; i++ {
		reg.RecordRequest("openai", "m", "chat_completions", 500, time.Millisecond, "error")
		reg.RecordUpstreamAttempt("openai", time.Millisecond, AttemptHeader)
		reg.RecordUpstreamError("openai", 500)
		reg.RecordTokens("openai", "m", 1, 1, 0, 0)
	}
	e := NewSLOEvaluator(reg, SLOConfig{UpstreamErrorRateMax: 0.01}, srv.URL, nil)
	defer e.Close()
	_ = e.CheckNow()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(buf.String(), "slo webhook non-2xx") {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.Contains(buf.String(), "slo webhook non-2xx") {
		t.Fatalf("expected non-2xx log, got: %s", buf.String())
	}
	if !strings.Contains(buf.String(), "status=500") && !strings.Contains(buf.String(), "status\":500") {
		// text handler: status=500
		if !strings.Contains(buf.String(), "500") {
			t.Fatalf("expected status in log: %s", buf.String())
		}
	}
}

// blockingRoundTripper 占住 worker:直到 request context 取消才返回。
// 用于确定性填满队列,不依赖真实网络时序。
type blockingRoundTripper struct{}

func (blockingRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	<-req.Context().Done()
	return nil, req.Context().Err()
}

// seedActive 让直接 enqueue 的 entered 在 reconcile 后仍可投递。
// 会分配 generation 与 EventID,与 applyState 语义一致。
func seedActive(e *SLOEvaluator, v SLOViolation) SLOViolation {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.genCounter++
	v.Generation = e.genCounter
	e.gen[violationKey(v)] = v.Generation
	v.EventID = violationEventID(e.instanceID, v.Provider, v.Rule, v.Generation, SLOStateEntered)
	e.active[violationKey(v)] = v
	return v
}

func testViolation(provider, rule string) SLOViolation {
	return SLOViolation{Provider: provider, Rule: rule, Observed: 1, Threshold: 0}
}

func TestSLOWebhookQueueDropWhenFull(t *testing.T) {
	// 单 worker + 阻塞 transport:1 在途 + 64 队列后必丢。
	client := &http.Client{
		Transport: blockingRoundTripper{},
		Timeout:   0,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	e := newSLOEvaluator(NewRegistry(), SLOConfig{}, "http://example.invalid/hook", nil, client)
	defer e.Close()

	v := seedActive(e, testViolation("p", "r"))
	// 1 worker + 64 队列;再多必丢。
	for i := 0; i < webhookMaxConcurrent+webhookQueueSize+16; i++ {
		e.enqueueWebhook(SLOWebhookPayload{
			At:      time.Now(),
			Seq:     uint64(i + 1),
			Entered: []SLOViolation{v},
		})
	}
	// 等 worker 取走并阻塞在 HTTP
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && e.WebhookDropped() == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	if e.WebhookDropped() == 0 {
		t.Fatal("expected webhook drops when worker blocked and queue full")
	}
	if q := e.WebhookQueueLength(); q == 0 {
		t.Fatal("expected non-empty queue while worker blocked")
	}
}

func TestSLOWebhookCloseCancelsInFlight(t *testing.T) {
	// Close 应取消在途 HTTP,剩余队列计入 dropped,queue_length 归零。
	client := &http.Client{
		Transport: blockingRoundTripper{},
		Timeout:   0,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	e := newSLOEvaluator(NewRegistry(), SLOConfig{}, "http://example.invalid/hook", nil, client)

	v := seedActive(e, testViolation("p", "r"))
	total := webhookMaxConcurrent + 8
	for i := 0; i < total; i++ {
		e.enqueueWebhook(SLOWebhookPayload{
			At:      time.Now(),
			Seq:     uint64(i + 1),
			Entered: []SLOViolation{v},
		})
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && e.WebhookQueueLength() >= total {
		time.Sleep(5 * time.Millisecond)
	}

	start := time.Now()
	e.Close()
	elapsed := time.Since(start)
	if elapsed > 500*time.Millisecond {
		t.Fatalf("Close took %v, want fast cancel (<500ms)", elapsed)
	}
	if q := e.WebhookQueueLength(); q != 0 {
		t.Fatalf("queue_length after Close = %d, want 0", q)
	}
	if e.WebhookDropped() == 0 {
		t.Fatal("expected remaining batches counted as dropped on Close")
	}
	before := e.WebhookDropped()
	e.enqueueWebhook(SLOWebhookPayload{At: time.Now(), Entered: []SLOViolation{testViolation("x", "y")}})
	if e.WebhookDropped() <= before {
		t.Fatal("expected drop after Close")
	}
}

// failNThenOKRoundTripper 前 failN 次返回 500,之后 204。
type failNThenOKRoundTripper struct {
	failN int32
	hits  atomic.Int32
}

func (f *failNThenOKRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	n := f.hits.Add(1)
	if n <= f.failN {
		return &http.Response{
			StatusCode: http.StatusInternalServerError,
			Body:       io.NopCloser(strings.NewReader("fail")),
			Header:     make(http.Header),
			Request:    req,
		}, nil
	}
	return &http.Response{
		StatusCode: http.StatusNoContent,
		Body:       io.NopCloser(strings.NewReader("")),
		Header:     make(http.Header),
		Request:    req,
	}, nil
}

// statusRoundTripper 返回固定状态码,可选 Retry-After。
type statusRoundTripper struct {
	status     int
	retryAfter string
	hits       atomic.Int32
}

func (s *statusRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	s.hits.Add(1)
	h := make(http.Header)
	if s.retryAfter != "" {
		h.Set("Retry-After", s.retryAfter)
	}
	return &http.Response{
		StatusCode: s.status,
		Body:       io.NopCloser(strings.NewReader("x")),
		Header:     h,
		Request:    req,
	}, nil
}

// flushUntilOK 周期性 flushUndelivered(不走 CheckNow,避免 seedActive 被 evaluate 清空),直到成功或超时。
func flushUntilOK(t *testing.T, e *SLOEvaluator, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if e.WebhookRequestCount("ok") >= 1 {
			return
		}
		e.flushUndelivered()
		time.Sleep(30 * time.Millisecond)
	}
	t.Fatalf("timeout waiting ok; non2xx=%d error=%d queue=%d dropped=%d",
		e.WebhookRequestCount("non_2xx"), e.WebhookRequestCount("error"),
		e.WebhookQueueLength(), e.WebhookDropped())
}

func TestSLOWebhookRetriesThenSucceeds(t *testing.T) {
	// 2 次 500 后成功:单次 attempt 失败进 undelivered,CheckNow flush 后继续,最终 ok。
	rt := &failNThenOKRoundTripper{failN: 2}
	client := &http.Client{Transport: rt, Timeout: 0}
	e := newSLOEvaluator(NewRegistry(), SLOConfig{}, "http://example.invalid/hook", nil, client)
	defer e.Close()

	v := seedActive(e, testViolation("p", "r"))
	e.enqueueWebhook(SLOWebhookPayload{At: time.Now(), Seq: 1, Entered: []SLOViolation{v}})
	flushUntilOK(t, e, 3*time.Second)
	if rt.hits.Load() != 3 {
		t.Fatalf("hits=%d, want 3 (2 fail + 1 ok)", rt.hits.Load())
	}
	if e.WebhookQueueLength() != 0 {
		t.Fatalf("queue should be empty after success, got %d", e.WebhookQueueLength())
	}
}

func TestSLOWebhookUndeliveredRedelivered(t *testing.T) {
	// 阶段 1: 一次 500 → undelivered(未耗尽)
	// 阶段 2: 换成成功 transport + CheckNow flush → 投递成功
	// listener 只在状态变化时触发,不因重投再响。
	failRT := &statusRoundTripper{status: 500}
	client := &http.Client{Transport: failRT, Timeout: 0}
	reg := NewRegistry()
	for i := 0; i < 20; i++ {
		reg.RecordRequest("openai", "m", "chat_completions", 500, time.Millisecond, "error")
		reg.RecordUpstreamAttempt("openai", time.Millisecond, AttemptHeader)
		reg.RecordUpstreamError("openai", 500)
	}
	var listenerHits atomic.Int32
	e := newSLOEvaluator(reg, SLOConfig{UpstreamErrorRateMax: 0.01}, "http://example.invalid/hook", func(ev SLOStateChange) {
		listenerHits.Add(1)
	}, client)
	defer e.Close()

	if len(e.CheckNow()) == 0 {
		t.Fatal("expected enter")
	}
	// 等首次 attempt 失败进入 undelivered
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if e.WebhookRequestCount("non_2xx") >= 1 && e.WebhookQueueLength() >= 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if e.WebhookRequestCount("ok") != 0 {
		t.Fatal("should not succeed while always 500")
	}
	if e.WebhookQueueLength() < 1 {
		t.Fatalf("expected undelivered backlog, queue=%d non2xx=%d",
			e.WebhookQueueLength(), e.WebhookRequestCount("non_2xx"))
	}
	if listenerHits.Load() != 1 {
		t.Fatalf("listener hits=%d, want 1 (entered only)", listenerHits.Load())
	}

	// 切换成功 transport;等 NextRetry 后 flush
	okRT := &failNThenOKRoundTripper{failN: 0}
	e.client.Transport = okRT
	time.Sleep(webhookRetryBase + 50*time.Millisecond)
	if second := e.CheckNow(); len(second) != 0 {
		t.Fatalf("sustained should not re-enter: %v", second)
	}
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if e.WebhookRequestCount("ok") >= 1 {
			break
		}
		_ = e.CheckNow()
		time.Sleep(20 * time.Millisecond)
	}
	if e.WebhookRequestCount("ok") < 1 {
		t.Fatalf("expected redelivery success, ok=%d queue=%d", e.WebhookRequestCount("ok"), e.WebhookQueueLength())
	}
	if listenerHits.Load() != 1 {
		t.Fatalf("redelivery must not re-fire listener, hits=%d", listenerHits.Load())
	}
}

func TestSLOWebhookStaleEnteredDroppedOnResolve(t *testing.T) {
	// entered 投递失败进入 undelivered 后状态恢复:
	// flush 时对账丢弃过期 entered,只成功投递 resolved。
	var mu sync.Mutex
	var success []SLOWebhookPayload
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var p SLOWebhookPayload
		_ = json.Unmarshal(body, &p)
		n := hits.Add(1)
		if n == 1 {
			// 首次:entered 失败
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		mu.Lock()
		success = append(success, p)
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	reg := NewRegistry()
	for i := 0; i < 20; i++ {
		reg.RecordRequest("openai", "m", "chat_completions", 500, time.Millisecond, "error")
		reg.RecordUpstreamAttempt("openai", time.Millisecond, AttemptHeader)
		reg.RecordUpstreamError("openai", 500)
	}
	e := NewSLOEvaluator(reg, SLOConfig{UpstreamErrorRateMax: 0.01}, srv.URL, nil)
	defer e.Close()

	if len(e.CheckNow()) == 0 {
		t.Fatal("expected enter")
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if e.WebhookRequestCount("non_2xx") >= 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if e.WebhookRequestCount("non_2xx") < 1 {
		t.Fatal("expected entered delivery failure")
	}

	// 恢复
	e.config.UpstreamErrorRateMax = 1.0
	time.Sleep(webhookRetryBase + 50*time.Millisecond)
	_ = e.CheckNow() // 丢弃过期 entered + 入队 resolved

	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if e.WebhookRequestCount("ok") >= 1 {
			break
		}
		_ = e.CheckNow()
		time.Sleep(20 * time.Millisecond)
	}
	if e.WebhookRequestCount("ok") < 1 {
		t.Fatalf("expected resolved delivered, ok=%d dropped=%d", e.WebhookRequestCount("ok"), e.WebhookDropped())
	}
	mu.Lock()
	defer mu.Unlock()
	if len(success) == 0 {
		t.Fatal("no successful payloads")
	}
	for _, p := range success {
		if len(p.Entered) > 0 {
			t.Fatalf("stale entered must not be delivered after resolve: %#v", p)
		}
		if len(p.Resolved) == 0 {
			t.Fatalf("expected resolved in success payload: %#v", p)
		}
	}
	// 过期 entered 应对账计入 dropped
	if e.WebhookDropped() == 0 {
		t.Fatal("expected stale entered counted as dropped")
	}
}

func TestSLOWebhook429RetryAfter(t *testing.T) {
	rt := &statusRoundTripper{status: http.StatusTooManyRequests, retryAfter: "1"}
	client := &http.Client{Transport: rt, Timeout: 0}
	e := newSLOEvaluator(NewRegistry(), SLOConfig{}, "http://example.invalid/hook", nil, client)
	defer e.Close()

	v := seedActive(e, testViolation("p", "r"))
	e.enqueueWebhook(SLOWebhookPayload{At: time.Now(), Seq: 1, Entered: []SLOViolation{v}})

	// 等 429 进入 undelivered
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if e.WebhookRequestCount("non_2xx") >= 1 && e.WebhookQueueLength() >= 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if e.WebhookRequestCount("non_2xx") < 1 {
		t.Fatal("expected 429 counted as non_2xx retryable")
	}
	// 直接 flush,勿 CheckNow(会把 seedActive 误判为 resolved)
	e.flushUndelivered()
	time.Sleep(20 * time.Millisecond)
	if rt.hits.Load() != 1 {
		t.Fatalf("hits=%d, want 1 before Retry-After elapses", rt.hits.Load())
	}
	// 换成成功,等 Retry-After 后 flush
	okRT := &failNThenOKRoundTripper{failN: 0}
	e.client.Transport = okRT
	time.Sleep(1100 * time.Millisecond)
	e.flushUndelivered()
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if e.WebhookRequestCount("ok") >= 1 {
			return
		}
		e.flushUndelivered()
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("expected success after Retry-After, ok=%d queue=%d", e.WebhookRequestCount("ok"), e.WebhookQueueLength())
}

func TestParseRetryAfter(t *testing.T) {
	if d := parseRetryAfter("2"); d != 2*time.Second {
		t.Fatalf("got %v", d)
	}
	if d := parseRetryAfter("9999"); d != webhookRetryAfterMax {
		t.Fatalf("cap got %v", d)
	}
	if d := parseRetryAfter("nope"); d != 0 {
		t.Fatalf("invalid got %v", d)
	}
	if d := parseRetryAfter(""); d != 0 {
		t.Fatalf("empty got %v", d)
	}
	// 极大整数:乘法前裁剪,不得溢出为负
	if d := parseRetryAfter(strconv.FormatInt(math.MaxInt64, 10)); d != webhookRetryAfterMax {
		t.Fatalf("MaxInt64 got %v, want cap %v", d, webhookRetryAfterMax)
	}
	// HTTP-date: 未来 5s
	future := time.Now().UTC().Add(5 * time.Second).Format(http.TimeFormat)
	d := parseRetryAfter(future)
	if d < 3*time.Second || d > 5*time.Second {
		t.Fatalf("http-date duration = %v, want ~5s", d)
	}
	// HTTP-date: 过去 → 0
	past := time.Now().UTC().Add(-time.Hour).Format(http.TimeFormat)
	if d := parseRetryAfter(past); d != 0 {
		t.Fatalf("past http-date = %v, want 0", d)
	}
	// HTTP-date 超上限裁剪
	far := time.Now().UTC().Add(2 * time.Hour).Format(http.TimeFormat)
	if d := parseRetryAfter(far); d != webhookRetryAfterMax {
		t.Fatalf("far http-date = %v, want cap %v", d, webhookRetryAfterMax)
	}
}

func TestSLOWebhookStaleGenerationNotReplayed(t *testing.T) {
	// 时序: gen1 entered 失败 → resolve → gen2 entered 成功;
	// 旧 gen1 entered 到期 flush 时必须对账丢弃,不得重放。
	var mu sync.Mutex
	var success []SLOWebhookPayload
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var p SLOWebhookPayload
		_ = json.Unmarshal(body, &p)
		n := hits.Add(1)
		if n == 1 {
			// 第一次: gen1 entered 失败
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		mu.Lock()
		success = append(success, p)
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	reg := NewRegistry()
	for i := 0; i < 20; i++ {
		reg.RecordRequest("openai", "m", "chat_completions", 500, time.Millisecond, "error")
		reg.RecordUpstreamAttempt("openai", time.Millisecond, AttemptHeader)
		reg.RecordUpstreamError("openai", 500)
	}
	e := NewSLOEvaluator(reg, SLOConfig{UpstreamErrorRateMax: 0.01}, srv.URL, nil)
	defer e.Close()

	// gen1 enter
	if len(e.CheckNow()) == 0 {
		t.Fatal("expected enter gen1")
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if e.WebhookRequestCount("non_2xx") >= 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if e.WebhookRequestCount("non_2xx") < 1 {
		t.Fatal("expected gen1 entered delivery failure")
	}
	gen1 := e.Active()[0].Generation
	if gen1 == 0 {
		t.Fatal("expected non-zero generation")
	}

	// resolve
	e.config.UpstreamErrorRateMax = 1.0
	time.Sleep(webhookRetryBase + 50*time.Millisecond)
	_ = e.CheckNow() // flush 丢弃 gen1 entered + 入队 resolved

	// 等 resolved 送达
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if e.WebhookRequestCount("ok") >= 1 {
			break
		}
		_ = e.CheckNow()
		time.Sleep(20 * time.Millisecond)
	}
	if e.WebhookRequestCount("ok") < 1 {
		t.Fatalf("expected resolved ok, got %d", e.WebhookRequestCount("ok"))
	}

	// 再次违规 → gen2 entered
	e.config.UpstreamErrorRateMax = 0.01
	entered2 := e.CheckNow()
	if len(entered2) == 0 {
		t.Fatal("expected re-enter gen2")
	}
	if entered2[0].Generation == gen1 {
		t.Fatalf("gen2 should differ from gen1: both %d", gen1)
	}
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if e.WebhookRequestCount("ok") >= 2 {
			break
		}
		_ = e.CheckNow()
		time.Sleep(20 * time.Millisecond)
	}
	if e.WebhookRequestCount("ok") < 2 {
		t.Fatalf("expected gen2 entered delivered, ok=%d", e.WebhookRequestCount("ok"))
	}

	mu.Lock()
	defer mu.Unlock()
	for _, p := range success {
		for _, v := range p.Entered {
			if v.Generation == gen1 {
				t.Fatalf("stale gen1 entered replayed: %#v", p)
			}
		}
	}
}

func TestCheckNowSerializesListenerOrder(t *testing.T) {
	// 并发 CheckNow 时 listener 顺序仍应 entered → resolved,且各只一次。
	reg := NewRegistry()
	for i := 0; i < 20; i++ {
		reg.RecordRequest("openai", "m", "chat_completions", 500, time.Millisecond, "error")
		reg.RecordUpstreamAttempt("openai", time.Millisecond, AttemptHeader)
		reg.RecordUpstreamError("openai", 500)
	}
	var mu sync.Mutex
	var states []string
	e := NewSLOEvaluator(reg, SLOConfig{UpstreamErrorRateMax: 0.01}, "", func(ev SLOStateChange) {
		mu.Lock()
		states = append(states, ev.State)
		mu.Unlock()
	})

	// 先 enter
	if len(e.CheckNow()) == 0 {
		t.Fatal("expected enter")
	}

	start := make(chan struct{})
	var wg sync.WaitGroup
	// 一批 goroutine 持续 CheckNow(违规中,应无新事件)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for j := 0; j < 30; j++ {
				_ = e.CheckNow()
			}
		}()
	}
	// 一个 goroutine 在 checkMu 保护下改阈值后 resolve
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		// 与 CheckNow 同一把锁,避免 config 读写数据竞争
		e.checkMu.Lock()
		e.config.UpstreamErrorRateMax = 1.0
		e.checkMu.Unlock()
		for j := 0; j < 30; j++ {
			_ = e.CheckNow()
		}
	}()
	close(start)
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	// 应恰好 1 次 entered + 1 次 resolved,且 entered 在前
	if len(states) != 2 {
		t.Fatalf("states=%v, want [entered resolved]", states)
	}
	if states[0] != SLOStateEntered || states[1] != SLOStateResolved {
		t.Fatalf("states=%v, want entered then resolved", states)
	}
}

func TestSLOWebhookMultiRulePartialRedeliveryReseq(t *testing.T) {
	// 多规则批次部分仍有效时,重投须分配新 seq,避免消费者按倒序拒绝策略丢弃有效 entered。
	// 时序: A+B entered(seq1) 失败 → A resolve(seq2) 成功 → B entered 重投时 seq>2。
	var mu sync.Mutex
	var delivered []SLOWebhookPayload
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var p SLOWebhookPayload
		_ = json.Unmarshal(body, &p)
		n := hits.Add(1)
		if n == 1 {
			// 首次:A+B entered 失败
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		mu.Lock()
		delivered = append(delivered, p)
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	reg := NewRegistry()
	// 两个 provider 同时违规
	for _, p := range []string{"provA", "provB"} {
		for i := 0; i < 20; i++ {
			reg.RecordRequest(p, "m", "chat_completions", 500, time.Millisecond, "error")
			reg.RecordUpstreamAttempt(p, time.Millisecond, AttemptHeader)
			reg.RecordUpstreamError(p, 500)
		}
	}
	e := NewSLOEvaluator(reg, SLOConfig{UpstreamErrorRateMax: 0.01}, srv.URL, nil)
	defer e.Close()

	entered := e.CheckNow()
	if len(entered) < 2 {
		t.Fatalf("expected enter A+B, got %d", len(entered))
	}
	// 等首次失败进 undelivered
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if e.WebhookRequestCount("non_2xx") >= 1 && e.WebhookQueueLength() >= 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if e.WebhookRequestCount("non_2xx") < 1 {
		t.Fatal("expected first batch delivery failure")
	}

	// 仅恢复 provA:通过超大阈值无法单 provider 恢复,改用清空 A 的指标不可行。
	// 直接操作 active:模拟 A 已 resolve,B 仍 active。
	e.mu.Lock()
	var aKey string
	for k := range e.active {
		if strings.HasPrefix(k, "provA|") {
			aKey = k
			break
		}
	}
	if aKey == "" {
		e.mu.Unlock()
		t.Fatal("missing provA active")
	}
	aViol := e.active[aKey]
	delete(e.active, aKey)
	// gen 保留供 resolved 对账
	instanceID := e.instanceID
	e.mu.Unlock()
	aViol.EventID = violationEventID(instanceID, aViol.Provider, aViol.Rule, aViol.Generation, SLOStateResolved)

	// 入队 A resolved(新 seq),并 flush 旧批次
	time.Sleep(webhookRetryBase + 50*time.Millisecond)
	e.enqueueWebhook(e.newPayload(nil, []SLOViolation{aViol}))
	e.flushUndelivered()

	// 等至少 2 次成功:resolved A + redelivered B entered
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if e.WebhookRequestCount("ok") >= 2 {
			break
		}
		e.flushUndelivered()
		time.Sleep(20 * time.Millisecond)
	}
	if e.WebhookRequestCount("ok") < 2 {
		t.Fatalf("ok=%d, want >=2 delivered=%v", e.WebhookRequestCount("ok"), deliveredSnapshot(&mu, delivered))
	}

	mu.Lock()
	defer mu.Unlock()
	var resolvedSeq, reenteredSeq uint64
	for _, p := range delivered {
		for _, v := range p.Resolved {
			if v.Provider == "provA" {
				resolvedSeq = p.Seq
			}
		}
		for _, v := range p.Entered {
			if v.Provider == "provB" {
				reenteredSeq = p.Seq
			}
		}
	}
	if resolvedSeq == 0 {
		t.Fatalf("missing A resolved in delivered: %#v", delivered)
	}
	if reenteredSeq == 0 {
		t.Fatalf("missing B entered redelivery: %#v", delivered)
	}
	if reenteredSeq <= resolvedSeq {
		t.Fatalf("redelivered B seq=%d must be > A resolved seq=%d (reseq on flush)", reenteredSeq, resolvedSeq)
	}
	// 重投 B 批次不得再含 A entered
	for _, p := range delivered {
		if p.Seq == reenteredSeq {
			for _, v := range p.Entered {
				if v.Provider == "provA" {
					t.Fatalf("stale A entered in redelivered batch: %#v", p)
				}
			}
		}
	}
}

func TestSLOWebhookEventIDStableAcrossRetry(t *testing.T) {
	// 远端处理成功但返回 5xx:重投时各条 EventID 不变,Seq 递增;InstanceID 不变。
	var mu sync.Mutex
	var seen []SLOWebhookPayload
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var p SLOWebhookPayload
		_ = json.Unmarshal(body, &p)
		mu.Lock()
		seen = append(seen, p)
		mu.Unlock()
		n := hits.Add(1)
		if n == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	reg := NewRegistry()
	for i := 0; i < 20; i++ {
		reg.RecordRequest("openai", "m", "chat_completions", 500, time.Millisecond, "error")
		reg.RecordUpstreamAttempt("openai", time.Millisecond, AttemptHeader)
		reg.RecordUpstreamError("openai", 500)
	}
	e := NewSLOEvaluator(reg, SLOConfig{UpstreamErrorRateMax: 0.01}, srv.URL, nil)
	defer e.Close()
	if e.InstanceID() == "" {
		t.Fatal("instance_id empty")
	}

	if len(e.CheckNow()) == 0 {
		t.Fatal("expected enter")
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if e.WebhookRequestCount("non_2xx") >= 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(webhookRetryBase + 50*time.Millisecond)
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if e.WebhookRequestCount("ok") >= 1 {
			break
		}
		_ = e.CheckNow()
		time.Sleep(20 * time.Millisecond)
	}
	if e.WebhookRequestCount("ok") < 1 {
		t.Fatal("expected successful redelivery")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) < 2 {
		t.Fatalf("want >=2 deliveries, got %d", len(seen))
	}
	first, second := seen[0], seen[len(seen)-1]
	if first.InstanceID == "" || first.InstanceID != second.InstanceID {
		t.Fatalf("instance_id mismatch: %q vs %q", first.InstanceID, second.InstanceID)
	}
	if first.InstanceID != e.InstanceID() {
		t.Fatalf("instance_id = %q, want %q", first.InstanceID, e.InstanceID())
	}
	if len(first.Entered) == 0 || len(second.Entered) == 0 {
		t.Fatalf("missing entered: first=%#v second=%#v", first, second)
	}
	if first.Entered[0].EventID == "" || first.Entered[0].EventID != second.Entered[0].EventID {
		t.Fatalf("event_id changed across retry: %q -> %q", first.Entered[0].EventID, second.Entered[0].EventID)
	}
	if second.Seq <= first.Seq {
		t.Fatalf("seq must increase on redelivery: %d -> %d", first.Seq, second.Seq)
	}
}

func TestSLOWebhookInstanceIDChangesOnRestart(t *testing.T) {
	// 重启契约:新 evaluator 的 instance_id 不同,seq 可从 1 重新开始。
	// 格式:32 个十六进制字符 = 16 字节随机熵(128 bit)。
	e1 := NewSLOEvaluator(NewRegistry(), SLOConfig{}, "", nil)
	e2 := NewSLOEvaluator(NewRegistry(), SLOConfig{}, "", nil)
	for _, id := range []string{e1.InstanceID(), e2.InstanceID()} {
		if id == "" {
			t.Fatal("empty instance_id")
		}
		if len(id) != 32 {
			t.Fatalf("instance_id hex length = %d, want 32 (16 bytes): %q", len(id), id)
		}
		raw, err := hex.DecodeString(id)
		if err != nil {
			t.Fatalf("instance_id not hex: %q: %v", id, err)
		}
		if len(raw) != 16 {
			t.Fatalf("instance_id decoded len = %d, want 16", len(raw))
		}
	}
	if e1.InstanceID() == e2.InstanceID() {
		t.Fatalf("instance_id should differ across evaluators: %q", e1.InstanceID())
	}
}

func TestNewInstanceIDFormat(t *testing.T) {
	// 连续采样:格式稳定且高概率互异。
	seen := map[string]struct{}{}
	for i := 0; i < 32; i++ {
		id := newInstanceID()
		if len(id) != 32 {
			t.Fatalf("len=%d id=%q", len(id), id)
		}
		raw, err := hex.DecodeString(id)
		if err != nil || len(raw) != 16 {
			t.Fatalf("decode %q: %v len=%d", id, err, len(raw))
		}
		if _, dup := seen[id]; dup {
			t.Fatalf("duplicate instance_id in 32 samples: %q", id)
		}
		seen[id] = struct{}{}
	}
}

func TestNewInstanceIDFallbackFormat(t *testing.T) {
	// crypto/rand 失败时仍输出 32 hex 字符,且同批多次互异。
	// hex.DecodeString 已排除旧 fallback 的非 hex 前缀 "t..."。
	failRead := func([]byte) (int, error) {
		return 0, io.ErrUnexpectedEOF
	}
	seen := map[string]struct{}{}
	for i := 0; i < 8; i++ {
		id := newInstanceIDFrom(failRead)
		if len(id) != 32 {
			t.Fatalf("fallback len=%d id=%q", len(id), id)
		}
		raw, err := hex.DecodeString(id)
		if err != nil || len(raw) != 16 {
			t.Fatalf("fallback decode %q: %v len=%d", id, err, len(raw))
		}
		if _, dup := seen[id]; dup {
			t.Fatalf("fallback duplicate: %q", id)
		}
		seen[id] = struct{}{}
	}
}

func TestNewInstanceIDShortReadFallback(t *testing.T) {
	// 短读且 err==nil 不得当作完整随机 ID;应走 fallback 且格式仍为 32 hex。
	// 若误用短读结果,ID 解码后会是 0xab 后跟 15 个 0。
	shortRead := func(b []byte) (int, error) {
		if len(b) == 0 {
			return 0, nil
		}
		b[0] = 0xab
		return 1, nil // n < 16, err == nil
	}
	id := newInstanceIDFrom(shortRead)
	if len(id) != 32 {
		t.Fatalf("len=%d id=%q", len(id), id)
	}
	raw, err := hex.DecodeString(id)
	if err != nil || len(raw) != 16 {
		t.Fatalf("decode %q: %v", id, err)
	}
	allZeroTail := true
	for i := 1; i < len(raw); i++ {
		if raw[i] != 0 {
			allZeroTail = false
			break
		}
	}
	if raw[0] == 0xab && allZeroTail {
		t.Fatalf("short-read accepted as full random id: %q", id)
	}
	// 两次短读 fallback 应因计数器不同而互异
	id2 := newInstanceIDFrom(shortRead)
	if id == id2 {
		t.Fatalf("fallback should differ across calls: %q", id)
	}
}

func TestSLOWebhookPartialReconcileKeepsPerRuleEventID(t *testing.T) {
	// 多规则 batch 对账后只剩 B:B 的 EventID 应与首次进入时一致,不得复用 batch 级 ID。
	var mu sync.Mutex
	var firstEntered map[string]string // provider -> event_id
	var redelivered []SLOViolation
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var p SLOWebhookPayload
		_ = json.Unmarshal(body, &p)
		n := hits.Add(1)
		if n == 1 {
			mu.Lock()
			firstEntered = map[string]string{}
			for _, v := range p.Entered {
				firstEntered[v.Provider] = v.EventID
			}
			mu.Unlock()
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		mu.Lock()
		if len(p.Entered) > 0 {
			redelivered = append(redelivered, p.Entered...)
		}
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	reg := NewRegistry()
	for _, p := range []string{"provA", "provB"} {
		for i := 0; i < 20; i++ {
			reg.RecordRequest(p, "m", "chat_completions", 500, time.Millisecond, "error")
			reg.RecordUpstreamAttempt(p, time.Millisecond, AttemptHeader)
			reg.RecordUpstreamError(p, 500)
		}
	}
	e := NewSLOEvaluator(reg, SLOConfig{UpstreamErrorRateMax: 0.01}, srv.URL, nil)
	defer e.Close()

	if len(e.CheckNow()) < 2 {
		t.Fatal("expected A+B enter")
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if e.WebhookRequestCount("non_2xx") >= 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	// A 恢复
	e.mu.Lock()
	var aKey string
	for k := range e.active {
		if strings.HasPrefix(k, "provA|") {
			aKey = k
			break
		}
	}
	if aKey == "" {
		e.mu.Unlock()
		t.Fatal("missing provA active")
	}
	aViol := e.active[aKey]
	delete(e.active, aKey)
	// gen 保留供 resolved 对账
	instanceID := e.instanceID
	e.mu.Unlock()
	aViol.EventID = violationEventID(instanceID, aViol.Provider, aViol.Rule, aViol.Generation, SLOStateResolved)

	// 入队 A resolved(新 seq),并 flush 旧批次
	time.Sleep(webhookRetryBase + 50*time.Millisecond)
	e.enqueueWebhook(e.newPayload(nil, []SLOViolation{aViol}))
	e.flushUndelivered()

	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(redelivered)
		mu.Unlock()
		if n > 0 && e.WebhookRequestCount("ok") >= 1 {
			break
		}
		e.flushUndelivered()
		time.Sleep(20 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if firstEntered["provB"] == "" {
		t.Fatal("missing first B event_id")
	}
	found := false
	for _, v := range redelivered {
		if v.Provider == "provB" {
			found = true
			if v.EventID != firstEntered["provB"] {
				t.Fatalf("B event_id changed after partial reconcile: %q -> %q", firstEntered["provB"], v.EventID)
			}
			if strings.Contains(v.EventID, "provA") {
				t.Fatalf("B event_id must not embed A: %q", v.EventID)
			}
		}
		if v.Provider == "provA" {
			t.Fatalf("stale A should not redeliver: %#v", v)
		}
	}
	if !found {
		t.Fatalf("B not redelivered: first=%v redelivered=%v", firstEntered, redelivered)
	}
}

func deliveredSnapshot(mu *sync.Mutex, delivered []SLOWebhookPayload) string {
	mu.Lock()
	defer mu.Unlock()
	var b strings.Builder
	for _, p := range delivered {
		fmt.Fprintf(&b, "{instance=%s seq=%d entered=%d resolved=%d} ", p.InstanceID, p.Seq, len(p.Entered), len(p.Resolved))
	}
	return b.String()
}

func TestIsRetryableWebhookStatus(t *testing.T) {
	for _, code := range []int{408, 425, 429, 500, 502, 503} {
		if !isRetryableWebhookStatus(code) {
			t.Fatalf("%d should be retryable", code)
		}
	}
	for _, code := range []int{400, 401, 403, 404, 302} {
		if isRetryableWebhookStatus(code) {
			t.Fatalf("%d should be permanent", code)
		}
	}
}

func TestSLOListenerResolvedState(t *testing.T) {
	reg := NewRegistry()
	for i := 0; i < 20; i++ {
		reg.RecordRequest("openai", "m", "chat_completions", 500, time.Millisecond, "error")
		reg.RecordUpstreamAttempt("openai", time.Millisecond, AttemptHeader)
		reg.RecordUpstreamError("openai", 500)
	}
	var events []SLOStateChange
	var mu sync.Mutex
	e := NewSLOEvaluator(reg, SLOConfig{UpstreamErrorRateMax: 0.01}, "", func(ev SLOStateChange) {
		mu.Lock()
		events = append(events, ev)
		mu.Unlock()
	})
	if len(e.CheckNow()) == 0 {
		t.Fatal("expected enter")
	}
	e.config.UpstreamErrorRateMax = 1.0
	if len(e.CheckNow()) != 0 {
		t.Fatal("should not re-enter")
	}
	mu.Lock()
	defer mu.Unlock()
	var entered, resolved int
	for _, ev := range events {
		switch ev.State {
		case SLOStateEntered:
			entered++
		case SLOStateResolved:
			resolved++
		default:
			t.Fatalf("unknown state %q", ev.State)
		}
	}
	if entered == 0 || resolved == 0 {
		t.Fatalf("entered=%d resolved=%d events=%#v", entered, resolved, events)
	}
}

func TestSLOWebhookMetricsExposed(t *testing.T) {
	reg := NewRegistry()
	client := &http.Client{
		Transport: blockingRoundTripper{},
		Timeout:   0,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	e := newSLOEvaluator(reg, SLOConfig{}, "http://example.invalid/hook", nil, client)
	defer e.Close()
	reg.AttachSLO(e)

	v := seedActive(e, testViolation("p", "r"))
	for i := 0; i < webhookMaxConcurrent+webhookQueueSize+4; i++ {
		e.enqueueWebhook(SLOWebhookPayload{
			At:      time.Now(),
			Seq:     uint64(i + 1),
			Entered: []SLOViolation{v},
		})
	}

	var buf strings.Builder
	if err := reg.WritePrometheus(&buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{
		"ai_proxy_slo_webhook_dropped_total",
		"ai_proxy_slo_webhook_queue_length",
		`ai_proxy_slo_webhook_requests_total{result="ok"}`,
		`ai_proxy_slo_webhook_requests_total{result="error"}`,
		`ai_proxy_slo_webhook_requests_total{result="non_2xx"}`,
		`ai_proxy_slo_webhook_requests_total{result="canceled"}`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("metrics missing %q\n%s", want, out)
		}
	}
	if e.WebhookDropped() == 0 {
		t.Fatal("expected drops for prometheus test setup")
	}
	if !strings.Contains(out, fmt.Sprintf("ai_proxy_slo_webhook_dropped_total %d", e.WebhookDropped())) {
		// 可能中间又 drop 了,至少应 >0
		if !strings.Contains(out, "ai_proxy_slo_webhook_dropped_total ") {
			t.Fatalf("dropped metric value missing:\n%s", out)
		}
	}
}

func TestSLOWebhookStateChangeOnly(t *testing.T) {
	var hits atomic.Int32
	var lastResolved atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		body, _ := io.ReadAll(r.Body)
		var p SLOWebhookPayload
		_ = json.Unmarshal(body, &p)
		lastResolved.Store(int32(len(p.Resolved)))
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	reg := NewRegistry()
	// 制造 violation
	for i := 0; i < 20; i++ {
		reg.RecordRequest("openai", "m", "chat_completions", 500, time.Millisecond, "error")
		reg.RecordUpstreamAttempt("openai", time.Millisecond, AttemptHeader)
		reg.RecordUpstreamError("openai", 500)
		reg.RecordTokens("openai", "m", 1, 1, 0, 0)
	}
	e := NewSLOEvaluator(reg, SLOConfig{UpstreamErrorRateMax: 0.01}, srv.URL, nil)
	defer e.Close()

	if len(e.CheckNow()) == 0 {
		t.Fatal("expected enter")
	}
	// 持续:无新通知
	time.Sleep(100 * time.Millisecond)
	if n := hits.Load(); n != 1 {
		// 等第一次 webhook
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) && hits.Load() < 1 {
			time.Sleep(20 * time.Millisecond)
		}
	}
	if hits.Load() != 1 {
		t.Fatalf("hits after enter = %d, want 1", hits.Load())
	}
	if len(e.CheckNow()) != 0 {
		t.Fatal("sustained should not re-enter")
	}
	time.Sleep(50 * time.Millisecond)
	if hits.Load() != 1 {
		t.Fatalf("sustained should not re-webhook, hits=%d", hits.Load())
	}

	// 恢复:新 registry 无错误 → 需要清空 active 对应指标
	// 换一个干净 registry 无法改 e.registry;通过超大阈值让 evaluate 返回空
	e.config.UpstreamErrorRateMax = 1.0 // 100% 才告警,当前 <1 则恢复
	if len(e.CheckNow()) != 0 {
		t.Fatal("should not enter with high threshold")
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if hits.Load() >= 2 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if hits.Load() < 2 {
		t.Fatalf("expected recovery webhook, hits=%d lastResolved=%d", hits.Load(), lastResolved.Load())
	}
}
