package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func minimalProvidersYAML() string {
	return `
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
}

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadAdminAuthDefaultsWhenDisabled(t *testing.T) {
	path := writeConfig(t, "server:\n  listen_addr: 127.0.0.1:8080\n"+minimalProvidersYAML())
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AdminAuth.Enabled {
		t.Fatal("admin auth should default to disabled")
	}
	if cfg.AdminAuth.BasePath != DefaultAdminBasePath {
		t.Fatalf("base path = %q", cfg.AdminAuth.BasePath)
	}
	if cfg.AdminAuth.SessionTTLSeconds != DefaultAdminSessionTTLSeconds {
		t.Fatalf("ttl = %d", cfg.AdminAuth.SessionTTLSeconds)
	}
	if cfg.AdminAuth.SessionCookieSecure {
		t.Fatal("session cookie secure should default to false")
	}
}

func TestLoadAdminAuthEnabledRequiresCredentials(t *testing.T) {
	path := writeConfig(t, `
server:
  admin_auth_enabled: true
  admin_username: ops
`+minimalProvidersYAML())
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "admin_password_hash") {
		t.Fatalf("err = %v", err)
	}
}

func TestLoadAdminAuthEnabledWithValidHash(t *testing.T) {
	hash, err := HashAdminPassword("correct-horse-battery-staple")
	if err != nil {
		t.Fatal(err)
	}
	path := writeConfig(t, `
server:
  admin_auth_enabled: true
  admin_base_path: /ops/ai-proxy
  admin_username: ops-admin
  admin_password_hash: `+hash+`
  admin_session_cookie_secure: true
  admin_session_ttl_seconds: 3600
`+minimalProvidersYAML())
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.AdminAuth.Enabled || cfg.AdminAuth.Username != "ops-admin" || cfg.AdminAuth.BasePath != "/ops/ai-proxy" || !cfg.AdminAuth.SessionCookieSecure || cfg.AdminAuth.SessionTTLSeconds != 3600 {
		t.Fatalf("auth = %+v", cfg.AdminAuth)
	}
	if !VerifyAdminPassword("correct-horse-battery-staple", cfg.AdminAuth.PasswordHash) {
		t.Fatal("password verification failed")
	}
	if VerifyAdminPassword("wrong", cfg.AdminAuth.PasswordHash) {
		t.Fatal("wrong password accepted")
	}
}

func TestLoadAdminAuthEnvOverrides(t *testing.T) {
	hash, err := HashAdminPassword("env-password")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("AI_PROXY_ADMIN_AUTH_ENABLED", "true")
	t.Setenv("AI_PROXY_ADMIN_BASE_PATH", "/secure-admin")
	t.Setenv("AI_PROXY_ADMIN_USERNAME", "env-admin")
	t.Setenv("AI_PROXY_ADMIN_PASSWORD_HASH", hash)
	t.Setenv("AI_PROXY_ADMIN_SESSION_COOKIE_SECURE", "true")
	t.Setenv("AI_PROXY_ADMIN_SESSION_TTL_SECONDS", "600")
	path := writeConfig(t, "server:\n  listen_addr: 127.0.0.1:8080\n"+minimalProvidersYAML())
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.AdminAuth.Enabled || cfg.AdminAuth.BasePath != "/secure-admin" || cfg.AdminAuth.Username != "env-admin" || !cfg.AdminAuth.SessionCookieSecure || cfg.AdminAuth.SessionTTLSeconds != 600 {
		t.Fatalf("auth = %+v", cfg.AdminAuth)
	}
}

func TestLoadAdminAuthRejectsInvalidBasePath(t *testing.T) {
	// # 会在配置解析时被 stripComment 当作注释起点；% ? 等同理直接校验。
	cases := []string{"admin", "/admin/", "/ops/../admin", "/ops//admin", "/a%20b", "/a?b", "/"}
	for _, base := range cases {
		path := writeConfig(t, "server:\n  admin_base_path: "+base+"\n"+minimalProvidersYAML())
		if _, err := Load(path); err == nil {
			t.Fatalf("base path %q should fail", base)
		}
	}
	for _, base := range []string{"/a#b", "/a?x", "/a%20b"} {
		if err := validateAdminBasePath(base); err == nil {
			t.Fatalf("validateAdminBasePath(%q) should fail", base)
		}
	}
}

func TestLoadAdminAuthRejectsInvalidTTL(t *testing.T) {
	hash, err := HashAdminPassword("x")
	if err != nil {
		t.Fatal(err)
	}
	path := writeConfig(t, `
server:
  admin_auth_enabled: true
  admin_username: ops
  admin_password_hash: `+hash+`
  admin_session_ttl_seconds: 60
`+minimalProvidersYAML())
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "admin_session_ttl_seconds") {
		t.Fatalf("err = %v", err)
	}
}

func TestLoadAdminAuthDisabledWithCredentialsIsWarningOnly(t *testing.T) {
	hash, err := HashAdminPassword("x")
	if err != nil {
		t.Fatal(err)
	}
	path := writeConfig(t, `
server:
  admin_auth_enabled: false
  admin_username: leftover
  admin_password_hash: `+hash+`
`+minimalProvidersYAML())
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AdminAuth.Enabled {
		t.Fatal("should remain disabled")
	}
}

func TestParseAdminPasswordHashRejectsWeakParams(t *testing.T) {
	// m=1024 is too weak / not our fixed params.
	weak := "$argon2id$v=19$m=1024,t=3,p=1$AAAAAAAAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	if _, err := ParseAdminPasswordHash(weak); err == nil {
		t.Fatal("weak params should fail")
	}
}

func TestHashAdminPasswordRoundTrip(t *testing.T) {
	phc, err := HashAdminPassword("s3cret!")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(phc, "$argon2id$v=19$m=65536,t=3,p=1$") {
		t.Fatalf("phc = %s", phc)
	}
	if !VerifyAdminPassword("s3cret!", phc) {
		t.Fatal("round-trip failed")
	}
	// leading-$ optional form
	if !VerifyAdminPassword("s3cret!", strings.TrimPrefix(phc, "$")) {
		t.Fatal("no-leading-$ form failed")
	}
}

func TestConstantTimeUsernameEqual(t *testing.T) {
	if !ConstantTimeUsernameEqual("ops", "ops") {
		t.Fatal("equal failed")
	}
	if ConstantTimeUsernameEqual("ops", "Ops") {
		t.Fatal("case should matter")
	}
}

func TestAdminAuthFingerprintChangesWithAuthFields(t *testing.T) {
	a := AdminAuthConfig{Enabled: true, BasePath: "/admin", Username: "u", PasswordHash: "h1", SessionTTLSeconds: 300}
	b := a
	b.PasswordHash = "h2"
	if AdminAuthFingerprint(a) == AdminAuthFingerprint(b) {
		t.Fatal("fingerprint should change with password hash")
	}
	b = a
	b.SessionCookieSecure = true
	if AdminAuthFingerprint(a) == AdminAuthFingerprint(b) {
		t.Fatal("fingerprint should change with session cookie secure")
	}
}
