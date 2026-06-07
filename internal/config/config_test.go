package config

import (
	"os"
	"path/filepath"
	"testing"
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
providers:
  deepseek:
    base_url: https://api.deepseek.com
    api_key: ${OPENAI_API_KEY}
    models: deepseek*
    fallbacks: openai, custom
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
