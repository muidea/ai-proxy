package admin

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"ai-proxy/internal/pkg/aiproxyconfig"
	"ai-proxy/internal/pkg/aiproxymetricsport"
	"ai-proxy/internal/pkg/aiproxyusage"
	"ai-proxy/internal/services/probe"
	adminweb "ai-proxy/web"

	"go.yaml.in/yaml/v4"
)

const maxRequestBodyBytes = 1 << 20

// RuntimeConfig 由代理处理器实现，用于读取和原子切换当前运行配置。
type RuntimeConfig interface {
	ConfigSnapshot() config.Config
	UpdateConfig(config.Config) error
}

type Handler struct {
	configPath      string
	runtime         RuntimeConfig
	usageStore      usage.Store
	metricsRegistry metricsport.Port
	auth            *authState
	updateMu        sync.Mutex
}

type providerView struct {
	Name                 string               `json:"name"`
	Protocol             string               `json:"protocol"`
	BaseURL              string               `json:"base_url"`
	Models               []string             `json:"models"`
	EndpointCapabilities []string             `json:"endpoint_capabilities"`
	AllowUnauthenticated bool                 `json:"allow_unauthenticated"`
	Enabled              bool                 `json:"enabled"`
	APIKeyConfigured     bool                 `json:"api_key_configured"`
	Availability         providerAvailability `json:"availability"`
}

type providerAvailability struct {
	Status              string `json:"status"`
	Successes           int64  `json:"successes"`
	Failures            int64  `json:"failures"`
	ConsecutiveFailures int64  `json:"consecutive_failures"`
	LastSuccessAt       string `json:"last_success_at,omitempty"`
	LastFailureAt       string `json:"last_failure_at,omitempty"`
	LastStatus          int    `json:"last_status,omitempty"`
	LastOutcome         string `json:"last_outcome,omitempty"`
}

type providerInput struct {
	Name                 string   `json:"name"`
	Protocol             string   `json:"protocol"`
	BaseURL              string   `json:"base_url"`
	APIKey               string   `json:"api_key"`
	ClearAPIKey          bool     `json:"clear_api_key"`
	Models               []string `json:"models"`
	EndpointCapabilities []string `json:"endpoint_capabilities"`
	AllowUnauthenticated bool     `json:"allow_unauthenticated"`
	Enabled              bool     `json:"enabled"`
}

type updateRequest struct {
	Providers []providerInput `json:"providers"`
}

func NewHandler(configPath string, runtime RuntimeConfig) *Handler {
	h := &Handler{configPath: configPath, runtime: runtime}
	if runtime != nil {
		h.auth = newAuthState(runtime.ConfigSnapshot().AdminAuth)
	} else {
		h.auth = newAuthState(config.AdminAuthConfig{BasePath: config.DefaultAdminBasePath, SessionTTLSeconds: config.DefaultAdminSessionTTLSeconds})
	}
	return h
}

// WithMetrics 挂接 usage 查询的健康与错误观测；nil-safe，便于单测复用。
func (h *Handler) WithMetrics(source any) *Handler {
	h.metricsRegistry = metricsport.AsPort(source)
	return h
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	base := h.adminBasePath()
	path := r.URL.Path
	if path != base && !strings.HasPrefix(path, base+"/") {
		http.NotFound(w, r)
		return
	}
	rel := "/"
	if path != base {
		rel = strings.TrimPrefix(path, base)
		if rel == "" {
			rel = "/"
		}
	}

	authOn := h.authEnabled()
	if !authOn {
		if !isLoopbackRequest(r) {
			http.Error(w, "admin access is loopback-only", http.StatusForbidden)
			return
		}
	}

	// 认证端点与登录页(开启认证时无需会话)。
	switch {
	case rel == "/login" && (r.Method == http.MethodGet || r.Method == http.MethodHead):
		h.serveLoginPage(w, r)
		return
	case rel == "/api/auth/login" && r.Method == http.MethodPost:
		h.handleLogin(w, r)
		return
	case rel == "/api/auth/session" && r.Method == http.MethodGet:
		h.handleSession(w, r)
		return
	case rel == "/api/auth/logout" && r.Method == http.MethodPost:
		h.handleLogout(w, r)
		return
	}

	// 认证开启时,其余路径需要会话。
	if authOn {
		isAPI := strings.HasPrefix(rel, "/api/")
		isPage := rel == "/" || rel == ""
		if isPage && (r.Method == http.MethodGet || r.Method == http.MethodHead) {
			if h.sessionFromRequest(r) == nil {
				if r.Method == http.MethodHead {
					writeAdminAuthError(w, http.StatusUnauthorized, "admin_authentication_required", "admin login is required")
					return
				}
				http.Redirect(w, r, base+"/login", http.StatusSeeOther)
				return
			}
		} else if h.sessionFromRequest(r) == nil {
			if isAPI || strings.HasPrefix(rel, "/api") {
				writeAdminAuthError(w, http.StatusUnauthorized, "admin_authentication_required", "admin login is required")
				return
			}
			http.Redirect(w, r, base+"/login", http.StatusSeeOther)
			return
		}
	}

	switch {
	case (rel == "/" || rel == "") && (r.Method == http.MethodGet || r.Method == http.MethodHead):
		h.serveIndex(w, r)
	case rel == "/api/providers" && r.Method == http.MethodGet:
		h.listProviders(w)
	case rel == "/api/providers" && r.Method == http.MethodPut:
		if !h.requireAdminMutation(w, r) {
			return
		}
		h.updateProviders(w, r)
	case strings.HasPrefix(rel, "/api/providers/") && strings.HasSuffix(rel, "/probe") && r.Method == http.MethodPost:
		if !h.requireAdminMutation(w, r) {
			return
		}
		h.probeProvider(w, r, rel)
	case rel == "/api/client-api-keys" && r.Method == http.MethodGet:
		h.listClientAPIKeys(w)
	case rel == "/api/client-api-keys" && r.Method == http.MethodPost:
		h.createClientAPIKey(w, r)
	case strings.HasPrefix(rel, "/api/client-api-keys/"):
		h.clientAPIKeyAction(w, r, rel)
	case strings.HasPrefix(rel, "/api/usage/"):
		h.usageAPI(w, r, rel)
	default:
		http.NotFound(w, r)
	}
}

func (h *Handler) probeProvider(w http.ResponseWriter, r *http.Request, rel string) {
	name := strings.TrimSuffix(strings.TrimPrefix(rel, "/api/providers/"), "/probe")
	name = strings.ToLower(strings.Trim(strings.TrimSpace(name), "/"))
	if name == "" {
		writeError(w, http.StatusBadRequest, "provider name is required")
		return
	}
	result, err := probe.Check(r.Context(), h.runtime.ConfigSnapshot(), name)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if h.metricsRegistry != nil {
		h.metricsRegistry.RecordRequestPlan(name, result.Model, result.Capability, result.Status, time.Duration(result.DurationMS)*time.Millisecond, mapProbeOutcome(result.Conclusion), "", result.Protocol, result.UpstreamPath, "probe")
	}
	writeJSON(w, http.StatusOK, map[string]any{"provider": name, "status": result.Status, "duration_ms": result.DurationMS, "conclusion": result.Conclusion, "summary": result.Summary})
}

func mapProbeOutcome(conclusion string) string {
	if conclusion == "success" {
		return "success"
	}
	if conclusion == "capability_drift" {
		return "capability_drift"
	}
	return "upstream_failed"
}

func (h *Handler) serveIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'unsafe-inline'; script-src 'unsafe-inline'; base-uri 'none'; frame-ancestors 'none'")
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(injectAdminBasePath(adminweb.AdminIndexHTML, h.adminBasePath()))
}

// injectAdminBasePath 在 HTML 开头注入安全的 basePath JSON 字面量。
func injectAdminBasePath(html []byte, basePath string) []byte {
	bp, err := json.Marshal(basePath)
	if err != nil {
		bp = []byte(`"/admin"`)
	}
	injection := []byte("<script>window.__AI_PROXY_ADMIN_BASE_PATH__=" + string(bp) + ";</script>")
	// 插在 <head> 后,保证脚本尽早可用。
	lower := strings.ToLower(string(html))
	idx := strings.Index(lower, "<head>")
	if idx < 0 {
		return append(injection, html...)
	}
	idx += len("<head>")
	out := make([]byte, 0, len(html)+len(injection))
	out = append(out, html[:idx]...)
	out = append(out, injection...)
	out = append(out, html[idx:]...)
	return out
}

func (h *Handler) listProviders(w http.ResponseWriter) {
	cfg := h.runtime.ConfigSnapshot()
	health := h.providerHealth(cfg)
	names := make([]string, 0, len(cfg.Providers))
	for name := range cfg.Providers {
		names = append(names, name)
	}
	sort.Strings(names)

	providers := make([]providerView, 0, len(names))
	for _, name := range names {
		provider := cfg.Providers[name]
		providers = append(providers, providerView{
			Name:                 name,
			Protocol:             provider.Protocol,
			BaseURL:              provider.BaseURL,
			Models:               append([]string(nil), provider.Models...),
			EndpointCapabilities: append([]string(nil), provider.EndpointCapabilities...),
			AllowUnauthenticated: provider.AllowUnauthenticated,
			Enabled:              !provider.Disabled,
			APIKeyConfigured:     strings.TrimSpace(provider.APIKey) != "",
			Availability:         health[name],
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"providers":  providers,
		"writable":   strings.TrimSpace(h.configPath) != "",
		"hot_reload": true,
	})
}

func (h *Handler) providerHealth(cfg config.Config) map[string]providerAvailability {
	result := map[string]providerAvailability{}
	for name, provider := range cfg.Providers {
		status := "unknown"
		if provider.Disabled {
			status = "disabled"
		}
		result[name] = providerAvailability{Status: status}
	}
	if h.metricsRegistry == nil {
		return result
	}
	data, err := h.metricsRegistry.StatsJSON()
	if err != nil {
		return result
	}
	var snapshot struct {
		ProviderHealth map[string]struct {
			Successes           int64  `json:"successes"`
			Failures            int64  `json:"failures"`
			ConsecutiveFailures int64  `json:"consecutive_failures"`
			LastSuccessAt       string `json:"last_success_at"`
			LastFailureAt       string `json:"last_failure_at"`
			LastStatus          int    `json:"last_status"`
			LastOutcome         string `json:"last_outcome"`
		} `json:"provider_health"`
	}
	if json.Unmarshal(data, &snapshot) != nil {
		return result
	}
	for name, value := range snapshot.ProviderHealth {
		status := "unknown"
		switch {
		case value.LastOutcome == "capability_drift":
			status = "capability_drift"
		case value.LastStatus == 401 || value.LastStatus == 403:
			status = "credential_error"
		case value.ConsecutiveFailures >= 3:
			status = "unavailable"
		case value.Failures > 0:
			status = "degraded"
		case value.Successes > 0:
			status = "healthy"
		}
		result[name] = providerAvailability{Status: status, Successes: value.Successes, Failures: value.Failures, ConsecutiveFailures: value.ConsecutiveFailures, LastSuccessAt: value.LastSuccessAt, LastFailureAt: value.LastFailureAt, LastStatus: value.LastStatus, LastOutcome: value.LastOutcome}
	}
	for name, provider := range cfg.Providers {
		if provider.Disabled {
			result[name] = providerAvailability{Status: "disabled"}
		}
	}
	return result
}

func (h *Handler) updateProviders(w http.ResponseWriter, r *http.Request) {
	h.updateMu.Lock()
	defer h.updateMu.Unlock()

	if strings.TrimSpace(h.configPath) == "" {
		writeError(w, http.StatusConflict, "no writable config file is active")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	var request updateRequest
	if err := dec.Decode(&request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request: "+err.Error())
		return
	}
	if err := ensureJSONEOF(dec); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request: "+err.Error())
		return
	}
	if len(request.Providers) == 0 {
		writeError(w, http.StatusBadRequest, "at least one provider is required")
		return
	}

	cfg, err := writeProviders(h.configPath, h.adminBasePath(), request.Providers)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := h.activateConfig(cfg); err != nil {
		writeError(w, http.StatusInternalServerError, "activate config: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "message": "provider configuration saved and activated"})
}

func writeProviders(path, expectedAdminBasePath string, providers []providerInput) (config.Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return config.Config{}, fmt.Errorf("read config: %w", err)
	}
	var document yaml.Node
	if err := yaml.Unmarshal(data, &document); err != nil {
		return config.Config{}, fmt.Errorf("parse config: %w", err)
	}
	root, err := documentRoot(&document)
	if err != nil {
		return config.Config{}, err
	}
	existingSecrets := providerSecrets(root)
	providersNode, err := buildProvidersNode(providers, existingSecrets)
	if err != nil {
		return config.Config{}, err
	}
	setMappingValue(root, "providers", providersNode)

	var encoded bytes.Buffer
	encoder := yaml.NewEncoder(&encoded)
	encoder.SetIndent(2)
	if err := encoder.Encode(&document); err != nil {
		return config.Config{}, fmt.Errorf("encode config: %w", err)
	}
	if err := encoder.Close(); err != nil {
		return config.Config{}, fmt.Errorf("close config encoder: %w", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		return config.Config{}, fmt.Errorf("stat config: %w", err)
	}
	dir := filepath.Dir(path)
	temp, err := os.CreateTemp(dir, ".ai-proxy-config-*.yaml")
	if err != nil {
		return config.Config{}, fmt.Errorf("create temporary config: %w", err)
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if err := temp.Chmod(info.Mode().Perm()); err != nil {
		_ = temp.Close()
		return config.Config{}, fmt.Errorf("set temporary config mode: %w", err)
	}
	if _, err := temp.Write(encoded.Bytes()); err != nil {
		_ = temp.Close()
		return config.Config{}, fmt.Errorf("write temporary config: %w", err)
	}
	if err := temp.Close(); err != nil {
		return config.Config{}, fmt.Errorf("close temporary config: %w", err)
	}

	cfg, err := config.Load(tempPath)
	if err != nil {
		return config.Config{}, fmt.Errorf("configuration rejected: %w", err)
	}
	if cfg.AdminAuth.BasePath != expectedAdminBasePath {
		return config.Config{}, errAdminBasePathRestart
	}
	if err := os.Rename(tempPath, path); err != nil {
		return config.Config{}, fmt.Errorf("replace config: %w", err)
	}
	return cfg, nil
}

func documentRoot(document *yaml.Node) (*yaml.Node, error) {
	if document.Kind != yaml.DocumentNode || len(document.Content) != 1 {
		return nil, errors.New("config must contain one YAML document")
	}
	root := document.Content[0]
	if root.Kind != yaml.MappingNode {
		return nil, errors.New("config root must be a mapping")
	}
	return root, nil
}

func providerSecrets(root *yaml.Node) map[string]string {
	providers := mappingValue(root, "providers")
	secrets := map[string]string{}
	if providers == nil || providers.Kind != yaml.MappingNode {
		return secrets
	}
	for i := 0; i+1 < len(providers.Content); i += 2 {
		name := strings.ToLower(strings.TrimSpace(providers.Content[i].Value))
		provider := providers.Content[i+1]
		if secret := mappingValue(provider, "api_key"); secret != nil {
			secrets[name] = secret.Value
		}
	}
	return secrets
}

func buildProvidersNode(inputs []providerInput, existingSecrets map[string]string) (*yaml.Node, error) {
	byName := make(map[string]providerInput, len(inputs))
	for _, input := range inputs {
		name := strings.ToLower(strings.TrimSpace(input.Name))
		if name == "" {
			return nil, errors.New("provider name is required")
		}
		if _, exists := byName[name]; exists {
			return nil, fmt.Errorf("duplicate provider %q", name)
		}
		input.Name = name
		byName[name] = input
	}
	names := make([]string, 0, len(byName))
	for name := range byName {
		names = append(names, name)
	}
	sort.Strings(names)

	node := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	for _, name := range names {
		input := byName[name]
		secret := strings.TrimSpace(input.APIKey)
		if secret == "" && !input.ClearAPIKey {
			secret = existingSecrets[name]
		}
		provider := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		appendScalar(provider, "enabled", fmt.Sprintf("%t", input.Enabled), "!!bool")
		appendScalar(provider, "protocol", strings.ToLower(strings.TrimSpace(input.Protocol)), "!!str")
		appendScalar(provider, "base_url", strings.TrimSpace(input.BaseURL), "!!str")
		appendScalar(provider, "api_key", secret, "!!str")
		appendScalar(provider, "endpoint_capabilities", strings.Join(input.EndpointCapabilities, ", "), "!!str")
		appendScalar(provider, "models", strings.Join(input.Models, ", "), "!!str")
		if input.AllowUnauthenticated {
			appendScalar(provider, "allow_unauthenticated", "true", "!!bool")
		}
		node.Content = append(node.Content, mappingKey(name), provider)
	}
	return node, nil
}

func appendScalar(mapping *yaml.Node, key, value, tag string) {
	mapping.Content = append(mapping.Content, mappingKey(key), scalar(value, tag))
}

func mappingKey(value string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value}
}

func scalar(value, tag string) *yaml.Node {
	node := &yaml.Node{Kind: yaml.ScalarNode, Tag: tag, Value: value}
	if tag == "!!str" {
		node.Style = yaml.DoubleQuotedStyle
	}
	return node
}

func mappingValue(mapping *yaml.Node, key string) *yaml.Node {
	if mapping == nil || mapping.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			return mapping.Content[i+1]
		}
	}
	return nil
}

func setMappingValue(mapping *yaml.Node, key string, value *yaml.Node) {
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			mapping.Content[i+1] = value
			return
		}
	}
	mapping.Content = append(mapping.Content, mappingKey(key), value)
}

func ensureJSONEOF(dec *json.Decoder) error {
	var extra any
	err := dec.Decode(&extra)
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err == nil {
		return errors.New("multiple JSON values")
	}
	return err
}

func isLoopbackRequest(r *http.Request) bool {
	host, _, err := net.SplitHostPort(strings.TrimSpace(r.RemoteAddr))
	if err != nil {
		host = strings.TrimSpace(r.RemoteAddr)
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"message": message}})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
