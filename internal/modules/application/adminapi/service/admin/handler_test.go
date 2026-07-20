package admin

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"ai-proxy/internal/pkg/aiproxyconfig"
)

type testRuntime struct {
	mu      sync.Mutex
	cfg     config.Config
	updates int
}

func (r *testRuntime) ConfigSnapshot() config.Config {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cfg
}

func (r *testRuntime) UpdateConfig(cfg config.Config) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cfg = cfg
	r.updates++
	return nil
}

func writeAdminTestConfig(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := `server:
  listen_addr: 127.0.0.1:8080
providers:
  openai:
    enabled: true
    protocol: openai
    base_url: https://api.openai.com/v1
    api_key: ${ADMIN_TEST_API_KEY}
    endpoint_capabilities: chat_completions
    models: gpt-*
model_catalog:
  gpt-4o:
    context_window_tokens: 128000
    max_output_tokens: 16384
    operations: chat_completions
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestHandlerServesProjectAdminPageAndMasksAPIKey(t *testing.T) {
	t.Setenv("ADMIN_TEST_API_KEY", "secret-value")
	path := writeAdminTestConfig(t)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	handler := NewHandler(path, &testRuntime{cfg: cfg})

	req := httptest.NewRequest(http.MethodGet, "/admin/", nil)
	req.RemoteAddr = "127.0.0.1:1234"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Provider 管理") {
		t.Fatalf("admin page = %d %s", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/admin/api/providers", nil)
	req.RemoteAddr = "127.0.0.1:1234"
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("providers = %d %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "secret-value") || strings.Contains(rec.Body.String(), "ADMIN_TEST_API_KEY") {
		t.Fatalf("provider response leaked API key: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"api_key_configured":true`) {
		t.Fatalf("provider response missing configured marker: %s", rec.Body.String())
	}
}

func TestHandlerRejectsRemoteAdminAccess(t *testing.T) {
	handler := NewHandler("config.yaml", &testRuntime{})
	req := httptest.NewRequest(http.MethodGet, "/admin/", nil)
	req.RemoteAddr = "203.0.113.8:1234"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
}

func TestHandlerUpdatesProvidersPreservesRawSecretAndHotReloads(t *testing.T) {
	t.Setenv("ADMIN_TEST_API_KEY", "secret-value")
	path := writeAdminTestConfig(t)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	runtime := &testRuntime{cfg: cfg}
	handler := NewHandler(path, runtime)
	body, err := json.Marshal(updateRequest{Providers: []providerInput{{
		Name:                 "openai",
		Protocol:             "openai",
		BaseURL:              "https://gateway.example.com/v1",
		Models:               []string{"gpt-*"},
		EndpointCapabilities: []string{config.EndpointCapabilityChatCompletions},
		Enabled:              true,
	}}})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPut, "/admin/api/providers", bytes.NewReader(body))
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-AI-Proxy-Admin", "1")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("update = %d %s", rec.Code, rec.Body.String())
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "${ADMIN_TEST_API_KEY}") {
		t.Fatalf("raw API key expression was not preserved:\n%s", raw)
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if runtime.updates != 1 {
		t.Fatalf("updates = %d, want 1", runtime.updates)
	}
	provider := runtime.cfg.Providers["openai"]
	if provider.BaseURL != "https://gateway.example.com/v1" || provider.APIKey != "secret-value" {
		t.Fatalf("runtime provider = %+v", provider)
	}
}

func TestHandlerManagesHashedClientAPIKeys(t *testing.T) {
	t.Setenv("ADMIN_TEST_API_KEY", "secret-value")
	path := writeAdminTestConfig(t)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	runtime := &testRuntime{cfg: cfg}
	handler := NewHandler(path, runtime)

	create := httptest.NewRequest(http.MethodPost, "/admin/api/client-api-keys", strings.NewReader(`{"id":"ci-agent"}`))
	create.RemoteAddr = "127.0.0.1:1234"
	create.Header.Set("X-AI-Proxy-Admin", "1")
	create.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, create)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", rec.Code, rec.Body.String())
	}
	var created struct {
		APIKey string `json:"api_key"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil || !strings.HasPrefix(created.APIKey, "aip_") {
		t.Fatalf("created = %#v err=%v", created, err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), created.APIKey) || !strings.Contains(string(raw), "api_key_hash") {
		t.Fatalf("key storage leaked secret: %s", raw)
	}

	list := httptest.NewRequest(http.MethodGet, "/admin/api/client-api-keys", nil)
	list.RemoteAddr = "127.0.0.1:1234"
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, list)
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), created.APIKey) || !strings.Contains(rec.Body.String(), `"credential_source":"managed"`) {
		t.Fatalf("list = %d %s", rec.Code, rec.Body.String())
	}

	disable := httptest.NewRequest(http.MethodPatch, "/admin/api/client-api-keys/ci-agent", strings.NewReader(`{"enabled":false}`))
	disable.RemoteAddr = "127.0.0.1:1234"
	disable.Header.Set("X-AI-Proxy-Admin", "1")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, disable)
	if rec.Code != http.StatusOK {
		t.Fatalf("disable = %d %s", rec.Code, rec.Body.String())
	}
	if runtime.cfg.ClientAPIKeys["ci-agent"].Enabled {
		t.Fatal("key remained enabled")
	}
}

func TestHandlerRejectsInvalidProviderChangeWithoutReplacingConfig(t *testing.T) {
	t.Setenv("ADMIN_TEST_API_KEY", "secret-value")
	path := writeAdminTestConfig(t)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	handler := NewHandler(path, &testRuntime{cfg: cfg})
	body := []byte(`{"providers":[{"name":"openai","protocol":"openai","base_url":"https://api.openai.com/v1","models":["other-*"],"endpoint_capabilities":["chat_completions"],"enabled":true}]}`)
	req := httptest.NewRequest(http.MethodPut, "/admin/api/providers", bytes.NewReader(body))
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("X-AI-Proxy-Admin", "1")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("invalid update replaced config file")
	}
}
