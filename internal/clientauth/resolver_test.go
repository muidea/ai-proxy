package clientauth

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

func testIndex(t *testing.T) *Index {
	t.Helper()
	return BuildIndex([]KeyEntry{
		{ID: "codex", APIKey: "sk-codex-secret", Enabled: true},
		{ID: "workorch", APIKey: "sk-workorch-secret", Enabled: true},
		{ID: "disabled-bot", APIKey: "sk-disabled", Enabled: false},
	})
}

func TestResolveOpenAIBearer(t *testing.T) {
	idx := testIndex(t)
	h := http.Header{}
	h.Set("Authorization", "Bearer sk-codex-secret")
	id, err := ResolveHeaders(h, idx)
	if err != nil {
		t.Fatal(err)
	}
	if id.KeyID != "codex" || id.Builtin {
		t.Fatalf("got %+v", id)
	}
}

func TestResolveAnthropicXAPIKey(t *testing.T) {
	idx := testIndex(t)
	h := http.Header{}
	h.Set("X-API-Key", "sk-workorch-secret")
	id, err := ResolveHeaders(h, idx)
	if err != nil {
		t.Fatal(err)
	}
	if id.KeyID != "workorch" {
		t.Fatalf("got %q", id.KeyID)
	}
}

func TestResolveCompatibleHeaders(t *testing.T) {
	idx := testIndex(t)
	// OpenAI 兼容 X-API-Key。
	h := http.Header{}
	h.Set("X-API-Key", "sk-codex-secret")
	id, err := ResolveHeaders(h, idx)
	if err != nil || id.KeyID != "codex" {
		t.Fatalf("openai x-api-key: %+v %v", id, err)
	}
	// Anthropic 兼容 Bearer。
	h = http.Header{}
	h.Set("Authorization", "Bearer sk-workorch-secret")
	id, err = ResolveHeaders(h, idx)
	if err != nil || id.KeyID != "workorch" {
		t.Fatalf("anthropic bearer: %+v %v", id, err)
	}
}

func TestResolveNoHeaderDefault(t *testing.T) {
	idx := testIndex(t)
	id, err := ResolveHeaders(http.Header{}, idx)
	if err != nil {
		t.Fatal(err)
	}
	if !id.Builtin || id.KeyID != DefaultKeyID {
		t.Fatalf("got %+v", id)
	}
}

func TestResolveEmptyHeaderDefault(t *testing.T) {
	idx := testIndex(t)
	h := http.Header{}
	h.Set("Authorization", "   ")
	h.Set("X-API-Key", "")
	id, err := ResolveHeaders(h, idx)
	if err != nil || id.KeyID != DefaultKeyID {
		t.Fatalf("got %+v %v", id, err)
	}
}

func TestResolveMalformedAuthorization(t *testing.T) {
	idx := testIndex(t)
	h := http.Header{}
	h.Set("Authorization", "Basic abc")
	if _, err := ResolveHeaders(h, idx); err != ErrAuthenticationFailed {
		t.Fatalf("err = %v", err)
	}
	h.Set("Authorization", "Bearer")
	if _, err := ResolveHeaders(h, idx); err != ErrAuthenticationFailed {
		t.Fatalf("short bearer err = %v", err)
	}
}

func TestResolveUnknownKey(t *testing.T) {
	idx := testIndex(t)
	h := http.Header{}
	h.Set("Authorization", "Bearer sk-unknown")
	if _, err := ResolveHeaders(h, idx); err != ErrAuthenticationFailed {
		t.Fatalf("err = %v", err)
	}
}

func TestResolveDisabledKey(t *testing.T) {
	idx := testIndex(t)
	h := http.Header{}
	h.Set("X-API-Key", "sk-disabled")
	if _, err := ResolveHeaders(h, idx); err != ErrAuthenticationFailed {
		t.Fatalf("err = %v", err)
	}
}

func TestResolveMatchingHeaders(t *testing.T) {
	idx := testIndex(t)
	h := http.Header{}
	h.Set("Authorization", "Bearer sk-codex-secret")
	h.Set("X-API-Key", "sk-codex-secret")
	id, err := ResolveHeaders(h, idx)
	if err != nil || id.KeyID != "codex" {
		t.Fatalf("got %+v %v", id, err)
	}
}

func TestResolveConflictingHeaders(t *testing.T) {
	idx := testIndex(t)
	h := http.Header{}
	h.Set("Authorization", "Bearer sk-codex-secret")
	h.Set("X-API-Key", "sk-workorch-secret")
	if _, err := ResolveHeaders(h, idx); err != ErrAuthenticationFailed {
		t.Fatalf("err = %v", err)
	}
}

func TestResolveIgnoresQueryAndBody(t *testing.T) {
	idx := testIndex(t)
	// 仅 Header 参与；无 Header 即 default，即使“看起来像”有 key 的 query 也不读。
	req, _ := http.NewRequest(http.MethodPost, "/v1/chat/completions?api_key=sk-codex-secret", strings.NewReader(`{"api_key":"sk-codex-secret"}`))
	id, err := ResolveHeaders(req.Header, idx)
	if err != nil || id.KeyID != DefaultKeyID {
		t.Fatalf("got %+v %v", id, err)
	}
}

func TestAuthErrorDoesNotContainSecrets(t *testing.T) {
	idx := testIndex(t)
	h := http.Header{}
	h.Set("Authorization", "Bearer sk-codex-secret-but-wrong")
	_, err := ResolveHeaders(h, idx)
	if err == nil {
		t.Fatal("expected error")
	}
	msg := err.Error()
	for _, secret := range []string{"sk-codex", "sk-workorch", "codex", "workorch"} {
		if strings.Contains(msg, secret) {
			t.Fatalf("error leaks %q: %s", secret, msg)
		}
	}
}

func TestContextRoundTrip(t *testing.T) {
	ctx := WithClientIdentity(context.Background(), ClientIdentity{KeyID: "codex"})
	got := ClientIdentityFromContext(ctx)
	if got.KeyID != "codex" {
		t.Fatalf("got %+v", got)
	}
	if ClientIdentityFromContext(context.Background()).KeyID != DefaultKeyID {
		t.Fatal("missing context should default")
	}
}

func TestEmptyIndexAllDefault(t *testing.T) {
	idx := BuildIndex(nil)
	id, err := ResolveHeaders(http.Header{}, idx)
	if err != nil || id.KeyID != DefaultKeyID {
		t.Fatalf("got %+v %v", id, err)
	}
	h := http.Header{}
	h.Set("Authorization", "Bearer anything")
	if _, err := ResolveHeaders(h, idx); err != ErrAuthenticationFailed {
		t.Fatalf("unknown against empty index: %v", err)
	}
}
