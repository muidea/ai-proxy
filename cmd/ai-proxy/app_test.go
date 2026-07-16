package main

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ai-proxy/internal/config"
)

func testConfig(dir string) config.Config {
	return config.Config{
		ListenAddr:               "127.0.0.1:0",
		UsageFile:                filepath.Join(dir, "usage.csv"),
		InteractionDir:           filepath.Join(dir, "interactions"),
		InteractionRetention:     10,
		ArchiveFullContent:       true,
		MetricsRemoteAccess:      false,
		MaxRequestBodyBytes:      config.DefaultMaxRequestBodyBytes,
		MaxUpstreamResponseBytes: config.DefaultMaxUpstreamResponseBytes,
		MaxStreamBytes:           config.DefaultMaxStreamBytes,
		MaxSSELineBytes:          config.DefaultMaxSSELineBytes,
		Providers: map[string]config.Provider{
			"openai": {
				Name:     "openai",
				Protocol: "openai",
				BaseURL:  "https://api.openai.com",
				APIKey:   "test",
				Models:   []string{"gpt-4o", "gpt-*"},
				EndpointCapabilities: []string{
					config.EndpointCapabilityChatCompletions,
					config.EndpointCapabilityResponses,
					config.EndpointCapabilityCompletions,
					config.EndpointCapabilityEmbeddings,
				},
			},
		},
		ModelCatalog: map[string]config.ModelInfo{
			"gpt-4o": {
				ID:                  "gpt-4o",
				ContextWindowTokens: 128000,
				MaxOutputTokens:     16384,
				Operations:          []string{config.ModelOperationChatCompletions},
				RouteOwner:          "openai",
			},
		},
	}
}

func TestBuildAppWiresObservabilityAndProxyRoutes(t *testing.T) {
	cfg := testConfig(t.TempDir())
	application, err := buildApp(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer application.Close()

	if application.server == nil || application.server.Handler == nil {
		t.Fatal("server not wired")
	}
	if application.registry == nil || application.evaluator == nil {
		t.Fatal("registry/evaluator missing")
	}

	h := application.server.Handler
	for _, path := range []string{"/healthz", "/metrics", "/stats", "/admin/"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.RemoteAddr = "127.0.0.1:12345"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code == http.StatusNotFound {
			t.Fatalf("%s returned 404", path)
		}
	}
	// metrics loopback-only
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	req.RemoteAddr = "8.8.8.8:9999"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("remote metrics status = %d, want 403", rec.Code)
	}
	// proxy catch-all
	req = httptest.NewRequest(http.MethodGet, "/v1/unknown", nil)
	req.RemoteAddr = "127.0.0.1:1"
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown path status = %d", rec.Code)
	}
	req = httptest.NewRequest(http.MethodGet, "/healthz", nil)
	req.RemoteAddr = "127.0.0.1:1"
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "ok") {
		t.Fatalf("healthz = %d %s", rec.Code, rec.Body.String())
	}
}

func TestBuildAppWithRemoteMetricsCIDR(t *testing.T) {
	cfg := testConfig(t.TempDir())
	cfg.MetricsRemoteAccess = true
	cfg.MetricsAllowedCIDRs = []string{"10.0.0.0/8"}
	application, err := buildApp(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer application.Close()
	h := application.server.Handler

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	req.RemoteAddr = "10.1.2.3:9"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("allowed cidr status = %d", rec.Code)
	}
	req = httptest.NewRequest(http.MethodGet, "/metrics", nil)
	req.RemoteAddr = "192.168.0.1:9"
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("denied cidr status = %d", rec.Code)
	}
}

func TestBuildAppReservesExactModels(t *testing.T) {
	cfg := testConfig(t.TempDir())
	application, err := buildApp(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer application.Close()
	application.registry.RecordRequest("openai", "gpt-4o", "chat_completions", 200, 0, "success")
	var b strings.Builder
	if err := application.registry.WritePrometheus(&b); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), "gpt-4o") {
		t.Fatalf("expected gpt-4o in metrics, got %s", b.String())
	}
}

func TestBuildAppWiresSLOEvaluator(t *testing.T) {
	cfg := testConfig(t.TempDir())
	cfg.SLO.CheckIntervalSeconds = 0
	cfg.SLO.UpstreamErrorRateMax = 0.05
	application, err := buildApp(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer application.Close()
	if application.evaluator.Webhook() != "" {
		t.Fatal("unexpected webhook")
	}
	// evaluator 可用
	_ = application.evaluator.CheckNow()
	// AttachSLO 后 /metrics 含 webhook 指标
	if application.registry.SLO() != application.evaluator {
		t.Fatal("evaluator not attached to registry")
	}
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	req.RemoteAddr = "127.0.0.1:1"
	rec := httptest.NewRecorder()
	application.server.Handler.ServeHTTP(rec, req)
	body := rec.Body.String()
	for _, want := range []string{
		"ai_proxy_slo_webhook_dropped_total",
		"ai_proxy_slo_webhook_queue_length",
		`ai_proxy_slo_webhook_requests_total{result="ok"}`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("metrics missing %q\n%s", want, body)
		}
	}
}

func TestBuildAppCloseWaitsForSLORun(t *testing.T) {
	cfg := testConfig(t.TempDir())
	cfg.SLO.CheckIntervalSeconds = 60
	application, err := buildApp(cfg)
	if err != nil {
		t.Fatal(err)
	}
	// Close 应取消 Run 并返回,不阻塞到 check interval
	done := make(chan struct{})
	go func() {
		application.Close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Close blocked waiting for SLO Run")
	}
	// 幂等
	application.Close()
}
