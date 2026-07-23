package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"ai-proxy/internal/pkg/aiproxyconfig"
)

func enabledAuthConfig(t *testing.T, username, password string) config.AdminAuthConfig {
	t.Helper()
	hash, err := config.HashAdminPassword(password)
	if err != nil {
		t.Fatal(err)
	}
	return config.AdminAuthConfig{
		Enabled:           true,
		BasePath:          "/ops/ai-proxy",
		Username:          username,
		PasswordHash:      hash,
		SessionTTLSeconds: 3600,
	}
}

func newAuthHandler(t *testing.T, auth config.AdminAuthConfig) *Handler {
	t.Helper()
	cfg := config.Config{
		AdminAuth: auth,
		Providers: map[string]config.Provider{"openai": {Name: "openai", Protocol: "openai"}},
	}
	h := NewHandler("", &testRuntime{cfg: cfg})
	return h
}

func TestAuthDisabledRejectsRemote(t *testing.T) {
	h := newAuthHandler(t, config.AdminAuthConfig{Enabled: false, BasePath: "/admin", SessionTTLSeconds: 28800})
	req := httptest.NewRequest(http.MethodGet, "/admin/api/providers", nil)
	req.RemoteAddr = "203.0.113.8:9"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestAuthEnabledAllowsRemoteLoginFlow(t *testing.T) {
	auth := enabledAuthConfig(t, "ops-admin", "s3cret-pass")
	h := newAuthHandler(t, auth)

	// unauthenticated page -> 303 login
	req := httptest.NewRequest(http.MethodGet, "/ops/ai-proxy/", nil)
	req.RemoteAddr = "203.0.113.8:9"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/ops/ai-proxy/login" {
		t.Fatalf("page redirect = %d %s", rec.Code, rec.Header().Get("Location"))
	}

	// unauthenticated API -> 401 JSON
	req = httptest.NewRequest(http.MethodGet, "/ops/ai-proxy/api/providers", nil)
	req.RemoteAddr = "203.0.113.8:9"
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), "admin_authentication_required") {
		t.Fatalf("api = %d %s", rec.Code, rec.Body.String())
	}

	// wrong password
	req = httptest.NewRequest(http.MethodPost, "/ops/ai-proxy/api/auth/login", strings.NewReader(`{"username":"ops-admin","password":"wrong"}`))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "203.0.113.8:9"
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), "invalid username or password") {
		t.Fatalf("bad login = %d %s", rec.Code, rec.Body.String())
	}

	// correct login
	req = httptest.NewRequest(http.MethodPost, "/ops/ai-proxy/api/auth/login", strings.NewReader(`{"username":"ops-admin","password":"s3cret-pass"}`))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "203.0.113.8:9"
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("login = %d %s", rec.Code, rec.Body.String())
	}
	var sess sessionView
	if err := json.Unmarshal(rec.Body.Bytes(), &sess); err != nil {
		t.Fatal(err)
	}
	if sess.Username != "ops-admin" || sess.CSRFToken == "" {
		t.Fatalf("session = %+v", sess)
	}
	cookie := rec.Result().Cookies()
	if len(cookie) == 0 || cookie[0].Name != adminSessionCookieName || !cookie[0].HttpOnly || cookie[0].Secure || cookie[0].Path != "/ops/ai-proxy" {
		t.Fatalf("cookie = %+v", cookie)
	}

	// session endpoint
	req = httptest.NewRequest(http.MethodGet, "/ops/ai-proxy/api/auth/session", nil)
	req.RemoteAddr = "203.0.113.8:9"
	req.AddCookie(cookie[0])
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), sess.CSRFToken) {
		t.Fatalf("session get = %d %s", rec.Code, rec.Body.String())
	}

	// mutation without CSRF -> 403
	req = httptest.NewRequest(http.MethodPut, "/ops/ai-proxy/api/providers", strings.NewReader(`{"providers":[]}`))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "203.0.113.8:9"
	req.AddCookie(cookie[0])
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("missing csrf = %d %s", rec.Code, rec.Body.String())
	}

	// logout with CSRF
	req = httptest.NewRequest(http.MethodPost, "/ops/ai-proxy/api/auth/logout", nil)
	req.Header.Set(adminCSRFHeader, sess.CSRFToken)
	req.RemoteAddr = "203.0.113.8:9"
	req.AddCookie(cookie[0])
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("logout = %d %s", rec.Code, rec.Body.String())
	}

	// old cookie invalid
	req = httptest.NewRequest(http.MethodGet, "/ops/ai-proxy/api/providers", nil)
	req.RemoteAddr = "203.0.113.8:9"
	req.AddCookie(cookie[0])
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("after logout = %d", rec.Code)
	}
}

func TestAuthLoginRateLimit(t *testing.T) {
	auth := enabledAuthConfig(t, "ops-admin", "s3cret-pass")
	h := newAuthHandler(t, auth)
	for i := 0; i < loginFailLimit; i++ {
		req := httptest.NewRequest(http.MethodPost, "/ops/ai-proxy/api/auth/login", strings.NewReader(`{"username":"ops-admin","password":"bad"}`))
		req.Header.Set("Content-Type", "application/json")
		req.RemoteAddr = "198.51.100.2:1"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("fail %d = %d", i, rec.Code)
		}
	}
	req := httptest.NewRequest(http.MethodPost, "/ops/ai-proxy/api/auth/login", strings.NewReader(`{"username":"ops-admin","password":"s3cret-pass"}`))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "198.51.100.2:1"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("rate limit = %d %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Fatal("missing Retry-After")
	}
}

func TestAuthCSRFAllowsHTTPSOriginWhenTLSTerminatesAtProxy(t *testing.T) {
	auth := enabledAuthConfig(t, "ops-admin", "s3cret-pass")
	h := newAuthHandler(t, auth)

	login := httptest.NewRequest(http.MethodPost, "/ops/ai-proxy/api/auth/login", strings.NewReader(`{"username":"ops-admin","password":"s3cret-pass"}`))
	login.Header.Set("Content-Type", "application/json")
	login.RemoteAddr = "203.0.113.8:9"
	login.Host = "admin.example.test"
	loginRec := httptest.NewRecorder()
	h.ServeHTTP(loginRec, login)
	if loginRec.Code != http.StatusOK {
		t.Fatalf("login = %d %s", loginRec.Code, loginRec.Body.String())
	}
	var sess sessionView
	if err := json.Unmarshal(loginRec.Body.Bytes(), &sess); err != nil {
		t.Fatal(err)
	}
	cookie := loginRec.Result().Cookies()[0]

	// 后端连接是 HTTP（TLS 在代理终止），浏览器仍发送外部 HTTPS Origin。
	logout := httptest.NewRequest(http.MethodPost, "/ops/ai-proxy/api/auth/logout", nil)
	logout.RemoteAddr = "203.0.113.8:9"
	logout.Host = "admin.example.test"
	logout.Header.Set("Origin", "https://admin.example.test")
	logout.Header.Set(adminCSRFHeader, sess.CSRFToken)
	logout.AddCookie(cookie)
	logoutRec := httptest.NewRecorder()
	h.ServeHTTP(logoutRec, logout)
	if logoutRec.Code != http.StatusNoContent {
		t.Fatalf("proxy HTTPS origin logout = %d %s", logoutRec.Code, logoutRec.Body.String())
	}

	// 同一 Host 的 HTTP 部署也被支持。
	login = httptest.NewRequest(http.MethodPost, "/ops/ai-proxy/api/auth/login", strings.NewReader(`{"username":"ops-admin","password":"s3cret-pass"}`))
	login.Header.Set("Content-Type", "application/json")
	login.RemoteAddr = "203.0.113.8:9"
	login.Host = "admin.example.test"
	loginRec = httptest.NewRecorder()
	h.ServeHTTP(loginRec, login)
	if loginRec.Code != http.StatusOK {
		t.Fatalf("second login = %d %s", loginRec.Code, loginRec.Body.String())
	}
	if err := json.Unmarshal(loginRec.Body.Bytes(), &sess); err != nil {
		t.Fatal(err)
	}
	cookie = loginRec.Result().Cookies()[0]
	logout = httptest.NewRequest(http.MethodPost, "/ops/ai-proxy/api/auth/logout", nil)
	logout.RemoteAddr = "203.0.113.8:9"
	logout.Host = "admin.example.test"
	logout.Header.Set("Origin", "http://admin.example.test")
	logout.Header.Set(adminCSRFHeader, sess.CSRFToken)
	logout.AddCookie(cookie)
	logoutRec = httptest.NewRecorder()
	h.ServeHTTP(logoutRec, logout)
	if logoutRec.Code != http.StatusNoContent {
		t.Fatalf("HTTP origin logout = %d %s", logoutRec.Code, logoutRec.Body.String())
	}
}

func TestAuthSessionCookieSecureFollowsConfig(t *testing.T) {
	auth := enabledAuthConfig(t, "ops-admin", "s3cret-pass")
	auth.SessionCookieSecure = true
	h := newAuthHandler(t, auth)
	req := httptest.NewRequest(http.MethodPost, "/ops/ai-proxy/api/auth/login", strings.NewReader(`{"username":"ops-admin","password":"s3cret-pass"}`))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "203.0.113.8:9"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("login = %d %s", rec.Code, rec.Body.String())
	}
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 || !cookies[0].Secure {
		t.Fatalf("cookie = %+v, want Secure=true", cookies)
	}
}

func TestAuthRejectsAdminBasePathHotUpdate(t *testing.T) {
	auth := enabledAuthConfig(t, "ops-admin", "s3cret-pass")
	h := newAuthHandler(t, auth)
	cfg := h.runtime.ConfigSnapshot()
	cfg.AdminAuth.BasePath = "/new-admin"
	if err := h.activateConfig(cfg); err != errAdminBasePathRestart {
		t.Fatalf("activateConfig error = %v, want %v", err, errAdminBasePathRestart)
	}
	if got := h.adminBasePath(); got != "/ops/ai-proxy" {
		t.Fatalf("base path changed to %q", got)
	}
}

func TestAuthConfigHotUpdateClearsSessions(t *testing.T) {
	auth := enabledAuthConfig(t, "ops-admin", "s3cret-pass")
	h := newAuthHandler(t, auth)

	req := httptest.NewRequest(http.MethodPost, "/ops/ai-proxy/api/auth/login", strings.NewReader(`{"username":"ops-admin","password":"s3cret-pass"}`))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "203.0.113.8:9"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("login = %d", rec.Code)
	}
	cookie := rec.Result().Cookies()[0]

	// 无关配置热更新:会话保持
	cfg := h.runtime.ConfigSnapshot()
	cfg.DebugLog = true
	if err := h.activateConfig(cfg); err != nil {
		t.Fatal(err)
	}
	req = httptest.NewRequest(http.MethodGet, "/ops/ai-proxy/api/auth/session", nil)
	req.RemoteAddr = "203.0.113.8:9"
	req.AddCookie(cookie)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("session after unrelated update = %d", rec.Code)
	}

	// 认证配置变更:会话清空
	cfg = h.runtime.ConfigSnapshot()
	cfg.AdminAuth.Username = "other"
	if err := h.activateConfig(cfg); err != nil {
		t.Fatal(err)
	}
	req = httptest.NewRequest(http.MethodGet, "/ops/ai-proxy/api/auth/session", nil)
	req.RemoteAddr = "203.0.113.8:9"
	req.AddCookie(cookie)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("session after auth update = %d", rec.Code)
	}
}

func TestAuthSessionExpiry(t *testing.T) {
	auth := enabledAuthConfig(t, "ops-admin", "s3cret-pass")
	auth.SessionTTLSeconds = 300
	h := newAuthHandler(t, auth)
	now := time.Now()
	h.auth.clock = func() time.Time { return now }

	req := httptest.NewRequest(http.MethodPost, "/ops/ai-proxy/api/auth/login", strings.NewReader(`{"username":"ops-admin","password":"s3cret-pass"}`))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "203.0.113.8:9"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("login = %d", rec.Code)
	}
	cookie := rec.Result().Cookies()[0]

	now = now.Add(301 * time.Second)
	req = httptest.NewRequest(http.MethodGet, "/ops/ai-proxy/api/providers", nil)
	req.RemoteAddr = "203.0.113.8:9"
	req.AddCookie(cookie)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expired = %d", rec.Code)
	}
}

func TestAuthIndexInjectsBasePath(t *testing.T) {
	auth := enabledAuthConfig(t, "ops-admin", "s3cret-pass")
	h := newAuthHandler(t, auth)
	// login first
	req := httptest.NewRequest(http.MethodPost, "/ops/ai-proxy/api/auth/login", strings.NewReader(`{"username":"ops-admin","password":"s3cret-pass"}`))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "127.0.0.1:1"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	cookie := rec.Result().Cookies()[0]

	req = httptest.NewRequest(http.MethodGet, "/ops/ai-proxy/", nil)
	req.RemoteAddr = "127.0.0.1:1"
	req.AddCookie(cookie)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("index = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `window.__AI_PROXY_ADMIN_BASE_PATH__="/ops/ai-proxy"`) {
		t.Fatalf("missing base path injection")
	}
	if !strings.Contains(rec.Body.String(), "apiURL") {
		t.Fatalf("frontend should use apiURL helper")
	}
}

func TestAuthOldAdminPathNotFoundWhenCustomBase(t *testing.T) {
	auth := enabledAuthConfig(t, "ops-admin", "s3cret-pass")
	h := newAuthHandler(t, auth)
	req := httptest.NewRequest(http.MethodGet, "/admin/", nil)
	req.RemoteAddr = "127.0.0.1:1"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("old path = %d", rec.Code)
	}
}

func TestAuthBasePathDoesNotPrefixMatchSibling(t *testing.T) {
	auth := enabledAuthConfig(t, "ops-admin", "s3cret-pass")
	h := newAuthHandler(t, auth)
	req := httptest.NewRequest(http.MethodGet, "/ops/ai-proxy-extra", nil)
	req.RemoteAddr = "127.0.0.1:1"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("sibling path = %d", rec.Code)
	}
}
