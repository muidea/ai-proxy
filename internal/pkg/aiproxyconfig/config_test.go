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
  interaction_dir: test-interactions
  debug_log: false
  stream_idle_timeout_seconds: 900
usage_store:
  path: test-usage.duckdb
  memory_limit: 256MB
  threads: 2
  query_cache_seconds: 15
providers:
  deepseek:
    protocol: openai
    base_url: https://api.deepseek.com
    api_key: ${OPENAI_API_KEY}
    endpoint_capabilities: chat_completions, responses, completions, embeddings
    models: deepseek*
  openai:
    protocol: openai
    base_url: https://api.openai.com
    api_key: test
    endpoint_capabilities: chat_completions, responses, completions, embeddings
    models: gpt-*
  custom:
    protocol: openai
    base_url: https://custom.example
    api_key: test
    endpoint_capabilities: chat_completions, responses, completions, embeddings
    models: custom-*
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
	// fallbacks 已移除:配置中不得声明 fallbacks。
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
}

func TestLoadDisabledProvider(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
providers:
  openai:
    protocol: openai
    base_url: https://api.openai.com
    api_key: test
    endpoint_capabilities: chat_completions, responses, completions, embeddings
    models: gpt-*
  deepseek:
    base_url: https://api.deepseek.com
    api_key: test
    endpoint_capabilities: chat_completions, responses, completions, embeddings
    enabled: false
    protocol: openai
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
    protocol: openai
    base_url: https://api.openai.com
    api_key: test
    endpoint_capabilities: chat_completions, responses, completions, embeddings
    models: gpt-*,DeepSeek*
model_catalog:
  gpt-4o:
    context_window_tokens: 128000
    max_output_tokens: 16384
    operations: chat_completions
  DeepSeek-V4-Flash:
    context_window_tokens: 128000
    max_output_tokens: 8192
    operations: chat_completions, embeddings
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
	if len(gpt.Operations) != 1 || gpt.Operations[0] != ModelOperationChatCompletions {
		t.Fatalf("gpt-4o operations = %#v", gpt.Operations)
	}
	// model id 严格区分大小写:查找键与展示 ID 均保留配置原文
	ds, ok := cfg.ModelCatalog["DeepSeek-V4-Flash"]
	if !ok {
		t.Fatalf("missing DeepSeek-V4-Flash catalog entry: %#v", cfg.ModelCatalog)
	}
	if ds.ID != "DeepSeek-V4-Flash" || ds.ContextWindowTokens != 128000 || ds.MaxOutputTokens != 8192 {
		t.Fatalf("DeepSeek-V4-Flash = %#v", ds)
	}
	if len(ds.Operations) != 2 || ds.Operations[0] != ModelOperationChatCompletions || ds.Operations[1] != ModelOperationEmbeddings {
		t.Fatalf("DeepSeek-V4-Flash operations = %#v", ds.Operations)
	}
	if gpt.RouteOwner != "openai" {
		t.Fatalf("gpt-4o route owner = %q", gpt.RouteOwner)
	}
	if ds.RouteOwner != "openai" {
		t.Fatalf("DeepSeek-V4-Flash route owner = %q", ds.RouteOwner)
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
    protocol: openai
    base_url: https://api.openai.com
    api_key: test
    endpoint_capabilities: chat_completions, responses, completions, embeddings
    models: gpt-*
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
    protocol: openai
    base_url: https://api.openai.com
    api_key: test
    endpoint_capabilities: chat_completions, responses, completions, embeddings
    models: gpt-*
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

func TestLoadRejectsDefaultProviderConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
providers:
  openai:
    protocol: openai
    base_url: https://api.openai.com
    api_key: test
    endpoint_capabilities: chat_completions
    models: gpt-*
default_provider: openai
model_catalog:
  gpt-4o:
    context_window_tokens: 128000
    max_output_tokens: 16384
    operations: chat_completions
`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "default_provider is not supported") {
		t.Fatalf("error = %v, want default_provider not supported", err)
	}
}

func TestLoadIgnoresAIProxyDefaultProviderEnv(t *testing.T) {
	// env 不得再注入/覆盖 default_provider 路由语义。
	t.Setenv("AI_PROXY_DEFAULT_PROVIDER", "openai")
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
providers:
  openai:
    protocol: openai
    base_url: https://api.openai.com
    api_key: test
    endpoint_capabilities: chat_completions
    models: gpt-*
model_catalog:
  gpt-4o:
    context_window_tokens: 128000
    max_output_tokens: 16384
    operations: chat_completions
`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	_ = cfg
}

func TestLoadRejectsDefaultProviderEvenIfValidProvider(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
server:
  default_provider: openai
providers:
  openai:
    protocol: openai
    base_url: https://api.openai.com
    api_key: test
    endpoint_capabilities: chat_completions
    models: gpt-*
model_catalog:
  gpt-4o:
    context_window_tokens: 128000
    max_output_tokens: 16384
    operations: chat_completions
`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "default_provider is not supported") {
		t.Fatalf("error = %v", err)
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
    protocol: openai
    base_url: https://api.openai.com
    api_key: test
    endpoint_capabilities: chat_completions, responses, completions, embeddings
    models: gpt-*
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

func TestLoadAllowsNonLoopbackWithoutClientKeys(t *testing.T) {
	// client_api_keys 是归属机制而非强制登录;非 loopback 不再要求 inbound key。
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
server:
  listen_addr: 0.0.0.0:8080
providers:
  openai:
    protocol: openai
    base_url: https://api.openai.com
    api_key: test
    endpoint_capabilities: chat_completions, responses, completions, embeddings
    models: gpt-*
`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ListenAddr != "0.0.0.0:8080" {
		t.Fatalf("listen = %q", cfg.ListenAddr)
	}
}

func TestLoadRejectsInboundAPIKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
server:
  listen_addr: 127.0.0.1:8080
  inbound_api_key: secret-key
providers:
  openai:
    protocol: openai
    base_url: https://api.openai.com
    api_key: test
    endpoint_capabilities: chat_completions, responses, completions, embeddings
    models: gpt-*
`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected inbound_api_key to be rejected")
	}
	if !strings.Contains(err.Error(), "inbound_api_key") {
		t.Fatalf("error = %q", err)
	}
}

func TestLoadRejectsUsageFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
server:
  usage_file: usage.csv
providers:
  openai:
    protocol: openai
    base_url: https://api.openai.com
    api_key: test
    endpoint_capabilities: chat_completions, responses, completions, embeddings
    models: gpt-*
`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected usage_file to be rejected")
	}
	if !strings.Contains(err.Error(), "usage_file") {
		t.Fatalf("error = %q", err)
	}
}

func TestLoadClientAPIKeys(t *testing.T) {
	t.Setenv("CODEX_API_KEY", "sk-codex-from-env")
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
client_api_keys:
  Codex:
    api_key: ${CODEX_API_KEY}
    enabled: true
  workorch:
    api_key: sk-workorch
    enabled: false
providers:
  openai:
    protocol: openai
    base_url: https://api.openai.com
    api_key: test
    endpoint_capabilities: chat_completions, responses, completions, embeddings
    models: gpt-*
`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.ClientAPIKeys) != 2 {
		t.Fatalf("keys = %#v", cfg.ClientAPIKeys)
	}
	codex, ok := cfg.ClientAPIKeys["codex"]
	if !ok || codex.APIKey != "sk-codex-from-env" || !codex.Enabled {
		t.Fatalf("codex = %#v ok=%v", codex, ok)
	}
	wo, ok := cfg.ClientAPIKeys["workorch"]
	if !ok || wo.Enabled {
		t.Fatalf("workorch = %#v", wo)
	}
}

func TestLoadRejectsDefaultClientKeyID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
client_api_keys:
  default:
    api_key: sk-x
providers:
  openai:
    protocol: openai
    base_url: https://api.openai.com
    api_key: test
    endpoint_capabilities: chat_completions, responses, completions, embeddings
    models: gpt-*
`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "reserved") {
		t.Fatalf("err = %v", err)
	}
}

func TestLoadRejectsDuplicateClientSecrets(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
client_api_keys:
  a:
    api_key: same-secret
  b:
    api_key: same-secret
providers:
  openai:
    protocol: openai
    base_url: https://api.openai.com
    api_key: test
    endpoint_capabilities: chat_completions, responses, completions, embeddings
    models: gpt-*
`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected duplicate secret error")
	}
	if strings.Contains(err.Error(), "same-secret") {
		t.Fatalf("error leaked secret: %v", err)
	}
	if !strings.Contains(err.Error(), "duplicate api_key") {
		t.Fatalf("error = %q", err)
	}
}

func TestLoadRejectsInboundEnv(t *testing.T) {
	t.Setenv("AI_PROXY_INBOUND_API_KEY", "x")
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
providers:
  openai:
    protocol: openai
    base_url: https://api.openai.com
    api_key: test
    endpoint_capabilities: chat_completions, responses, completions, embeddings
    models: gpt-*
`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "AI_PROXY_INBOUND_API_KEY") {
		t.Fatalf("err = %v", err)
	}
}

func TestLoadRejectsUnknownKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
server:
  typo_key: true
providers:
  openai:
    protocol: openai
    base_url: https://api.openai.com
    api_key: test
    endpoint_capabilities: chat_completions, responses, completions, embeddings
    models: gpt-*
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
    protocol: openai
    base_url: https://api.openai.com
    api_key: test
    endpoint_capabilities: chat_completions, responses, completions, embeddings
    models: gpt-*
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
    endpoint_capabilities: chat_completions, responses, completions, embeddings
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

func TestLoadRejectsFallbacksConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
providers:
  openai:
    protocol: openai
    base_url: https://api.openai.com
    api_key: test
    endpoint_capabilities: chat_completions
    fallbacks: backup
`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "fallbacks is not supported") {
		t.Fatalf("error = %v, want fallbacks not supported", err)
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
    protocol: openai
    base_url: https://api.openai.com
    api_key: test
    endpoint_capabilities: chat_completions, responses, completions, embeddings
    models: gpt-*
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
    protocol: openai
    base_url: https://api.openai.com
    api_key: test
    endpoint_capabilities: chat_completions, responses, completions, embeddings
    models: gpt-*
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
    protocol: openai
    base_url: https://api.openai.com
    api_key: test
    endpoint_capabilities: chat_completions, responses, completions, embeddings
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
    protocol: openai
    base_url: https://api.openai.com
    api_key: test
    endpoint_capabilities: chat_completions, responses, completions, embeddings
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
    protocol: openai
    base_url: not-a-url
    api_key: test
    endpoint_capabilities: chat_completions, responses, completions, embeddings
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
    protocol: openai
    base_url: ftp://example.com
    api_key: test
    endpoint_capabilities: chat_completions, responses, completions, embeddings
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

func TestLoadAllowsCaseDifferentModelCatalogIDs(t *testing.T) {
	// model ID 严格区分大小写:DeepSeek-V4-Flash 与 deepseek-v4-flash / GPT-4o 与 gpt-4o 是不同模型。
	cfg := Config{
		ListenAddr: "127.0.0.1:8080",
		Providers: map[string]Provider{
			"openai": {
				Name: "openai", Protocol: "openai", BaseURL: "https://api.openai.com", APIKey: "a",
				Models:               []string{"gpt-*", "GPT-*"},
				EndpointCapabilities: []string{EndpointCapabilityChatCompletions, EndpointCapabilityEmbeddings},
			},
		},
		ModelCatalog: map[string]ModelInfo{
			"GPT-4o": {
				ID: "GPT-4o", ContextWindowTokens: 128000, MaxOutputTokens: 16384,
				Operations: []string{ModelOperationChatCompletions},
			},
			"gpt-4o": {
				ID: "gpt-4o", ContextWindowTokens: 8192, MaxOutputTokens: 4096,
				Operations: []string{ModelOperationEmbeddings},
			},
		},
	}
	if err := normalize(&cfg); err != nil {
		t.Fatalf("normalize case-different models: %v", err)
	}
	if err := validate(cfg); err != nil {
		t.Fatalf("validate case-different models: %v", err)
	}
	if _, ok := cfg.ModelCatalog["GPT-4o"]; !ok {
		t.Fatal("missing GPT-4o")
	}
	if _, ok := cfg.ModelCatalog["gpt-4o"]; !ok {
		t.Fatal("missing gpt-4o")
	}
	if cfg.ModelCatalog["GPT-4o"].RouteOwner != "openai" || cfg.ModelCatalog["gpt-4o"].RouteOwner != "openai" {
		t.Fatalf("route owners = %#v %#v", cfg.ModelCatalog["GPT-4o"].RouteOwner, cfg.ModelCatalog["gpt-4o"].RouteOwner)
	}
}

func TestCatalogModelsSortedUsesExactIDTieBreak(t *testing.T) {
	catalog := map[string]ModelInfo{
		"deepseek-v4-flash": {ID: "deepseek-v4-flash"},
		"DeepSeek-V4-Flash": {ID: "DeepSeek-V4-Flash"},
	}
	for range 100 {
		items := CatalogModelsSorted(catalog)
		if len(items) != 2 {
			t.Fatalf("items = %#v", items)
		}
		if items[0].ID != "DeepSeek-V4-Flash" || items[1].ID != "deepseek-v4-flash" {
			t.Fatalf("unstable exact-id order = %q, %q", items[0].ID, items[1].ID)
		}
	}
}

func TestProviderMatchesModelRequiresExplicitPatterns(t *testing.T) {
	provider := Provider{Name: "deepseek", Protocol: "openai"}
	if ProviderMatchesModel("deepseek", provider, "deepseek-chat") {
		t.Fatal("provider name/protocol must not infer model patterns")
	}
	provider.Models = []string{"deepseek-*"}
	if !ProviderMatchesModel("deepseek", provider, "deepseek-chat") {
		t.Fatal("explicit case-sensitive model pattern should match")
	}
}

func TestLoadRejectsEnabledProviderWithoutModels(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
providers:
  openai:
    protocol: openai
    base_url: https://api.openai.com
    api_key: test
    endpoint_capabilities: chat_completions
`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "models is required") {
		t.Fatalf("error = %v, want explicit models requirement", err)
	}
}

func TestLoadRejectsExactModelCatalogDuplicate(t *testing.T) {
	cfg := Config{
		ListenAddr: "127.0.0.1:8080",
		Providers: map[string]Provider{
			"openai": {Name: "openai", Protocol: "openai", BaseURL: "https://api.openai.com", APIKey: "a"},
		},
		ModelCatalog: map[string]ModelInfo{
			"gpt-4o": {ID: "gpt-4o", Operations: []string{ModelOperationChatCompletions}},
		},
	}
	// 模拟 map 键与 info.ID 不同但归一化后撞上同一 id 的情况:
	// 再塞一个 name 不同、ID 相同的条目(通过二次写入 ensure 路径不方便,直接调 normalize 前构造)。
	cfg.ModelCatalog["alias"] = ModelInfo{ID: "gpt-4o", Operations: []string{ModelOperationChatCompletions}}
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
    protocol: openai
    base_url: https://api.openai.com
    api_key: test
    endpoint_capabilities: chat_completions, responses, completions, embeddings
    models: gpt-*
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
    protocol: openai
    base_url: https://api.openai.com
    api_key: test
    endpoint_capabilities: chat_completions, responses, completions, embeddings
`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected scheme error")
	}
}

func TestLoadRejectsMissingModelOperations(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
providers:
  openai:
    protocol: openai
    base_url: https://api.openai.com
    api_key: test
    endpoint_capabilities: chat_completions, responses, completions, embeddings
model_catalog:
  gpt-4o:
    context_window_tokens: 128000
    max_output_tokens: 16384
`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "operations") {
		t.Fatalf("error = %v, want missing operations", err)
	}
}

func TestLoadRejectsUnknownModelOperations(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
providers:
  openai:
    protocol: openai
    base_url: https://api.openai.com
    api_key: test
    endpoint_capabilities: chat_completions, responses, completions, embeddings
model_catalog:
  gpt-4o:
    context_window_tokens: 128000
    max_output_tokens: 16384
    operations: chat_completions, responses
`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "unknown operation") {
		t.Fatalf("error = %v, want unknown operation", err)
	}
}

func TestLoadNormalizesModelOperationsOrderAndDedupe(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
providers:
  openai:
    protocol: openai
    base_url: https://api.openai.com
    api_key: test
    endpoint_capabilities: chat_completions, responses, completions, embeddings
    models: multi
model_catalog:
  multi:
    context_window_tokens: 128000
    max_output_tokens: 16384
    operations: embeddings, chat_completions, embeddings, CHAT_COMPLETIONS
`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	ops := cfg.ModelCatalog["multi"].Operations
	if len(ops) != 2 || ops[0] != ModelOperationChatCompletions || ops[1] != ModelOperationEmbeddings {
		t.Fatalf("operations = %#v", ops)
	}
}

func TestLoadRejectsCatalogModelWithoutRoute(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
providers:
  openai:
    protocol: openai
    base_url: https://api.openai.com
    api_key: test
    endpoint_capabilities: chat_completions, responses, completions, embeddings
    models: gpt-*
model_catalog:
  orphan-model:
    context_window_tokens: 128000
    max_output_tokens: 16384
    operations: chat_completions
`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "no enabled provider matches") {
		t.Fatalf("error = %v, want no enabled provider matches", err)
	}
}

func TestLoadRejectsCatalogModelWithAmbiguousRoute(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
providers:
  primary:
    protocol: openai
    base_url: https://a.example
    api_key: test
    endpoint_capabilities: chat_completions, responses, completions, embeddings
    models: shared-*
  backup:
    protocol: openai
    base_url: https://b.example
    api_key: test
    endpoint_capabilities: chat_completions, responses, completions, embeddings
    models: shared-*
model_catalog:
  shared-model:
    context_window_tokens: 128000
    max_output_tokens: 16384
    operations: chat_completions
`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "multiple enabled providers") {
		t.Fatalf("error = %v, want multiple enabled providers", err)
	}
}

func TestLoadRejectsInvalidCatalogCapacity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
providers:
  openai:
    protocol: openai
    base_url: https://api.openai.com
    api_key: test
    endpoint_capabilities: chat_completions, responses, completions, embeddings
    models: gpt-*
model_catalog:
  gpt-4o:
    context_window_tokens: 1000
    max_output_tokens: 1000
    operations: chat_completions
`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "max_output_tokens must be less than context_window_tokens") {
		t.Fatalf("error = %v, want capacity relation error", err)
	}
}

func TestLoadRejectsMissingEndpointCapabilities(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
providers:
  custom-openai:
    protocol: openai
    base_url: https://example.com
    api_key: test
    models: gpt-*
model_catalog:
  gpt-4o:
    context_window_tokens: 128000
    max_output_tokens: 16384
    operations: chat_completions
`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "endpoint_capabilities is required") {
		t.Fatalf("error = %v", err)
	}
}

func TestLoadRejectsUnknownEndpointCapability(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
providers:
  custom-openai:
    protocol: openai
    base_url: https://example.com
    api_key: test
    endpoint_capabilities: chat_completions, widgets
    models: gpt-*
model_catalog:
  gpt-4o:
    context_window_tokens: 128000
    max_output_tokens: 16384
    operations: chat_completions
`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "unknown endpoint capability") {
		t.Fatalf("error = %v", err)
	}
}

func TestLoadRejectsOperationWithoutEndpointCapability(t *testing.T) {
	// openai provider only chat_completions, but catalog claims embeddings.
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
providers:
  custom-openai:
    protocol: openai
    base_url: https://example.com
    api_key: test
    endpoint_capabilities: chat_completions
    models: emb-*
model_catalog:
  emb-model:
    context_window_tokens: 8192
    max_output_tokens: 8191
    operations: embeddings
`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "operation") || !strings.Contains(err.Error(), "endpoint_capabilities") {
		t.Fatalf("error = %v, want operation/capability mismatch", err)
	}
}

func TestLoadNormalizesEndpointCapabilitiesOrderAndDedupe(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
providers:
  custom-openai:
    protocol: openai
    base_url: https://example.com
    api_key: test
    endpoint_capabilities: embeddings, chat_completions, embeddings, RESPONSES
    models: gpt-*
model_catalog:
  gpt-4o:
    context_window_tokens: 128000
    max_output_tokens: 16384
    operations: chat_completions
`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	caps := cfg.Providers["custom-openai"].EndpointCapabilities
	if len(caps) != 3 || caps[0] != EndpointCapabilityChatCompletions || caps[1] != EndpointCapabilityResponses || caps[2] != EndpointCapabilityEmbeddings {
		t.Fatalf("caps = %#v", caps)
	}
}

func TestLoadRejectsOpenAIMessagesCapability(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
providers:
  custom-openai:
    protocol: openai
    base_url: https://example.com
    api_key: test
    endpoint_capabilities: messages
    models: gpt-*
model_catalog:
  gpt-4o:
    context_window_tokens: 128000
    max_output_tokens: 16384
    operations: chat_completions
`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "messages") {
		t.Fatalf("error = %v", err)
	}
}

func TestLoadRejectsAnthropicEmbeddingsCapability(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
providers:
  custom-anthropic:
    protocol: anthropic
    base_url: https://example.com
    api_key: test
    endpoint_capabilities: embeddings
    models: claude-*
model_catalog:
  claude-x:
    context_window_tokens: 200000
    max_output_tokens: 8192
    operations: chat_completions
`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "anthropic") {
		t.Fatalf("error = %v", err)
	}
}

func TestLoadRejectsRemoteEmptyAPIKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
providers:
  remote:
    protocol: openai
    base_url: https://api.example.com
    endpoint_capabilities: chat_completions
    models: m-*
`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "empty api_key") {
		t.Fatalf("error = %v", err)
	}
}

func TestLoadRejectsAllowUnauthenticatedOnRemote(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
providers:
  remote:
    protocol: openai
    base_url: https://api.example.com
    allow_unauthenticated: true
    endpoint_capabilities: chat_completions
    models: m-*
`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "loopback") {
		t.Fatalf("error = %v", err)
	}
}

func TestLoadRejectsAllowUnauthenticatedWithAPIKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
providers:
  local:
    protocol: openai
    base_url: http://127.0.0.1:9000/v1
    api_key: should-not-be-set
    allow_unauthenticated: true
    endpoint_capabilities: chat_completions
    models: local-*
`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("error = %v", err)
	}
}

func TestLoadAllowsLoopbackUnauthenticated(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
providers:
  local:
    protocol: openai
    base_url: http://127.0.0.1:9000/v1
    allow_unauthenticated: true
    endpoint_capabilities: chat_completions
    models: local-*
model_catalog:
  local-model:
    context_window_tokens: 8000
    max_output_tokens: 1000
    operations: chat_completions
`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Providers["local"].AllowUnauthenticated {
		t.Fatal("expected allow_unauthenticated")
	}
}

func TestLoadRejectsMissingProtocol(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
providers:
  openai:
    base_url: https://api.openai.com
    api_key: test
    endpoint_capabilities: chat_completions
`), 0o644); err != nil {
		t.Fatal(err)
	}
	// raw file has no protocol field
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "protocol is required") {
		t.Fatalf("error = %v", err)
	}
}
