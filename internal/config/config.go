package config

import (
	"bufio"
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// 默认体大小上限:入站 32MiB,上游响应 64MiB。
// 流式:累计输出默认 64MiB,单条 SSE 行默认 1MiB。
const (
	DefaultMaxRequestBodyBytes      int64 = 32 << 20
	DefaultMaxUpstreamResponseBytes int64 = 64 << 20
	DefaultMaxStreamBytes           int64 = 64 << 20
	DefaultMaxSSELineBytes          int64 = 1 << 20
)

type Config struct {
	ListenAddr string
	// InboundAPIKey 是代理入站认证密钥。非 loopback 监听时必须配置;
	// 客户端通过 Authorization: Bearer <key> 或 X-API-Key 提交。
	InboundAPIKey string
	// MaxRequestBodyBytes 限制入站请求体大小;<=0 时使用默认值。
	MaxRequestBodyBytes int64
	// MaxUpstreamResponseBytes 限制上游非流式响应读取上限;<=0 时使用默认值。
	MaxUpstreamResponseBytes int64
	// MaxStreamBytes 限制单次流式响应累计转发/累积字节;<=0 时使用默认值。
	MaxStreamBytes int64
	// MaxSSELineBytes 限制单条 SSE 行(到 \n)最大字节;<=0 时使用默认值。
	MaxSSELineBytes int64
	// ArchiveFullContent 为 false 时仅写元数据,不落盘完整请求/响应正文。
	ArchiveFullContent   bool
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
	// ViolationWebhook 是可选 webhook URL,命中 SLO 时异步 POST JSON(短超时/有限并发)。
	// 尽力而为:不与 shutdown 协同;进程退出时在途告警可能丢失。日志仅记录 scheme://host。
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
		ListenAddr:               "127.0.0.1:8080",
		MaxRequestBodyBytes:      DefaultMaxRequestBodyBytes,
		MaxUpstreamResponseBytes: DefaultMaxUpstreamResponseBytes,
		MaxStreamBytes:           DefaultMaxStreamBytes,
		MaxSSELineBytes:          DefaultMaxSSELineBytes,
		ArchiveFullContent:       true,
		UsageFile:                "usage.csv",
		InteractionDir:           "interactions",
		InteractionRetention:     500,
		DebugLog:                 true,
		LogFormat:                "json",
		RequestTimeout:           5 * time.Minute,
		StreamIdleTimeout:        5 * time.Minute,
		Providers:                map[string]Provider{},
		ModelCatalog:             map[string]ModelInfo{},
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

	if err := applyEnv(&cfg); err != nil {
		return Config{}, err
	}
	ensureKnownProviders(&cfg)
	if err := normalize(&cfg); err != nil {
		return Config{}, err
	}
	if err := validate(cfg); err != nil {
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

		var setErr error
		switch {
		case indent == 0 && !hasValue:
			switch key {
			case "server", "providers", "model_catalog":
				section = key
				providerName = ""
				modelName = ""
			default:
				return fmt.Errorf("%s:%d: unknown section %q", path, lineNo, key)
			}
		case indent == 0:
			section = ""
			providerName = ""
			modelName = ""
			setErr = setTopLevel(cfg, key, expand(value))
		case section == "server" && indent >= 2:
			setErr = setServer(cfg, key, expand(value))
		case section == "providers" && indent == 2 && !hasValue:
			providerName = key
			modelName = ""
			ensureProvider(cfg, providerName)
		case section == "providers" && indent >= 4 && providerName != "":
			setErr = setProvider(cfg, providerName, key, expand(value))
		case section == "model_catalog" && indent == 2 && !hasValue:
			modelName = key
			providerName = ""
			ensureModelInfo(cfg, modelName)
		case section == "model_catalog" && indent >= 4 && modelName != "":
			setErr = setModelInfo(cfg, modelName, key, expand(value))
		default:
			return fmt.Errorf("%s:%d: unsupported config shape", path, lineNo)
		}
		if setErr != nil {
			return fmt.Errorf("%s:%d: %w", path, lineNo, setErr)
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return nil
}

func setTopLevel(cfg *Config, key, value string) error {
	switch key {
	case "usage_file":
		cfg.UsageFile = value
	case "interaction_dir":
		cfg.InteractionDir = value
	case "interaction_retention":
		n, err := parseStrictPositiveInt(value)
		if err != nil {
			return fmt.Errorf("interaction_retention: %w", err)
		}
		cfg.InteractionRetention = n
	case "debug_log":
		b, err := parseStrictBool(value)
		if err != nil {
			return fmt.Errorf("debug_log: %w", err)
		}
		cfg.DebugLog = b
	case "log_format":
		cfg.LogFormat = value
	case "port":
		cfg.ListenAddr = addrFromPort(value)
	case "listen_addr":
		cfg.ListenAddr = value
	case "inbound_api_key":
		cfg.InboundAPIKey = value
	case "max_request_body_bytes":
		n, err := parseStrictPositiveInt64(value)
		if err != nil {
			return fmt.Errorf("max_request_body_bytes: %w", err)
		}
		cfg.MaxRequestBodyBytes = n
	case "max_upstream_response_bytes":
		n, err := parseStrictPositiveInt64(value)
		if err != nil {
			return fmt.Errorf("max_upstream_response_bytes: %w", err)
		}
		cfg.MaxUpstreamResponseBytes = n
	case "max_stream_bytes":
		n, err := parseStrictPositiveInt64(value)
		if err != nil {
			return fmt.Errorf("max_stream_bytes: %w", err)
		}
		cfg.MaxStreamBytes = n
	case "max_sse_line_bytes":
		n, err := parseStrictPositiveInt64(value)
		if err != nil {
			return fmt.Errorf("max_sse_line_bytes: %w", err)
		}
		cfg.MaxSSELineBytes = n
	case "archive_full_content":
		b, err := parseStrictBool(value)
		if err != nil {
			return fmt.Errorf("archive_full_content: %w", err)
		}
		cfg.ArchiveFullContent = b
	case "request_timeout_seconds":
		n, err := parseStrictPositiveInt(value)
		if err != nil {
			return fmt.Errorf("request_timeout_seconds: %w", err)
		}
		cfg.RequestTimeout = time.Duration(n) * time.Second
	case "stream_idle_timeout_seconds":
		n, err := parseStrictNonNegativeInt(value)
		if err != nil {
			return fmt.Errorf("stream_idle_timeout_seconds: %w", err)
		}
		cfg.StreamIdleTimeout = time.Duration(n) * time.Second
	case "default_provider":
		cfg.DefaultProvider = value
	case "metrics_remote_access":
		b, err := parseStrictBool(value)
		if err != nil {
			return fmt.Errorf("metrics_remote_access: %w", err)
		}
		cfg.MetricsRemoteAccess = b
	case "metrics_allowed_cidrs":
		cfg.MetricsAllowedCIDRs = parseList(value)
	case "slo_cache_hit_rate_min":
		f, err := parseStrictFloat(value)
		if err != nil {
			return fmt.Errorf("slo_cache_hit_rate_min: %w", err)
		}
		cfg.SLO.CacheHitRateMin = f
	case "slo_upstream_error_rate_max":
		f, err := parseStrictFloat(value)
		if err != nil {
			return fmt.Errorf("slo_upstream_error_rate_max: %w", err)
		}
		cfg.SLO.UpstreamErrorRateMax = f
	case "slo_p99_latency_max_ms":
		f, err := parseStrictFloat(value)
		if err != nil {
			return fmt.Errorf("slo_p99_latency_max_ms: %w", err)
		}
		cfg.SLO.P99LatencyMaxMS = f
	case "slo_check_interval_seconds":
		n, err := parseStrictNonNegativeInt(value)
		if err != nil {
			return fmt.Errorf("slo_check_interval_seconds: %w", err)
		}
		cfg.SLO.CheckIntervalSeconds = n
	case "slo_violation_webhook":
		cfg.SLO.ViolationWebhook = value
	default:
		return fmt.Errorf("unknown config key %q", key)
	}
	return nil
}

func setServer(cfg *Config, key, value string) error {
	// server 段与顶层键共享同一套字段。
	return setTopLevel(cfg, key, value)
}

func setProvider(cfg *Config, name, key, value string) error {
	provider := ensureProvider(cfg, name)
	switch key {
	case "base_url":
		provider.BaseURL = value
	case "api_key":
		provider.APIKey = value
	case "type", "protocol":
		provider.Protocol = strings.ToLower(value)
	case "models", "model_patterns":
		// models 严格区分大小写,与请求 body.model 原文匹配。
		provider.Models = parseModelList(value)
	case "fallbacks", "fallback_providers":
		provider.Fallbacks = parseList(value)
	case "enabled":
		b, err := parseStrictBool(value)
		if err != nil {
			return fmt.Errorf("providers.%s.enabled: %w", name, err)
		}
		provider.Disabled = !b
	default:
		return fmt.Errorf("providers.%s: unknown key %q", name, key)
	}
	cfg.Providers[name] = provider
	return nil
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

func setModelInfo(cfg *Config, id, key, value string) error {
	info := ensureModelInfo(cfg, id)
	switch strings.ToLower(key) {
	case "context_window_tokens", "contextwindowtokens", "context_window":
		n, err := parseStrictNonNegativeInt(value)
		if err != nil {
			return fmt.Errorf("model_catalog.%s.%s: %w", id, key, err)
		}
		info.ContextWindowTokens = n
	case "max_output_tokens", "maxoutputtokens":
		n, err := parseStrictNonNegativeInt(value)
		if err != nil {
			return fmt.Errorf("model_catalog.%s.%s: %w", id, key, err)
		}
		info.MaxOutputTokens = n
	default:
		return fmt.Errorf("model_catalog.%s: unknown key %q", id, key)
	}
	cfg.ModelCatalog[id] = info
	return nil
}

func applyEnv(cfg *Config) error {
	if value := os.Getenv("AI_PROXY_LISTEN_ADDR"); value != "" {
		cfg.ListenAddr = value
	}
	if value := os.Getenv("AI_PROXY_PORT"); value != "" {
		cfg.ListenAddr = addrFromPort(value)
	}
	if value := os.Getenv("AI_PROXY_INBOUND_API_KEY"); value != "" {
		cfg.InboundAPIKey = value
	}
	if value := os.Getenv("AI_PROXY_MAX_REQUEST_BODY_BYTES"); value != "" {
		n, err := parseStrictPositiveInt64(value)
		if err != nil {
			return fmt.Errorf("AI_PROXY_MAX_REQUEST_BODY_BYTES: %w", err)
		}
		cfg.MaxRequestBodyBytes = n
	}
	if value := os.Getenv("AI_PROXY_MAX_UPSTREAM_RESPONSE_BYTES"); value != "" {
		n, err := parseStrictPositiveInt64(value)
		if err != nil {
			return fmt.Errorf("AI_PROXY_MAX_UPSTREAM_RESPONSE_BYTES: %w", err)
		}
		cfg.MaxUpstreamResponseBytes = n
	}
	if value := os.Getenv("AI_PROXY_MAX_STREAM_BYTES"); value != "" {
		n, err := parseStrictPositiveInt64(value)
		if err != nil {
			return fmt.Errorf("AI_PROXY_MAX_STREAM_BYTES: %w", err)
		}
		cfg.MaxStreamBytes = n
	}
	if value := os.Getenv("AI_PROXY_MAX_SSE_LINE_BYTES"); value != "" {
		n, err := parseStrictPositiveInt64(value)
		if err != nil {
			return fmt.Errorf("AI_PROXY_MAX_SSE_LINE_BYTES: %w", err)
		}
		cfg.MaxSSELineBytes = n
	}
	if value := os.Getenv("AI_PROXY_ARCHIVE_FULL_CONTENT"); value != "" {
		b, err := parseStrictBool(value)
		if err != nil {
			return fmt.Errorf("AI_PROXY_ARCHIVE_FULL_CONTENT: %w", err)
		}
		cfg.ArchiveFullContent = b
	}
	if value := os.Getenv("AI_PROXY_USAGE_FILE"); value != "" {
		cfg.UsageFile = value
	}
	if value := os.Getenv("AI_PROXY_INTERACTION_DIR"); value != "" {
		cfg.InteractionDir = value
	}
	if value := os.Getenv("AI_PROXY_INTERACTION_RETENTION"); value != "" {
		n, err := parseStrictPositiveInt(value)
		if err != nil {
			return fmt.Errorf("AI_PROXY_INTERACTION_RETENTION: %w", err)
		}
		cfg.InteractionRetention = n
	}
	if value := os.Getenv("AI_PROXY_DEBUG_LOG"); value != "" {
		b, err := parseStrictBool(value)
		if err != nil {
			return fmt.Errorf("AI_PROXY_DEBUG_LOG: %w", err)
		}
		cfg.DebugLog = b
	}
	if value := firstEnv("AI_PROXY_LOG_FORMAT", "LOG_FORMAT"); value != "" {
		cfg.LogFormat = value
	}
	if value := os.Getenv("AI_PROXY_REQUEST_TIMEOUT_SECONDS"); value != "" {
		n, err := parseStrictPositiveInt(value)
		if err != nil {
			return fmt.Errorf("AI_PROXY_REQUEST_TIMEOUT_SECONDS: %w", err)
		}
		cfg.RequestTimeout = time.Duration(n) * time.Second
	}
	if value := os.Getenv("AI_PROXY_STREAM_IDLE_TIMEOUT_SECONDS"); value != "" {
		n, err := parseStrictNonNegativeInt(value)
		if err != nil {
			return fmt.Errorf("AI_PROXY_STREAM_IDLE_TIMEOUT_SECONDS: %w", err)
		}
		cfg.StreamIdleTimeout = time.Duration(n) * time.Second
	}
	if value := os.Getenv("AI_PROXY_DEFAULT_PROVIDER"); value != "" {
		cfg.DefaultProvider = value
	}
	if value := os.Getenv("AI_PROXY_METRICS_REMOTE_ACCESS"); value != "" {
		b, err := parseStrictBool(value)
		if err != nil {
			return fmt.Errorf("AI_PROXY_METRICS_REMOTE_ACCESS: %w", err)
		}
		cfg.MetricsRemoteAccess = b
	}
	if value := os.Getenv("AI_PROXY_METRICS_ALLOWED_CIDRS"); value != "" {
		cfg.MetricsAllowedCIDRs = parseList(value)
	}

	if err := applyProviderEnv(cfg, "openai", "https://api.openai.com"); err != nil {
		return err
	}
	if err := applyProviderEnv(cfg, "deepseek", "https://api.deepseek.com"); err != nil {
		return err
	}
	if err := applyProviderEnv(cfg, "anthropic", "https://api.anthropic.com"); err != nil {
		return err
	}

	if key := os.Getenv("API_KEY"); key != "" {
		provider := ensureProvider(cfg, "custom")
		provider.APIKey = key
		if base := os.Getenv("API_BASE_URL"); base != "" {
			provider.BaseURL = base
		}
		cfg.Providers["custom"] = provider
	}
	return nil
}

func applyProviderEnv(cfg *Config, name, fallbackBaseURL string) error {
	envPrefix := strings.ToUpper(strings.ReplaceAll(name, "-", "_"))
	key := firstEnv("AI_PROXY_"+envPrefix+"_API_KEY", envPrefix+"_API_KEY")
	baseURL := firstEnv("AI_PROXY_"+envPrefix+"_BASE_URL", envPrefix+"_BASE_URL")
	models := firstEnv("AI_PROXY_"+envPrefix+"_MODELS", envPrefix+"_MODELS")
	fallbacks := firstEnv("AI_PROXY_"+envPrefix+"_FALLBACKS", envPrefix+"_FALLBACKS")
	enabled := firstEnv("AI_PROXY_"+envPrefix+"_ENABLED", envPrefix+"_ENABLED")
	if key == "" && baseURL == "" && models == "" && fallbacks == "" && enabled == "" {
		return nil
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
		// models 严格区分大小写,与请求 body.model 原文匹配。
		provider.Models = parseModelList(models)
	}
	if fallbacks != "" {
		provider.Fallbacks = parseList(fallbacks)
	}
	if enabled != "" {
		b, err := parseStrictBool(enabled)
		if err != nil {
			return fmt.Errorf("%s_ENABLED: %w", envPrefix, err)
		}
		provider.Disabled = !b
	}
	cfg.Providers[name] = provider
	return nil
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

func normalize(cfg *Config) error {
	cfg.LogFormat = normalizeLogFormat(cfg.LogFormat)
	cfg.DefaultProvider = strings.ToLower(strings.TrimSpace(cfg.DefaultProvider))
	cfg.InboundAPIKey = strings.TrimSpace(cfg.InboundAPIKey)
	if cfg.MaxRequestBodyBytes <= 0 {
		cfg.MaxRequestBodyBytes = DefaultMaxRequestBodyBytes
	}
	if cfg.MaxUpstreamResponseBytes <= 0 {
		cfg.MaxUpstreamResponseBytes = DefaultMaxUpstreamResponseBytes
	}
	if cfg.MaxStreamBytes <= 0 {
		cfg.MaxStreamBytes = DefaultMaxStreamBytes
	}
	if cfg.MaxSSELineBytes <= 0 {
		cfg.MaxSSELineBytes = DefaultMaxSSELineBytes
	}
	normalized := make(map[string]Provider, len(cfg.Providers))
	for name, provider := range cfg.Providers {
		key := strings.ToLower(strings.TrimSpace(name))
		if key == "" {
			return fmt.Errorf("provider name is empty")
		}
		if existing, ok := normalized[key]; ok {
			return fmt.Errorf("duplicate provider name after case fold: %q and %q both map to %q", existing.Name, name, key)
		}
		provider.Name = key
		if provider.Protocol == "" {
			provider.Protocol = "openai"
		}
		provider.Protocol = strings.ToLower(provider.Protocol)
		provider.BaseURL = strings.TrimRight(provider.BaseURL, "/")
		// models 严格区分大小写,与请求 body.model 原文匹配。
		provider.Models = normalizeModelPatterns(provider.Models)
		// fallbacks 是 provider 名,与 provider 键一样做大小写折叠。
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
		// model id 严格区分大小写:查找键与展示 ID 均保留配置原文。
		if prev, ok := catalog[id]; ok {
			return fmt.Errorf("duplicate model_catalog id: %q (also seen as %q)", id, prev.ID)
		}
		catalog[id] = info
	}
	cfg.ModelCatalog = catalog
	return nil
}

// validate 在启动期做完整校验,把配置错误尽早暴露。

func validateMetricsCIDRs(cidrs []string) error {
	for _, cidr := range cidrs {
		cidr = strings.TrimSpace(cidr)
		if cidr == "" {
			continue
		}
		if ip := net.ParseIP(cidr); ip != nil {
			continue
		}
		if _, _, err := net.ParseCIDR(cidr); err != nil {
			return fmt.Errorf("metrics_allowed_cidrs: invalid entry %q", cidr)
		}
	}
	return nil
}

func validate(cfg Config) error {
	if len(cfg.Providers) == 0 {
		return fmt.Errorf("no providers configured; set config.yaml providers or OPENAI_API_KEY/API_KEY")
	}
	if !hasEnabledProvider(cfg.Providers) {
		return fmt.Errorf("no enabled providers configured")
	}
	if err := validateDefaultProvider(cfg); err != nil {
		return err
	}
	if err := validateListenAndAuth(cfg); err != nil {
		return err
	}
	if err := validateProviders(cfg); err != nil {
		return err
	}
	if err := validateSLO(cfg.SLO); err != nil {
		return err
	}
	if err := validateMetricsCIDRs(cfg.MetricsAllowedCIDRs); err != nil {
		return err
	}
	return nil
}

// validateListenAndAuth:非 loopback 监听时必须配置入站 API Key。
func validateListenAndAuth(cfg Config) error {
	if IsLoopbackListenAddr(cfg.ListenAddr) {
		return nil
	}
	if strings.TrimSpace(cfg.InboundAPIKey) == "" {
		return fmt.Errorf("listen_addr %q is not loopback; set inbound_api_key (or AI_PROXY_INBOUND_API_KEY) to require client auth", cfg.ListenAddr)
	}
	return nil
}

// IsLoopbackListenAddr 判断监听地址是否绑定 loopback。
// 空 host(如 ":8080") 表示所有网卡,不算 loopback。
func IsLoopbackListenAddr(addr string) bool {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return false
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		if strings.HasPrefix(addr, ":") {
			return false
		}
		ip := net.ParseIP(addr)
		return ip != nil && ip.IsLoopback()
	}
	if host == "" {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func validateHTTPBaseURL(raw string) error {
	parsed, err := url.ParseRequestURI(raw)
	if err != nil {
		return fmt.Errorf("invalid URL: %w", err)
	}
	switch strings.ToLower(parsed.Scheme) {
	case "http", "https":
	default:
		return fmt.Errorf("unsupported scheme %q (want http or https)", parsed.Scheme)
	}
	if parsed.Host == "" {
		return fmt.Errorf("missing host")
	}
	return nil
}

func validateProviders(cfg Config) error {
	for name, provider := range cfg.Providers {
		if provider.Disabled {
			continue
		}
		if strings.TrimSpace(provider.BaseURL) == "" {
			return fmt.Errorf("provider %q has empty base_url", name)
		}
		if err := validateHTTPBaseURL(provider.BaseURL); err != nil {
			return fmt.Errorf("provider %q base_url: %w", name, err)
		}
		switch provider.Protocol {
		case "openai", "anthropic":
		default:
			return fmt.Errorf("provider %q has unknown protocol %q (want openai or anthropic)", name, provider.Protocol)
		}
		for _, fb := range provider.Fallbacks {
			fallback, ok := cfg.Providers[fb]
			if !ok {
				return fmt.Errorf("provider %q fallback %q is not configured", name, fb)
			}
			if fallback.Disabled {
				continue
			}
			if fallback.Protocol != provider.Protocol {
				return fmt.Errorf("provider %q fallback %q has protocol %q, want same as primary %q", name, fb, fallback.Protocol, provider.Protocol)
			}
		}
	}
	return nil
}

func validateSLO(slo SLOConfig) error {
	if slo.CacheHitRateMin < 0 || slo.CacheHitRateMin > 1 {
		return fmt.Errorf("slo_cache_hit_rate_min must be in [0,1], got %v", slo.CacheHitRateMin)
	}
	if slo.UpstreamErrorRateMax < 0 || slo.UpstreamErrorRateMax > 1 {
		return fmt.Errorf("slo_upstream_error_rate_max must be in [0,1], got %v", slo.UpstreamErrorRateMax)
	}
	if slo.P99LatencyMaxMS < 0 {
		return fmt.Errorf("slo_p99_latency_max_ms must be >= 0, got %v", slo.P99LatencyMaxMS)
	}
	if slo.CheckIntervalSeconds < 0 {
		return fmt.Errorf("slo_check_interval_seconds must be >= 0, got %d", slo.CheckIntervalSeconds)
	}
	if wh := strings.TrimSpace(slo.ViolationWebhook); wh != "" {
		if err := validateHTTPBaseURL(wh); err != nil {
			return fmt.Errorf("slo_violation_webhook: %w", err)
		}
	}
	return nil
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

// addrFromPort 将纯端口转为 loopback 地址,避免 AI_PROXY_PORT=8080 变成 :8080(全网卡)。
// 若传入已是 host:port 或 :port,则保留原语义(:port 仍表示全网卡,需配 inbound_api_key)。
func addrFromPort(port string) string {
	port = strings.TrimSpace(port)
	if port == "" {
		return "127.0.0.1:8080"
	}
	// 已是 host:port 或 [ipv6]:port
	if strings.Contains(port, ":") && !strings.HasPrefix(port, ":") {
		return port
	}
	port = strings.TrimPrefix(port, ":")
	return "127.0.0.1:" + port
}

func firstEnv(names ...string) string {
	for _, name := range names {
		if value := os.Getenv(name); value != "" {
			return value
		}
	}
	return ""
}

func parseStrictBool(value string) (bool, error) {
	parsed, err := strconv.ParseBool(strings.TrimSpace(value))
	if err != nil {
		return false, fmt.Errorf("invalid boolean %q", value)
	}
	return parsed, nil
}

func parseStrictPositiveInt(value string) (int, error) {
	parsed, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil {
		return 0, fmt.Errorf("invalid integer %q", value)
	}
	if parsed <= 0 {
		return 0, fmt.Errorf("expected positive integer, got %d", parsed)
	}
	return parsed, nil
}

func parseStrictNonNegativeInt(value string) (int, error) {
	parsed, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil {
		return 0, fmt.Errorf("invalid integer %q", value)
	}
	if parsed < 0 {
		return 0, fmt.Errorf("expected non-negative integer, got %d", parsed)
	}
	return parsed, nil
}

func parseStrictPositiveInt64(value string) (int64, error) {
	parsed, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid integer %q", value)
	}
	if parsed <= 0 {
		return 0, fmt.Errorf("expected positive integer, got %d", parsed)
	}
	return parsed, nil
}

func parseStrictFloat(value string) (float64, error) {
	parsed, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
	if err != nil {
		return 0, fmt.Errorf("invalid number %q", value)
	}
	return parsed, nil
}

// parseList 解析逗号分隔列表,并折叠为小写(用于 provider fallbacks / CIDR 等)。
func parseList(value string) []string {
	return parseCSVList(value, true)
}

// parseModelList 解析 models 列表,保留原文大小写。
func parseModelList(value string) []string {
	return parseCSVList(value, false)
}

func parseCSVList(value string, foldCase bool) []string {
	value = strings.TrimSpace(value)
	value = strings.TrimPrefix(value, "[")
	value = strings.TrimSuffix(value, "]")
	if value == "" {
		return nil
	}
	parts := strings.Split(value, ",")
	items := make([]string, 0, len(parts))
	for _, part := range parts {
		item := strings.TrimSpace(unquote(part))
		if foldCase {
			item = strings.ToLower(item)
		}
		if item != "" {
			items = append(items, item)
		}
	}
	return items
}

// normalizeList 折叠为小写(provider fallbacks 等)。
func normalizeList(values []string) []string {
	return normalizeCSVList(values, true)
}

// normalizeModelPatterns 保留 models 原文大小写,仅 trim。
func normalizeModelPatterns(values []string) []string {
	return normalizeCSVList(values, false)
}

func normalizeCSVList(values []string, foldCase bool) []string {
	if len(values) == 0 {
		return nil
	}
	normalized := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if foldCase {
			value = strings.ToLower(value)
		}
		if value != "" {
			normalized = append(normalized, value)
		}
	}
	return normalized
}
