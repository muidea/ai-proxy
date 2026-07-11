package config

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	ListenAddr           string
	UsageFile            string
	InteractionDir       string
	InteractionRetention int
	DebugLog             bool
	LogFormat            string
	RequestTimeout       time.Duration
	StreamIdleTimeout    time.Duration
	DefaultProvider      string
	MetricsRemoteAccess  bool
	MetricsAllowedCIDRs  []string
	SLO                  SLOConfig
	Providers            map[string]Provider
	// ModelCatalog 是全局模型元数据目录,供 GET /v1/models 使用;与 provider 路由解耦。
	ModelCatalog map[string]ModelInfo
}

// ModelInfo 描述客户端可查询的模型能力(各 provider 共用同一目录)。
type ModelInfo struct {
	ID                  string
	ContextWindowTokens int
	MaxOutputTokens     int
}

// SLOConfig 描述可观测性层面的服务等级目标。
type SLOConfig struct {
	// CacheHitRateMin 是单 provider 缓存命中率的最低要求(0~1)。
	CacheHitRateMin float64
	// UpstreamErrorRateMax 是单 provider 上游错误率上限(0~1)。
	UpstreamErrorRateMax float64
	// P99LatencyMaxMS 是单 provider p99 延迟上限(毫秒)。
	P99LatencyMaxMS float64
	// CheckIntervalSeconds 是后台巡检周期;0 表示禁用周期检查。
	CheckIntervalSeconds int
	// ViolationWebhook 是可选 webhook URL,命中 SLO 时异步 POST。
	ViolationWebhook string
}

type Provider struct {
	Name      string
	Protocol  string
	BaseURL   string
	APIKey    string
	Models    []string
	Fallbacks []string
	Disabled  bool
}

func Load(path string) (Config, error) {
	cfg := Config{
		ListenAddr:           ":8080",
		UsageFile:            "usage.csv",
		InteractionDir:       "interactions",
		InteractionRetention: 500,
		DebugLog:             true,
		LogFormat:            "json",
		RequestTimeout:       5 * time.Minute,
		StreamIdleTimeout:    5 * time.Minute,
		Providers:            map[string]Provider{},
		ModelCatalog:         map[string]ModelInfo{},
	}

	if path == "" {
		if _, err := os.Stat("config.yaml"); err == nil {
			path = "config.yaml"
		}
	}
	if path != "" {
		if err := loadFile(path, &cfg); err != nil {
			return Config{}, err
		}
	}

	applyEnv(&cfg)
	ensureKnownProviders(&cfg)
	normalize(&cfg)
	if len(cfg.Providers) == 0 {
		return Config{}, fmt.Errorf("no providers configured; set config.yaml providers or OPENAI_API_KEY/API_KEY")
	}
	if !hasEnabledProvider(cfg.Providers) {
		return Config{}, fmt.Errorf("no enabled providers configured")
	}
	if err := validateDefaultProvider(cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func loadFile(path string, cfg *Config) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	section := ""
	providerName := ""
	modelName := ""
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		raw := stripComment(scanner.Text())
		if strings.TrimSpace(raw) == "" {
			continue
		}
		indent := countIndent(raw)
		line := strings.TrimSpace(raw)
		key, value, hasValue := splitKV(line)
		if key == "" {
			return fmt.Errorf("%s:%d: invalid config line", path, lineNo)
		}

		switch {
		case indent == 0 && !hasValue:
			section = key
			providerName = ""
			modelName = ""
		case indent == 0:
			section = ""
			providerName = ""
			modelName = ""
			setTopLevel(cfg, key, expand(value))
		case section == "server" && indent >= 2:
			setServer(cfg, key, expand(value))
		case section == "providers" && indent == 2 && !hasValue:
			providerName = key
			modelName = ""
			ensureProvider(cfg, providerName)
		case section == "providers" && indent >= 4 && providerName != "":
			setProvider(cfg, providerName, key, expand(value))
		case section == "model_catalog" && indent == 2 && !hasValue:
			modelName = key
			providerName = ""
			ensureModelInfo(cfg, modelName)
		case section == "model_catalog" && indent >= 4 && modelName != "":
			setModelInfo(cfg, modelName, key, expand(value))
		default:
			return fmt.Errorf("%s:%d: unsupported config shape", path, lineNo)
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return nil
}

func setTopLevel(cfg *Config, key, value string) {
	switch key {
	case "usage_file":
		cfg.UsageFile = value
	case "interaction_dir":
		cfg.InteractionDir = value
	case "interaction_retention":
		cfg.InteractionRetention = parsePositiveInt(value, cfg.InteractionRetention)
	case "debug_log":
		cfg.DebugLog = parseBool(value, cfg.DebugLog)
	case "log_format":
		cfg.LogFormat = value
	case "port":
		cfg.ListenAddr = addrFromPort(value)
	case "listen_addr":
		cfg.ListenAddr = value
	case "request_timeout_seconds":
		if seconds, err := strconv.Atoi(value); err == nil && seconds > 0 {
			cfg.RequestTimeout = time.Duration(seconds) * time.Second
		}
	case "stream_idle_timeout_seconds":
		cfg.StreamIdleTimeout = parseNonNegativeSeconds(value, cfg.StreamIdleTimeout)
	case "default_provider":
		cfg.DefaultProvider = value
	case "metrics_remote_access":
		cfg.MetricsRemoteAccess = parseBool(value, cfg.MetricsRemoteAccess)
	case "metrics_allowed_cidrs":
		cfg.MetricsAllowedCIDRs = parseList(value)
	case "slo_cache_hit_rate_min":
		cfg.SLO.CacheHitRateMin = parseFloat(value, cfg.SLO.CacheHitRateMin)
	case "slo_upstream_error_rate_max":
		cfg.SLO.UpstreamErrorRateMax = parseFloat(value, cfg.SLO.UpstreamErrorRateMax)
	case "slo_p99_latency_max_ms":
		cfg.SLO.P99LatencyMaxMS = parseFloat(value, cfg.SLO.P99LatencyMaxMS)
	case "slo_check_interval_seconds":
		cfg.SLO.CheckIntervalSeconds = parsePositiveInt(value, cfg.SLO.CheckIntervalSeconds)
	case "slo_violation_webhook":
		cfg.SLO.ViolationWebhook = value
	}
}

func setServer(cfg *Config, key, value string) {
	switch key {
	case "port":
		cfg.ListenAddr = addrFromPort(value)
	case "listen_addr":
		cfg.ListenAddr = value
	case "usage_file":
		cfg.UsageFile = value
	case "interaction_dir":
		cfg.InteractionDir = value
	case "interaction_retention":
		cfg.InteractionRetention = parsePositiveInt(value, cfg.InteractionRetention)
	case "debug_log":
		cfg.DebugLog = parseBool(value, cfg.DebugLog)
	case "log_format":
		cfg.LogFormat = value
	case "request_timeout_seconds":
		if seconds, err := strconv.Atoi(value); err == nil && seconds > 0 {
			cfg.RequestTimeout = time.Duration(seconds) * time.Second
		}
	case "stream_idle_timeout_seconds":
		cfg.StreamIdleTimeout = parseNonNegativeSeconds(value, cfg.StreamIdleTimeout)
	case "default_provider":
		cfg.DefaultProvider = value
	case "metrics_remote_access":
		cfg.MetricsRemoteAccess = parseBool(value, cfg.MetricsRemoteAccess)
	case "metrics_allowed_cidrs":
		cfg.MetricsAllowedCIDRs = parseList(value)
	case "slo_cache_hit_rate_min":
		cfg.SLO.CacheHitRateMin = parseFloat(value, cfg.SLO.CacheHitRateMin)
	case "slo_upstream_error_rate_max":
		cfg.SLO.UpstreamErrorRateMax = parseFloat(value, cfg.SLO.UpstreamErrorRateMax)
	case "slo_p99_latency_max_ms":
		cfg.SLO.P99LatencyMaxMS = parseFloat(value, cfg.SLO.P99LatencyMaxMS)
	case "slo_check_interval_seconds":
		cfg.SLO.CheckIntervalSeconds = parsePositiveInt(value, cfg.SLO.CheckIntervalSeconds)
	case "slo_violation_webhook":
		cfg.SLO.ViolationWebhook = value
	}
}

func setProvider(cfg *Config, name, key, value string) {
	provider := ensureProvider(cfg, name)
	switch key {
	case "base_url":
		provider.BaseURL = value
	case "api_key":
		provider.APIKey = value
	case "type", "protocol":
		provider.Protocol = strings.ToLower(value)
	case "models", "model_patterns":
		provider.Models = parseList(value)
	case "fallbacks", "fallback_providers":
		provider.Fallbacks = parseList(value)
	case "enabled":
		provider.Disabled = !parseBool(value, true)
	}
	cfg.Providers[name] = provider
}

func ensureProvider(cfg *Config, name string) Provider {
	provider, ok := cfg.Providers[name]
	if !ok {
		provider = Provider{Name: name}
	}
	cfg.Providers[name] = provider
	return provider
}

func ensureModelInfo(cfg *Config, id string) ModelInfo {
	if cfg.ModelCatalog == nil {
		cfg.ModelCatalog = map[string]ModelInfo{}
	}
	info, ok := cfg.ModelCatalog[id]
	if !ok {
		info = ModelInfo{ID: id}
	}
	if info.ID == "" {
		info.ID = id
	}
	cfg.ModelCatalog[id] = info
	return info
}

func setModelInfo(cfg *Config, id, key, value string) {
	info := ensureModelInfo(cfg, id)
	switch strings.ToLower(key) {
	case "context_window_tokens", "contextwindowtokens", "context_window":
		info.ContextWindowTokens = parseNonNegativeInt(value, info.ContextWindowTokens)
	case "max_output_tokens", "maxoutputtokens":
		info.MaxOutputTokens = parseNonNegativeInt(value, info.MaxOutputTokens)
	}
	cfg.ModelCatalog[id] = info
}

func applyEnv(cfg *Config) {
	if value := os.Getenv("AI_PROXY_LISTEN_ADDR"); value != "" {
		cfg.ListenAddr = value
	}
	if value := os.Getenv("AI_PROXY_PORT"); value != "" {
		cfg.ListenAddr = addrFromPort(value)
	}
	if value := os.Getenv("AI_PROXY_USAGE_FILE"); value != "" {
		cfg.UsageFile = value
	}
	if value := os.Getenv("AI_PROXY_INTERACTION_DIR"); value != "" {
		cfg.InteractionDir = value
	}
	if value := os.Getenv("AI_PROXY_INTERACTION_RETENTION"); value != "" {
		cfg.InteractionRetention = parsePositiveInt(value, cfg.InteractionRetention)
	}
	if value := os.Getenv("AI_PROXY_DEBUG_LOG"); value != "" {
		cfg.DebugLog = parseBool(value, cfg.DebugLog)
	}
	if value := firstEnv("AI_PROXY_LOG_FORMAT", "LOG_FORMAT"); value != "" {
		cfg.LogFormat = value
	}
	if value := os.Getenv("AI_PROXY_REQUEST_TIMEOUT_SECONDS"); value != "" {
		if seconds, err := strconv.Atoi(value); err == nil && seconds > 0 {
			cfg.RequestTimeout = time.Duration(seconds) * time.Second
		}
	}
	if value := os.Getenv("AI_PROXY_STREAM_IDLE_TIMEOUT_SECONDS"); value != "" {
		cfg.StreamIdleTimeout = parseNonNegativeSeconds(value, cfg.StreamIdleTimeout)
	}
	if value := os.Getenv("AI_PROXY_DEFAULT_PROVIDER"); value != "" {
		cfg.DefaultProvider = value
	}
	if value := os.Getenv("AI_PROXY_METRICS_REMOTE_ACCESS"); value != "" {
		cfg.MetricsRemoteAccess = parseBool(value, cfg.MetricsRemoteAccess)
	}
	if value := os.Getenv("AI_PROXY_METRICS_ALLOWED_CIDRS"); value != "" {
		cfg.MetricsAllowedCIDRs = parseList(value)
	}

	applyProviderEnv(cfg, "openai", "https://api.openai.com")
	applyProviderEnv(cfg, "deepseek", "https://api.deepseek.com")
	applyProviderEnv(cfg, "anthropic", "https://api.anthropic.com")

	if key := os.Getenv("API_KEY"); key != "" {
		provider := ensureProvider(cfg, "custom")
		provider.APIKey = key
		if base := os.Getenv("API_BASE_URL"); base != "" {
			provider.BaseURL = base
		}
		cfg.Providers["custom"] = provider
	}
}

func applyProviderEnv(cfg *Config, name, fallbackBaseURL string) {
	envPrefix := strings.ToUpper(strings.ReplaceAll(name, "-", "_"))
	key := firstEnv("AI_PROXY_"+envPrefix+"_API_KEY", envPrefix+"_API_KEY")
	baseURL := firstEnv("AI_PROXY_"+envPrefix+"_BASE_URL", envPrefix+"_BASE_URL")
	models := firstEnv("AI_PROXY_"+envPrefix+"_MODELS", envPrefix+"_MODELS")
	fallbacks := firstEnv("AI_PROXY_"+envPrefix+"_FALLBACKS", envPrefix+"_FALLBACKS")
	enabled := firstEnv("AI_PROXY_"+envPrefix+"_ENABLED", envPrefix+"_ENABLED")
	if key == "" && baseURL == "" && models == "" && fallbacks == "" && enabled == "" {
		return
	}
	provider := ensureProvider(cfg, name)
	if key != "" {
		provider.APIKey = key
	}
	if baseURL != "" {
		provider.BaseURL = baseURL
	} else if provider.BaseURL == "" {
		provider.BaseURL = fallbackBaseURL
	}
	if provider.Protocol == "" && name == "anthropic" {
		provider.Protocol = "anthropic"
	}
	if models != "" {
		provider.Models = parseList(models)
	}
	if fallbacks != "" {
		provider.Fallbacks = parseList(fallbacks)
	}
	if enabled != "" {
		provider.Disabled = !parseBool(enabled, true)
	}
	cfg.Providers[name] = provider
}

func ensureKnownProviders(cfg *Config) {
	defaults := map[string]string{
		"openai":    "https://api.openai.com",
		"deepseek":  "https://api.deepseek.com",
		"anthropic": "https://api.anthropic.com",
	}
	for name, provider := range cfg.Providers {
		if provider.Name == "" {
			provider.Name = name
		}
		if provider.Protocol == "" {
			if name == "anthropic" {
				provider.Protocol = "anthropic"
			} else {
				provider.Protocol = "openai"
			}
		}
		if provider.BaseURL == "" {
			provider.BaseURL = defaults[name]
		}
		cfg.Providers[name] = provider
	}
}

func normalize(cfg *Config) {
	cfg.LogFormat = normalizeLogFormat(cfg.LogFormat)
	cfg.DefaultProvider = strings.ToLower(strings.TrimSpace(cfg.DefaultProvider))
	normalized := make(map[string]Provider, len(cfg.Providers))
	for name, provider := range cfg.Providers {
		key := strings.ToLower(name)
		provider.Name = key
		if provider.Protocol == "" {
			provider.Protocol = "openai"
		}
		provider.Protocol = strings.ToLower(provider.Protocol)
		provider.BaseURL = strings.TrimRight(provider.BaseURL, "/")
		provider.Models = normalizeList(provider.Models)
		provider.Fallbacks = normalizeList(provider.Fallbacks)
		normalized[key] = provider
	}
	cfg.Providers = normalized

	if cfg.ModelCatalog == nil {
		cfg.ModelCatalog = map[string]ModelInfo{}
	}
	catalog := make(map[string]ModelInfo, len(cfg.ModelCatalog))
	for name, info := range cfg.ModelCatalog {
		id := strings.TrimSpace(info.ID)
		if id == "" {
			id = strings.TrimSpace(name)
		}
		if id == "" {
			continue
		}
		info.ID = id
		if info.ContextWindowTokens < 0 {
			info.ContextWindowTokens = 0
		}
		if info.MaxOutputTokens < 0 {
			info.MaxOutputTokens = 0
		}
		// 查找键小写;展示 ID 保留配置原文。
		catalog[strings.ToLower(id)] = info
	}
	cfg.ModelCatalog = catalog
}

func normalizeLogFormat(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "text":
		return "text"
	default:
		return "json"
	}
}

func hasEnabledProvider(providers map[string]Provider) bool {
	for _, provider := range providers {
		if !provider.Disabled {
			return true
		}
	}
	return false
}

func validateDefaultProvider(cfg Config) error {
	if cfg.DefaultProvider == "" {
		return nil
	}
	provider, ok := cfg.Providers[cfg.DefaultProvider]
	if !ok {
		return fmt.Errorf("default_provider %q is not configured", cfg.DefaultProvider)
	}
	if provider.Disabled {
		return fmt.Errorf("default_provider %q is disabled", cfg.DefaultProvider)
	}
	return nil
}

func stripComment(line string) string {
	inQuote := rune(0)
	for i, r := range line {
		switch r {
		case '\'', '"':
			if inQuote == 0 {
				inQuote = r
			} else if inQuote == r {
				inQuote = 0
			}
		case '#':
			if inQuote == 0 {
				return line[:i]
			}
		}
	}
	return line
}

func countIndent(line string) int {
	count := 0
	for _, r := range line {
		if r != ' ' {
			return count
		}
		count++
	}
	return count
}

func splitKV(line string) (string, string, bool) {
	idx := strings.IndexRune(line, ':')
	if idx < 0 {
		return "", "", false
	}
	key := strings.TrimSpace(line[:idx])
	value := strings.TrimSpace(line[idx+1:])
	if value == "" {
		return key, "", false
	}
	return key, unquote(value), true
}

func unquote(value string) string {
	if len(value) >= 2 {
		if (value[0] == '"' && value[len(value)-1] == '"') || (value[0] == '\'' && value[len(value)-1] == '\'') {
			return value[1 : len(value)-1]
		}
	}
	return value
}

func expand(value string) string {
	return os.ExpandEnv(value)
}

func addrFromPort(port string) string {
	port = strings.TrimSpace(port)
	if strings.HasPrefix(port, ":") {
		return port
	}
	return ":" + port
}

func firstEnv(names ...string) string {
	for _, name := range names {
		if value := os.Getenv(name); value != "" {
			return value
		}
	}
	return ""
}

func parseBool(value string, fallback bool) bool {
	parsed, err := strconv.ParseBool(strings.TrimSpace(value))
	if err != nil {
		return fallback
	}
	return parsed
}

func parsePositiveInt(value string, fallback int) int {
	parsed, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || parsed <= 0 {
		return fallback
	}
	return parsed
}

func parseNonNegativeInt(value string, fallback int) int {
	parsed, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || parsed < 0 {
		return fallback
	}
	return parsed
}

func parseFloat(value string, fallback float64) float64 {
	parsed, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
	if err != nil {
		return fallback
	}
	return parsed
}

func parseNonNegativeSeconds(value string, fallback time.Duration) time.Duration {
	parsed, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || parsed < 0 {
		return fallback
	}
	return time.Duration(parsed) * time.Second
}

func parseList(value string) []string {
	value = strings.TrimSpace(value)
	value = strings.TrimPrefix(value, "[")
	value = strings.TrimSuffix(value, "]")
	if value == "" {
		return nil
	}
	parts := strings.Split(value, ",")
	items := make([]string, 0, len(parts))
	for _, part := range parts {
		item := strings.ToLower(strings.TrimSpace(unquote(part)))
		if item != "" {
			items = append(items, item)
		}
	}
	return items
}

func normalizeList(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	normalized := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.ToLower(strings.TrimSpace(value))
		if value != "" {
			normalized = append(normalized, value)
		}
	}
	return normalized
}
