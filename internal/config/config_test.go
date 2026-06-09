package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadConfigFileAndEnv(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "env-openai-key")
	t.Setenv("AI_PROXY_PORT", "18080")
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
default_provider: deepseek
`), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ListenAddr != ":18080" {
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
