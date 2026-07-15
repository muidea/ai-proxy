package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoadConfigFileAndEnv(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "env-openai-key")
	t.Setenv("AI_PROXY_LISTEN_ADDR", "127.0.0.1:18080")
	t.Setenv("AI_PROXY_INTERACTION_RETENTION", "")
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
server:
  port: 9090
  usage_file: test-usage.csv
  interaction_dir: test-interactions
  debug_log: false
  stream_idle_timeout_seconds: 900
providers:
  deepseek:
    base_url: https://api.deepseek.com
    api_key: ${OPENAI_API_KEY}
    models: deepseek*
    fallbacks: openai, custom
  openai:
    base_url: https://api.openai.com
    api_key: test
  custom:
    base_url: https://custom.example
    api_key: test
default_provider: deepseek
`), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ListenAddr != "127.0.0.1:18080" {
		t.Fatalf("listen addr = %s", cfg.ListenAddr)
	}
	if cfg.Providers["deepseek"].APIKey != "env-openai-key" {
		t.Fatalf("api key was not expanded")
	}
	if len(cfg.Providers["deepseek"].Models) != 1 || cfg.Providers["deepseek"].Models[0] != "deepseek*" {
		t.Fatalf("models = %#v", cfg.Providers["deepseek"].Models)
	}
	if got := cfg.Providers["deepseek"].Fallbacks; len(got) != 2 || got[0] != "openai" || got[1] != "custom" {
		t.Fatalf("fallbacks = %#v", got)
	}
	if cfg.InteractionDir != "test-interactions" {
		t.Fatalf("interaction dir = %s", cfg.InteractionDir)
	}
	if cfg.InteractionRetention != 500 {
		t.Fatalf("interaction retention = %d", cfg.InteractionRetention)
	}
	if cfg.DebugLog {
		t.Fatalf("debug log should be disabled by config")
	}
	if cfg.StreamIdleTimeout != 900*time.Second {
		t.Fatalf("stream idle timeout = %s", cfg.StreamIdleTimeout)
	}
	if cfg.DefaultProvider != "deepseek" {
		t.Fatalf("default provider = %s", cfg.DefaultProvider)
	}
}

func TestLoadDisabledProvider(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
providers:
  openai:
    base_url: https://api.openai.com
    api_key: test
  deepseek:
    base_url: https://api.deepseek.com
    api_key: test
    enabled: false
`), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Providers["deepseek"].Disabled {
		t.Fatalf("deepseek should be disabled")
	}
	if cfg.Providers["openai"].Disabled {
		t.Fatalf("openai should be enabled")
	}
}

func TestLoadModelCatalog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
providers:
  openai:
    base_url: https://api.openai.com
    api_key: test
model_catalog:
  gpt-4o:
    context_window_tokens: 128000
    max_output_tokens: 16384
  DeepSeek-V4-Flash:
    context_window_tokens: 128000
    max_output_tokens: 8192
`), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	gpt, ok := cfg.ModelCatalog["gpt-4o"]
	if !ok {
		t.Fatalf("missing gpt-4o catalog entry: %#v", cfg.ModelCatalog)
	}
	if gpt.ID != "gpt-4o" || gpt.ContextWindowTokens != 128000 || gpt.MaxOutputTokens != 16384 {
		t.Fatalf("gpt-4o = %#v", gpt)
	}
	// model id 严格区分大小写:查找键与展示 ID 均保留配置原文
	ds, ok := cfg.ModelCatalog["DeepSeek-V4-Flash"]
	if !ok {
		t.Fatalf("missing DeepSeek-V4-Flash catalog entry: %#v", cfg.ModelCatalog)
	}
	if ds.ID != "DeepSeek-V4-Flash" || ds.ContextWindowTokens != 128000 || ds.MaxOutputTokens != 8192 {
		t.Fatalf("DeepSeek-V4-Flash = %#v", ds)
	}
	if _, ok := cfg.ModelCatalog["deepseek-v4-flash"]; ok {
		t.Fatalf("unexpected lowercased catalog key: %#v", cfg.ModelCatalog)
	}
}

func TestLoadInteractionRetentionFromConfigAndEnv(t *testing.T) {
	t.Setenv("AI_PROXY_INTERACTION_RETENTION", "321")
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
server:
  interaction_retention: 123
providers:
  openai:
    base_url: https://api.openai.com
    api_key: test
`), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.InteractionRetention != 321 {
		t.Fatalf("interaction retention = %d", cfg.InteractionRetention)
	}
}

func TestLoadStreamIdleTimeoutCanBeDisabledFromEnv(t *testing.T) {
	t.Setenv("AI_PROXY_STREAM_IDLE_TIMEOUT_SECONDS", "0")
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
server:
  stream_idle_timeout_seconds: 120
providers:
  openai:
    base_url: https://api.openai.com
    api_key: test
`), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.StreamIdleTimeout != 0 {
		t.Fatalf("stream idle timeout = %s", cfg.StreamIdleTimeout)
	}
}

func TestLoadDefaultProviderFromEnv(t *testing.T) {
	t.Setenv("AI_PROXY_DEFAULT_PROVIDER", "DEEPSEEK")
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
default_provider: openai
providers:
  openai:
    base_url: https://api.openai.com
    api_key: test
  deepseek:
    base_url: https://api.deepseek.com
    api_key: test
`), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DefaultProvider != "deepseek" {
		t.Fatalf("default provider = %s", cfg.DefaultProvider)
	}
}

func TestLoadRejectsInvalidDefaultProvider(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
default_provider: missing
providers:
  openai:
    base_url: https://api.openai.com
    api_key: test
`), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := Load(path)
	if err == nil {
		t.Fatal("expected invalid default provider error")
	}
	if got := err.Error(); got != `default_provider "missing" is not configured` {
		t.Fatalf("error = %q", got)
	}
}

func TestLoadRejectsDisabledDefaultProvider(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
default_provider: deepseek
providers:
  openai:
    base_url: https://api.openai.com
    api_key: test
  deepseek:
    base_url: https://api.deepseek.com
    api_key: test
    enabled: false
`), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := Load(path)
	if err == nil {
		t.Fatal("expected disabled default provider error")
	}
	if got := err.Error(); got != `default_provider "deepseek" is disabled` {
		t.Fatalf("error = %q", got)
	}
}

func TestLoadParsesMetricsFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	body := `server:
  listen_addr: 127.0.0.1:9090
  metrics_remote_access: true
  metrics_allowed_cidrs: 10.0.0.0/8, 192.168.0.0/16
providers:
  openai:
    base_url: https://api.openai.com
    api_key: test
`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ListenAddr != "127.0.0.1:9090" {
		t.Fatalf("listen = %q", cfg.ListenAddr)
	}
	if !cfg.MetricsRemoteAccess {
		t.Fatalf("MetricsRemoteAccess = false, want true")
	}
	if len(cfg.MetricsAllowedCIDRs) != 2 {
		t.Fatalf("cidrs = %v, want 2 entries", cfg.MetricsAllowedCIDRs)
	}
}

func TestLoadRejectsNonLoopbackWithoutInboundKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
server:
  listen_addr: 0.0.0.0:8080
providers:
  openai:
    base_url: https://api.openai.com
    api_key: test
`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected non-loopback without inbound_api_key to fail")
	}
	if !strings.Contains(err.Error(), "inbound_api_key") {
		t.Fatalf("error = %q", err)
	}
}

func TestLoadAllowsNonLoopbackWithInboundKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
server:
  listen_addr: 0.0.0.0:8080
  inbound_api_key: secret-key
providers:
  openai:
    base_url: https://api.openai.com
    api_key: test
`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.InboundAPIKey != "secret-key" {
		t.Fatalf("InboundAPIKey = %q", cfg.InboundAPIKey)
	}
}

func TestLoadRejectsUnknownKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
server:
  typo_key: true
providers:
  openai:
    base_url: https://api.openai.com
    api_key: test
`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected unknown key error")
	}
	if !strings.Contains(err.Error(), "unknown config key") {
		t.Fatalf("error = %q", err)
	}
}

func TestLoadRejectsInvalidBool(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
server:
  debug_log: maybe
providers:
  openai:
    base_url: https://api.openai.com
    api_key: test
`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected invalid bool error")
	}
	if !strings.Contains(err.Error(), "invalid boolean") {
		t.Fatalf("error = %q", err)
	}
}

func TestLoadRejectsUnknownProtocol(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
providers:
  custom:
    base_url: https://example.com
    api_key: test
    protocol: graphql
`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected unknown protocol error")
	}
	if !strings.Contains(err.Error(), "unknown protocol") {
		t.Fatalf("error = %q", err)
	}
}

func TestLoadRejectsMissingFallback(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
providers:
  openai:
    base_url: https://api.openai.com
    api_key: test
    fallbacks: missing-provider
`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected missing fallback error")
	}
	if !strings.Contains(err.Error(), "fallback") {
		t.Fatalf("error = %q", err)
	}
}

func TestIsLoopbackListenAddr(t *testing.T) {
	cases := map[string]bool{
		"127.0.0.1:8080": true,
		"[::1]:8080":     true,
		"localhost:8080": true,
		":8080":          false,
		"0.0.0.0:8080":   false,
		"192.168.1.1:80": false,
		"":               false,
	}
	for addr, want := range cases {
		if got := IsLoopbackListenAddr(addr); got != want {
			t.Fatalf("IsLoopbackListenAddr(%q)=%v want %v", addr, got, want)
		}
	}
}

func TestLoadMetricsRemoteAccessFromEnv(t *testing.T) {
	t.Setenv("AI_PROXY_METRICS_REMOTE_ACCESS", "true")
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
providers:
  openai:
    base_url: https://api.openai.com
    api_key: test
`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.MetricsRemoteAccess {
		t.Fatalf("MetricsRemoteAccess = false, want true from env")
	}
}

func TestLoadLogFormatFromConfigAndEnv(t *testing.T) {
	t.Setenv("AI_PROXY_LOG_FORMAT", "json")
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
server:
  log_format: text
providers:
  openai:
    base_url: https://api.openai.com
    api_key: test
`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.LogFormat != "json" {
		t.Fatalf("log format = %q, want env override json", cfg.LogFormat)
	}
}

func TestLoadRejectsInvalidEnv(t *testing.T) {
	t.Setenv("AI_PROXY_DEBUG_LOG", "maybe")
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
providers:
  openai:
    base_url: https://api.openai.com
    api_key: test
`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected invalid env bool to fail")
	}
	if !strings.Contains(err.Error(), "AI_PROXY_DEBUG_LOG") {
		t.Fatalf("error = %q", err)
	}
}

func TestLoadRejectsInvalidMaxBodyEnv(t *testing.T) {
	t.Setenv("AI_PROXY_MAX_REQUEST_BODY_BYTES", "invalid")
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
providers:
  openai:
    base_url: https://api.openai.com
    api_key: test
`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected invalid env int to fail")
	}
	if !strings.Contains(err.Error(), "AI_PROXY_MAX_REQUEST_BODY_BYTES") {
		t.Fatalf("error = %q", err)
	}
}

func TestAddrFromPortUsesLoopback(t *testing.T) {
	if got := addrFromPort("8080"); got != "127.0.0.1:8080" {
		t.Fatalf("addrFromPort(8080) = %q", got)
	}
	if got := addrFromPort(":9090"); got != "127.0.0.1:9090" {
		t.Fatalf("addrFromPort(:9090) = %q", got)
	}
	if got := addrFromPort("0.0.0.0:8080"); got != "0.0.0.0:8080" {
		t.Fatalf("addrFromPort full = %q", got)
	}
}

func TestLoadRejectsInvalidBaseURL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
providers:
  bad:
    base_url: not-a-url
    api_key: test
`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected invalid base_url error")
	}
	if !strings.Contains(err.Error(), "base_url") {
		t.Fatalf("error = %q", err)
	}
}

func TestLoadRejectsNonHTTPBaseURL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
providers:
  bad:
    base_url: ftp://example.com
    api_key: test
`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected scheme error")
	}
}

func TestLoadRejectsCaseFoldProviderDuplicate(t *testing.T) {
	// 通过直接构造 Config 测 normalize:大小写折叠后重复 provider 应启动失败。
	cfg := Config{
		ListenAddr: "127.0.0.1:8080",
		Providers: map[string]Provider{
			"OpenAI": {Name: "OpenAI", Protocol: "openai", BaseURL: "https://api.openai.com", APIKey: "a"},
			"openai": {Name: "openai", Protocol: "openai", BaseURL: "https://api.openai.com", APIKey: "b"},
		},
	}
	if err := normalize(&cfg); err == nil {
		t.Fatal("expected case-fold duplicate provider error")
	}
}

func TestLoadAllowsCaseDifferingModelCatalogIDs(t *testing.T) {
	// model 严格区分大小写:仅大小写不同的 id 可并存。
	cfg := Config{
		ListenAddr: "127.0.0.1:8080",
		Providers: map[string]Provider{
			"openai": {Name: "openai", Protocol: "openai", BaseURL: "https://api.openai.com", APIKey: "a"},
		},
		ModelCatalog: map[string]ModelInfo{
			"GPT-4o": {ID: "GPT-4o"},
			"gpt-4o": {ID: "gpt-4o"},
		},
	}
	if err := normalize(&cfg); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if _, ok := cfg.ModelCatalog["GPT-4o"]; !ok {
		t.Fatalf("missing GPT-4o: %#v", cfg.ModelCatalog)
	}
	if _, ok := cfg.ModelCatalog["gpt-4o"]; !ok {
		t.Fatalf("missing gpt-4o: %#v", cfg.ModelCatalog)
	}
}

func TestLoadRejectsExactModelCatalogDuplicate(t *testing.T) {
	cfg := Config{
		ListenAddr: "127.0.0.1:8080",
		Providers: map[string]Provider{
			"openai": {Name: "openai", Protocol: "openai", BaseURL: "https://api.openai.com", APIKey: "a"},
		},
		ModelCatalog: map[string]ModelInfo{
			"gpt-4o": {ID: "gpt-4o"},
		},
	}
	// 模拟 map 键与 info.ID 不同但归一化后撞上同一 id 的情况:
	// 再塞一个 name 不同、ID 相同的条目(通过二次写入 ensure 路径不方便,直接调 normalize 前构造)。
	cfg.ModelCatalog["alias"] = ModelInfo{ID: "gpt-4o"}
	if err := normalize(&cfg); err == nil {
		t.Fatal("expected exact duplicate model error")
	}
}

func TestLoadRejectsInvalidSLOWebhook(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
server:
  listen_addr: 127.0.0.1:8080
  slo_violation_webhook: not-a-url
providers:
  openai:
    base_url: https://api.openai.com
    api_key: test
`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected invalid webhook error")
	}
	if !strings.Contains(err.Error(), "slo_violation_webhook") {
		t.Fatalf("error = %q", err)
	}
}

func TestLoadRejectsNonHTTPWebhook(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
server:
  listen_addr: 127.0.0.1:8080
  slo_violation_webhook: ftp://hooks.example/secret
providers:
  openai:
    base_url: https://api.openai.com
    api_key: test
`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected scheme error")
	}
}
