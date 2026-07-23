package admin

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"ai-proxy/internal/pkg/aiproxyconfig"
)

const (
	adminSessionCookieName = "ai_proxy_admin_session"
	adminCSRFHeader        = "X-AI-Proxy-CSRF"
	adminWriteHeader       = "X-AI-Proxy-Admin"
	maxAdminSessions       = 64
	loginFailLimit         = 5
	loginLockoutDuration   = 15 * time.Minute
	sessionIDBytes         = 32
	csrfTokenBytes         = 32
)

// authState 是 Handler 持有的内存会话与限速状态。
type authState struct {
	mu       sync.Mutex
	sessions map[string]*adminSession
	failures map[string]*loginFailure
	// clock 可注入,默认 time.Now。
	clock func() time.Time
	// random 可注入,默认 crypto/rand.Read。
	random func([]byte) (int, error)
	// 当前生效的认证配置快照。
	cfg config.AdminAuthConfig
	// fingerprint 用于热更新时判断是否需要清空会话。
	fingerprint string
}

type adminSession struct {
	ID        string
	Username  string
	IssuedAt  time.Time
	ExpiresAt time.Time
	CSRFToken string
}

type loginFailure struct {
	Count       int
	LockedUntil time.Time
}

type sessionView struct {
	Username  string `json:"username"`
	ExpiresAt string `json:"expires_at"`
	CSRFToken string `json:"csrf_token"`
}

type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

func newAuthState(cfg config.AdminAuthConfig) *authState {
	return &authState{
		sessions:    map[string]*adminSession{},
		failures:    map[string]*loginFailure{},
		clock:       time.Now,
		random:      rand.Read,
		cfg:         cfg,
		fingerprint: config.AdminAuthFingerprint(cfg),
	}
}

// applyAuthConfig 在 RuntimeConfig 成功激活后调用。
// 认证相关字段变化时清空全部会话与登录限速。
func (s *authState) applyAuthConfig(cfg config.AdminAuthConfig) {
	if s == nil {
		return
	}
	fp := config.AdminAuthFingerprint(cfg)
	s.mu.Lock()
	defer s.mu.Unlock()
	if fp != s.fingerprint {
		s.sessions = map[string]*adminSession{}
		s.failures = map[string]*loginFailure{}
		s.fingerprint = fp
	}
	s.cfg = cfg
}

func (s *authState) enabled() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cfg.Enabled
}

func (s *authState) snapshot() config.AdminAuthConfig {
	if s == nil {
		return config.AdminAuthConfig{BasePath: config.DefaultAdminBasePath, SessionTTLSeconds: config.DefaultAdminSessionTTLSeconds}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cfg
}

func (s *authState) now() time.Time {
	if s == nil || s.clock == nil {
		return time.Now()
	}
	return s.clock()
}

func (s *authState) readRandom(b []byte) error {
	fn := rand.Read
	if s != nil && s.random != nil {
		fn = s.random
	}
	_, err := fn(b)
	return err
}

func (s *authState) purgeExpiredLocked(now time.Time) {
	for id, sess := range s.sessions {
		if !sess.ExpiresAt.After(now) {
			delete(s.sessions, id)
		}
	}
	for addr, fail := range s.failures {
		if !fail.LockedUntil.IsZero() && now.After(fail.LockedUntil) && fail.Count == 0 {
			delete(s.failures, addr)
		}
		if fail.Count == 0 && fail.LockedUntil.IsZero() {
			delete(s.failures, addr)
		}
	}
}

func (s *authState) createSession(username string) (*adminSession, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	s.purgeExpiredLocked(now)
	if len(s.sessions) >= maxAdminSessions {
		return nil, errSessionCapacity
	}
	sid, err := s.randomToken(sessionIDBytes)
	if err != nil {
		return nil, err
	}
	csrf, err := s.randomToken(csrfTokenBytes)
	if err != nil {
		return nil, err
	}
	ttl := time.Duration(s.cfg.SessionTTLSeconds) * time.Second
	if ttl <= 0 {
		ttl = time.Duration(config.DefaultAdminSessionTTLSeconds) * time.Second
	}
	sess := &adminSession{
		ID:        sid,
		Username:  username,
		IssuedAt:  now,
		ExpiresAt: now.Add(ttl),
		CSRFToken: csrf,
	}
	s.sessions[sid] = sess
	return sess, nil
}

func (s *authState) randomToken(n int) (string, error) {
	buf := make([]byte, n)
	if err := s.readRandom(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

func (s *authState) lookup(sessionID string) *adminSession {
	if s == nil || sessionID == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	s.purgeExpiredLocked(now)
	sess := s.sessions[sessionID]
	if sess == nil || !sess.ExpiresAt.After(now) {
		if sess != nil {
			delete(s.sessions, sessionID)
		}
		return nil
	}
	// 返回副本,避免外部持有指针时竞态。
	cp := *sess
	return &cp
}

func (s *authState) delete(sessionID string) {
	if s == nil || sessionID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, sessionID)
}

var (
	errSessionCapacity      = errString("session capacity reached")
	errAdminBasePathRestart = errString("admin_base_path changes require process restart")
)

type errString string

func (e errString) Error() string { return string(e) }

func (s *authState) checkLoginAllowed(remoteAddr string) (retryAfter time.Duration, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	s.purgeExpiredLocked(now)
	key := clientIP(remoteAddr)
	fail := s.failures[key]
	if fail == nil {
		return 0, true
	}
	if !fail.LockedUntil.IsZero() && now.Before(fail.LockedUntil) {
		return fail.LockedUntil.Sub(now), false
	}
	if !fail.LockedUntil.IsZero() && !now.Before(fail.LockedUntil) {
		// 锁定期结束,重置计数。
		delete(s.failures, key)
	}
	return 0, true
}

func (s *authState) recordLoginFailure(remoteAddr string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	key := clientIP(remoteAddr)
	fail := s.failures[key]
	if fail == nil {
		fail = &loginFailure{}
		s.failures[key] = fail
	}
	fail.Count++
	if fail.Count >= loginFailLimit {
		fail.LockedUntil = now.Add(loginLockoutDuration)
		fail.Count = 0
	}
}

func (s *authState) clearLoginFailures(remoteAddr string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.failures, clientIP(remoteAddr))
}

func clientIP(remoteAddr string) string {
	host, _, err := net.SplitHostPort(strings.TrimSpace(remoteAddr))
	if err != nil {
		return strings.TrimSpace(remoteAddr)
	}
	return host
}

func (h *Handler) authEnabled() bool {
	return h.auth != nil && h.auth.enabled()
}

func (h *Handler) adminBasePath() string {
	if h.auth != nil {
		if p := strings.TrimSpace(h.auth.snapshot().BasePath); p != "" {
			return p
		}
	}
	if h.runtime != nil {
		if p := strings.TrimSpace(h.runtime.ConfigSnapshot().AdminAuth.BasePath); p != "" {
			return p
		}
	}
	return config.DefaultAdminBasePath
}

// activateConfig 收口 RuntimeConfig.UpdateConfig:先激活,再应用认证设置。
// 任何 Admin 配置写入路径都必须复用本方法。
func (h *Handler) activateConfig(cfg config.Config) error {
	if err := h.requireUnchangedAdminBasePath(cfg); err != nil {
		return err
	}
	if err := h.runtime.UpdateConfig(cfg); err != nil {
		return err
	}
	if h.auth == nil {
		h.auth = newAuthState(cfg.AdminAuth)
	} else {
		h.auth.applyAuthConfig(cfg.AdminAuth)
	}
	return nil
}

// requireUnchangedAdminBasePath 防止热更新改变启动期注册的路由。
// 候选配置在写入正式文件前也必须调用本检查，避免磁盘与运行时配置分叉。
func (h *Handler) requireUnchangedAdminBasePath(cfg config.Config) error {
	if cfg.AdminAuth.BasePath != h.adminBasePath() {
		return errAdminBasePathRestart
	}
	return nil
}

func (h *Handler) sessionFromRequest(r *http.Request) *adminSession {
	if h.auth == nil {
		return nil
	}
	c, err := r.Cookie(adminSessionCookieName)
	if err != nil || c == nil || c.Value == "" {
		return nil
	}
	return h.auth.lookup(c.Value)
}

func setSessionCookie(w http.ResponseWriter, basePath string, sessionID string, maxAge int, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name:     adminSessionCookieName,
		Value:    sessionID,
		Path:     basePath,
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   secure,
		MaxAge:   maxAge,
	})
}

func clearSessionCookie(w http.ResponseWriter, basePath string, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name:     adminSessionCookieName,
		Value:    "",
		Path:     basePath,
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   secure,
		MaxAge:   -1,
	})
}

func writeAdminAuthError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]string{
			"code":    code,
			"message": message,
		},
	})
}

func (h *Handler) requireSession(w http.ResponseWriter, r *http.Request) *adminSession {
	sess := h.sessionFromRequest(r)
	if sess == nil {
		writeAdminAuthError(w, http.StatusUnauthorized, "admin_authentication_required", "admin login is required")
		return nil
	}
	return sess
}

func originAllowed(r *http.Request) bool {
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin == "" {
		// 缺失 Origin 的非浏览器客户端允许继续使用 CSRF token。
		return true
	}
	// 同源浏览器请求的 Origin 仅允许 http(s)://Host。为同时支持直连 HTTP、
	// HTTPS 和 TLS 在反向代理终止的部署，不以后端 r.TLS 推断协议，也不信任
	// X-Forwarded-Proto；代理必须保留外部 Host。
	return origin == "http://"+r.Host || origin == "https://"+r.Host
}

func (h *Handler) requireCSRF(w http.ResponseWriter, r *http.Request, sess *adminSession) bool {
	if sess == nil {
		return false
	}
	token := strings.TrimSpace(r.Header.Get(adminCSRFHeader))
	if token == "" || subtle.ConstantTimeCompare([]byte(token), []byte(sess.CSRFToken)) != 1 {
		writeAdminAuthError(w, http.StatusForbidden, "admin_csrf_invalid", "invalid csrf token")
		return false
	}
	if !originAllowed(r) {
		writeAdminAuthError(w, http.StatusForbidden, "admin_origin_invalid", "invalid origin")
		return false
	}
	return true
}

// requireAdminMutation 在认证开启时要求会话+CSRF;关闭时要求 X-AI-Proxy-Admin: 1。
func (h *Handler) requireAdminMutation(w http.ResponseWriter, r *http.Request) bool {
	if h.authEnabled() {
		sess := h.requireSession(w, r)
		if sess == nil {
			return false
		}
		return h.requireCSRF(w, r, sess)
	}
	if r.Header.Get(adminWriteHeader) != "1" {
		writeError(w, http.StatusForbidden, "missing admin request header")
		return false
	}
	return true
}

func (h *Handler) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}
	if !h.authEnabled() {
		writeError(w, http.StatusNotFound, "admin auth is disabled")
		return
	}
	if retry, ok := h.auth.checkLoginAllowed(r.RemoteAddr); !ok {
		w.Header().Set("Retry-After", formatRetryAfter(retry))
		writeAdminAuthError(w, http.StatusTooManyRequests, "admin_login_rate_limited", "too many login attempts")
		return
	}
	var input loginRequest
	if !decodeAdminJSON(w, r, &input) {
		return
	}
	authCfg := h.auth.snapshot()
	userOK := config.ConstantTimeUsernameEqual(input.Username, authCfg.Username)
	passOK := config.VerifyAdminPassword(input.Password, authCfg.PasswordHash)
	if !userOK || !passOK {
		h.auth.recordLoginFailure(r.RemoteAddr)
		// 统一错误,不区分账号/密码。
		writeAdminAuthError(w, http.StatusUnauthorized, "admin_authentication_failed", "invalid username or password")
		return
	}
	h.auth.clearLoginFailures(r.RemoteAddr)
	sess, err := h.auth.createSession(authCfg.Username)
	if err != nil {
		writeAdminAuthError(w, http.StatusServiceUnavailable, "admin_session_unavailable", "unable to create session")
		return
	}
	maxAge := authCfg.SessionTTLSeconds
	if maxAge <= 0 {
		maxAge = config.DefaultAdminSessionTTLSeconds
	}
	setSessionCookie(w, h.adminBasePath(), sess.ID, maxAge, authCfg.SessionCookieSecure)
	writeJSON(w, http.StatusOK, sessionView{
		Username:  sess.Username,
		ExpiresAt: sess.ExpiresAt.UTC().Format(time.RFC3339),
		CSRFToken: sess.CSRFToken,
	})
}

func formatRetryAfter(d time.Duration) string {
	sec := int(d.Seconds())
	if sec < 1 {
		sec = 1
	}
	return strconv.Itoa(sec)
}

func (h *Handler) handleSession(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.NotFound(w, r)
		return
	}
	if !h.authEnabled() {
		writeError(w, http.StatusNotFound, "admin auth is disabled")
		return
	}
	sess := h.requireSession(w, r)
	if sess == nil {
		return
	}
	writeJSON(w, http.StatusOK, sessionView{
		Username:  sess.Username,
		ExpiresAt: sess.ExpiresAt.UTC().Format(time.RFC3339),
		CSRFToken: sess.CSRFToken,
	})
}

func (h *Handler) handleLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}
	if !h.authEnabled() {
		writeError(w, http.StatusNotFound, "admin auth is disabled")
		return
	}
	sess := h.requireSession(w, r)
	if sess == nil {
		return
	}
	if !h.requireCSRF(w, r, sess) {
		return
	}
	h.auth.delete(sess.ID)
	clearSessionCookie(w, h.adminBasePath(), h.auth.snapshot().SessionCookieSecure)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
}

// loginPageHTML 是最小登录页;basePath 以安全 JSON 字面量注入。
func loginPageHTML(basePath string) []byte {
	bp, _ := json.Marshal(basePath)
	const tpl = `<!doctype html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<meta name="referrer" content="no-referrer">
<title>AI Proxy · 登录</title>
<style>
:root{--primary:#1677ff;--border:#d9d9d9;--bg:#f5f7fa;--text:#1f1f1f;--muted:#8c8c8c;--danger:#ff4d4f}
*{box-sizing:border-box}body{margin:0;min-height:100vh;display:grid;place-items:center;background:var(--bg);color:var(--text);font:14px/1.5 -apple-system,BlinkMacSystemFont,"Segoe UI","PingFang SC","Microsoft YaHei",sans-serif}
.card{width:min(400px,calc(100vw - 32px));background:#fff;border:1px solid #f0f0f0;border-radius:10px;padding:28px 24px;box-shadow:0 6px 16px rgba(0,0,0,.06)}
h1{margin:0 0 6px;font-size:20px}p{margin:0 0 20px;color:var(--muted)}.field{margin-bottom:14px}label{display:block;margin-bottom:6px}
input{width:100%;height:36px;padding:6px 11px;border:1px solid var(--border);border-radius:6px;outline:none}input:focus{border-color:var(--primary)}
button{width:100%;height:36px;border:0;border-radius:6px;background:var(--primary);color:#fff;cursor:pointer;margin-top:6px}button:disabled{opacity:.55;cursor:not-allowed}
.err{min-height:20px;color:var(--danger);margin-top:10px;font-size:13px}
</style>
</head>
<body>
<div class="card">
  <h1>AI Proxy 管理登录</h1>
  <p>请输入管理员账号与密码</p>
  <form id="f">
    <div class="field"><label for="u">用户名</label><input id="u" name="username" autocomplete="username" required></div>
    <div class="field"><label for="p">密码</label><input id="p" name="password" type="password" autocomplete="current-password" required></div>
    <button id="btn" type="submit">登录</button>
    <div class="err" id="err"></div>
  </form>
</div>
<script>
window.__AI_PROXY_ADMIN_BASE_PATH__=BASE_PATH_JSON;
const base=window.__AI_PROXY_ADMIN_BASE_PATH__||"/admin";
const errEl=document.getElementById("err");
const btn=document.getElementById("btn");
let retryTimer=null;
function showErr(msg){errEl.textContent=msg||""}
document.getElementById("f").onsubmit=async(e)=>{
  e.preventDefault();showErr("");btn.disabled=true;
  try{
    const r=await fetch(base+"/api/auth/login",{method:"POST",credentials:"same-origin",headers:{"Content-Type":"application/json"},body:JSON.stringify({username:document.getElementById("u").value,password:document.getElementById("p").value}),cache:"no-store"});
    const data=await r.json().catch(()=>({}));
    if(r.status===429){
      const ra=Number(r.headers.get("Retry-After")||"0");
      let left=Number.isFinite(ra)&&ra>0?Math.ceil(ra):900;
      showErr("尝试过多，请 "+left+" 秒后重试");
      if(retryTimer)clearInterval(retryTimer);
      retryTimer=setInterval(()=>{left--;if(left<=0){clearInterval(retryTimer);showErr("");btn.disabled=false}else{showErr("尝试过多，请 "+left+" 秒后重试")}},1000);
      return;
    }
    if(!r.ok){showErr("用户名或密码错误");btn.disabled=false;return}
    location.replace(base+"/");
  }catch(err){showErr("网络错误，请稍后重试");btn.disabled=false}
};
</script>
</body>
</html>`
	return []byte(strings.Replace(tpl, "BASE_PATH_JSON", string(bp), 1))
}

func (h *Handler) serveLoginPage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.NotFound(w, r)
		return
	}
	if !h.authEnabled() {
		http.NotFound(w, r)
		return
	}
	if sess := h.sessionFromRequest(r); sess != nil {
		http.Redirect(w, r, h.adminBasePath()+"/", http.StatusSeeOther)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'unsafe-inline'; script-src 'unsafe-inline'; base-uri 'none'; frame-ancestors 'none'")
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(loginPageHTML(h.adminBasePath()))
}
