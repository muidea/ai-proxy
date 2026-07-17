package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"ai-proxy/internal/config"
	"ai-proxy/internal/usage"
)

type fakeRuntime struct {
	cfg config.Config
}

func (f *fakeRuntime) ConfigSnapshot() config.Config { return f.cfg }
func (f *fakeRuntime) UpdateConfig(cfg config.Config) error {
	f.cfg = cfg
	return nil
}

func TestUsageDashboardAndEventsLoopback(t *testing.T) {
	store := usage.NewMemoryStore()
	now := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	_ = store.Start(context.Background(), usage.StartRecord{
		EventID: "e1", StartedAt: now, APIKeyID: "codex",
		Operation: "chat_completions", ClientEndpoint: "/v1/chat/completions", ClientProtocol: "openai",
	})
	_ = store.Complete(context.Background(), usage.CompleteRecord{
		EventID: "e1", CompletedAt: now.Add(time.Second), Provider: "openai", Model: "gpt-4o",
		InputTokens: 10, OutputTokens: 5, HTTPStatus: 200, Outcome: "success",
	})

	rt := &fakeRuntime{cfg: config.Config{
		ClientAPIKeys: map[string]config.ClientAPIKey{
			"codex": {ID: "codex", APIKey: "x", Enabled: true},
		},
	}}
	h := NewHandlerWithUsage("", rt, store)

	req := httptest.NewRequest(http.MethodGet, "/admin/api/usage/dashboard?range=30d", nil)
	req.RemoteAddr = "127.0.0.1:1234"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	summary := body["summary"].(map[string]any)
	if int(summary["requests"].(float64)) != 1 {
		t.Fatalf("requests=%v", summary["requests"])
	}

	// remote forbidden
	req = httptest.NewRequest(http.MethodGet, "/admin/api/usage/dashboard", nil)
	req.RemoteAddr = "8.8.8.8:99"
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("remote status=%d", rec.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/admin/api/usage/events?page_size=10&range=30d", nil)
	req.RemoteAddr = "127.0.0.1:1"
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("events status=%d body=%s", rec.Code, rec.Body.String())
	}
}
