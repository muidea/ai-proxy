package aiproxy

import (
	"os"
	"path/filepath"
	"testing"

	"ai-proxy/internal/pkg/aiproxyconfig"
)

func TestSetAdminCredentialsCreatesAndResetsCredentials(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := `server:
  listen_addr: 127.0.0.1:8080
providers:
  openai:
    enabled: true
    protocol: openai
    base_url: https://api.openai.com/v1
    api_key: test-key
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
	firstHash, err := config.HashAdminPassword("first-password")
	if err != nil {
		t.Fatal(err)
	}
	if err := setAdminCredentials(path, "ops-admin", firstHash); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.AdminAuth.Enabled || cfg.AdminAuth.Username != "ops-admin" || !config.VerifyAdminPassword("first-password", cfg.AdminAuth.PasswordHash) {
		t.Fatalf("admin auth = %+v", cfg.AdminAuth)
	}

	secondHash, err := config.HashAdminPassword("second-password")
	if err != nil {
		t.Fatal(err)
	}
	if err := setAdminCredentials(path, "new-admin", secondHash); err != nil {
		t.Fatal(err)
	}
	cfg, err = config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AdminAuth.Username != "new-admin" || !config.VerifyAdminPassword("second-password", cfg.AdminAuth.PasswordHash) || config.VerifyAdminPassword("first-password", cfg.AdminAuth.PasswordHash) {
		t.Fatalf("reset admin auth = %+v", cfg.AdminAuth)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o, want 600", info.Mode().Perm())
	}
}

func TestSetAdminCredentialsRejectsInvalidUsernameWithoutWriting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	content := `server:
  listen_addr: 127.0.0.1:8080
providers:
  openai:
    enabled: true
    protocol: openai
    base_url: https://api.openai.com/v1
    api_key: test-key
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
	hash, err := config.HashAdminPassword("password")
	if err != nil {
		t.Fatal(err)
	}
	if err := setAdminCredentials(path, "bad:name", hash); err == nil {
		t.Fatal("expected invalid username error")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != content {
		t.Fatal("invalid credentials changed config file")
	}
}
