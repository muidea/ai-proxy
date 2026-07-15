package config

import (
	"bufio"
	"fmt"
	"net"
	"net/url"
	"os"
	"sort"
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
	MetricsRemoteAccess  bool
	MetricsAllowedCIDRs  []string
	SLO                  SLOConfig
	Providers            map[string]Provider
	// ModelCatalog 是全局模型元数据目录,供 GET /v1/models 使用;是请求路由与 /v1/models 的共同 authority。
	ModelCatalog map[string]ModelInfo
}

// ModelInfo 描述客户端可查询的模型能力与确定路由(各 provider 共用同一目录)。
// Operations 为规范化执行合同,仅允许 chat_completions / embeddings。
// RouteOwner 在启动校验后填入唯一匹配的 enabled provider 名。
type ModelInfo struct {
	ID                  string
	ContextWindowTokens int
	MaxOutputTokens     int
	Operations          []string
	RouteOwner          string
}

// ResolvedModelRoute 是启动期只读模型路由 authority 的请求侧视图。
// 回答“这个具体模型属于谁、具备哪些业务 operation”,供 /v1/models 与请求路由共同消费。
// 与 ModelInfo 同源:LookupResolvedModelRoute 从 ModelCatalog 投影,不单独持有第二份状态。
type ResolvedModelRoute struct {
	ModelID    string
	Operations []string
	RouteOwner string
}

// MaxModelCatalogIDLength 限制 model_catalog id 长度,避免异常配置与标签膨胀。
const MaxModelCatalogIDLength = 256

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
	Name     string
	Protocol string
	BaseURL  string
	APIKey   string
	Models   []string
	// EndpointCapabilities 为 provider 显式声明的直通端点能力(非 protocol 推断)。
	// 取值: chat_completions / messages / responses / completions / embeddings。
	EndpointCapabilities []string
	// AllowUnauthenticated 仅允许受信 loopback 上游在无 API Key 时启动。
	// 远程 base_url 即使设置 true 也必须 fail-fast。
	AllowUnauthenticated bool
	Disabled             bool
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
	ensureProviderNames(&cfg)
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
		return fmt.Errorf("default_provider is not supported; routing uses model_catalog RouteOwner only")
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
		return fmt.Errorf("providers.%s: fallbacks is not supported; remove fallbacks and rely on unique model_catalog RouteOwner", name)
	case "endpoint_capabilities", "endpoint_capability":
		caps, err := parseEndpointCapabilities(value)
		if err != nil {
			return fmt.Errorf("providers.%s.endpoint_capabilities: %w", name, err)
		}
		provider.EndpointCapabilities = caps
	case "allow_unauthenticated":
		b, err := parseStrictBool(value)
		if err != nil {
			return fmt.Errorf("providers.%s.allow_unauthenticated: %w", name, err)
		}
		provider.AllowUnauthenticated = b
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
	case "operations", "operation":
		ops, err := parseModelOperations(value)
		if err != nil {
			return fmt.Errorf("model_catalog.%s.%s: %w", id, key, err)
		}
		info.Operations = ops
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

	// Provider 仅从 config 文件声明;不支持通过 env 注入/创建 provider。
	// api_key 等字段仍可用 ${ENV} 在配置文件中展开。
	return nil
}

// ensureProviderNames 只补齐 Provider.Name = map key,不补 protocol/base_url/api_key。
// protocol 与 base_url 必须由配置显式声明,否则 validate 启动失败。
func ensureProviderNames(cfg *Config) {
	for name, provider := range cfg.Providers {
		if provider.Name == "" {
			provider.Name = name
			cfg.Providers[name] = provider
		}
	}
}

func normalize(cfg *Config) error {
	cfg.LogFormat = normalizeLogFormat(cfg.LogFormat)
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
		// protocol 必须显式配置;空值留给 validate fail-fast,不按 provider 名推断。
		if provider.Protocol != "" {
			provider.Protocol = strings.ToLower(provider.Protocol)
		}
		provider.BaseURL = strings.TrimRight(provider.BaseURL, "/")
		// models 严格区分大小写,与请求 body.model 原文匹配。
		provider.Models = normalizeModelPatterns(provider.Models)
		caps, err := normalizeEndpointCapabilities(provider.EndpointCapabilities)
		if err != nil {
			return fmt.Errorf("provider %q endpoint_capabilities: %w", key, err)
		}
		provider.EndpointCapabilities = caps
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
		if err := validateModelCatalogID(id); err != nil {
			return fmt.Errorf("model_catalog.%s: %w", id, err)
		}
		info.ID = id
		if info.ContextWindowTokens < 0 {
			info.ContextWindowTokens = 0
		}
		if info.MaxOutputTokens < 0 {
			info.MaxOutputTokens = 0
		}
		ops, err := normalizeModelOperations(info.Operations)
		if err != nil {
			return fmt.Errorf("model_catalog.%s.operations: %w", id, err)
		}
		if len(ops) == 0 {
			return fmt.Errorf("model_catalog.%s.operations: at least one of chat_completions, embeddings is required", id)
		}
		info.Operations = ops
		// model ID 严格区分大小写:DeepSeek-V4-Flash 与 deepseek-v4-flash 是两个不同模型。
		// 仅 exact id 唯一;不做 case-fold 去重。
		if prev, ok := catalog[id]; ok {
			return fmt.Errorf("duplicate model_catalog id: %q (also seen as %q)", id, prev.ID)
		}
		info.RouteOwner = "" // filled in validateModelRoutes
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
		return fmt.Errorf("no providers configured; declare providers in config.yaml")
	}
	if !hasEnabledProvider(cfg.Providers) {
		return fmt.Errorf("no enabled providers configured")
	}
	if err := validateListenAndAuth(cfg); err != nil {
		return err
	}
	if err := validateProviders(cfg); err != nil {
		return err
	}
	if err := validateModelRoutes(cfg); err != nil {
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
		if strings.TrimSpace(provider.Protocol) == "" {
			return fmt.Errorf("provider %q protocol is required (explicit; not inferred from name)", name)
		}
		switch provider.Protocol {
		case "openai", "anthropic":
		default:
			return fmt.Errorf("provider %q has unknown protocol %q (want openai or anthropic)", name, provider.Protocol)
		}
		if strings.TrimSpace(provider.BaseURL) == "" {
			return fmt.Errorf("provider %q base_url is required (explicit; not inferred from name)", name)
		}
		if err := validateHTTPBaseURL(provider.BaseURL); err != nil {
			return fmt.Errorf("provider %q base_url: %w", name, err)
		}
		if err := validateProviderAPIKey(name, provider); err != nil {
			return err
		}
		if len(provider.Models) == 0 {
			return fmt.Errorf("provider %q models is required (explicit; not inferred from provider name or protocol)", name)
		}
		if len(provider.EndpointCapabilities) == 0 {
			return fmt.Errorf("provider %q endpoint_capabilities is required (explicit; not inferred from protocol)", name)
		}
		if err := validateProtocolEndpointCaps(provider.Protocol, provider.EndpointCapabilities); err != nil {
			return fmt.Errorf("provider %q: %w", name, err)
		}
	}
	return nil
}

// validateProviderAPIKey:远程上游必须有 API Key;仅 allow_unauthenticated + loopback base_url 允许空 Key。
func validateProviderAPIKey(name string, provider Provider) error {
	key := strings.TrimSpace(provider.APIKey)
	if provider.AllowUnauthenticated && key != "" {
		return fmt.Errorf("provider %q allow_unauthenticated requires empty api_key; authenticated and unauthenticated modes are mutually exclusive", name)
	}
	if key != "" {
		return nil
	}
	loopback, err := isLoopbackBaseURL(provider.BaseURL)
	if err != nil {
		return fmt.Errorf("provider %q base_url: %w", name, err)
	}
	if provider.AllowUnauthenticated {
		if !loopback {
			return fmt.Errorf("provider %q allow_unauthenticated requires loopback base_url; remote empty api_key is not allowed", name)
		}
		return nil
	}
	if loopback {
		return fmt.Errorf("provider %q has empty api_key; set api_key or allow_unauthenticated=true for trusted loopback upstream", name)
	}
	return fmt.Errorf("provider %q has empty api_key; remote providers require explicit credentials", name)
}

func isLoopbackBaseURL(raw string) (bool, error) {
	parsed, err := url.ParseRequestURI(raw)
	if err != nil {
		return false, err
	}
	host := parsed.Hostname()
	if host == "" {
		return false, fmt.Errorf("missing host")
	}
	if host == "localhost" {
		return true, nil
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback(), nil
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

// parseList 解析逗号分隔列表,并折叠为小写(用于 CIDR 等)。
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

// normalizeList 折叠为小写(CIDR 等)。
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

// 允许的 model_catalog.operations 取值(与 WorkOrch LLMOperation 对齐)。
const (
	ModelOperationChatCompletions = "chat_completions"
	ModelOperationEmbeddings      = "embeddings"
)

// parseModelOperations 解析 model_catalog.operations(逗号分隔或单值),保留已知枚举大小写折叠后的规范名。
func parseModelOperations(value string) ([]string, error) {
	raw := parseCSVList(value, true)
	return normalizeModelOperations(raw)
}

// normalizeModelOperations 去重并稳定排序;仅允许 chat_completions / embeddings。
func normalizeModelOperations(values []string) ([]string, error) {
	if len(values) == 0 {
		return nil, nil
	}
	seen := map[string]struct{}{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		op := strings.ToLower(strings.TrimSpace(value))
		if op == "" {
			continue
		}
		switch op {
		case ModelOperationChatCompletions, ModelOperationEmbeddings:
		default:
			return nil, fmt.Errorf("unknown operation %q (allowed: chat_completions, embeddings)", value)
		}
		if _, ok := seen[op]; ok {
			continue
		}
		seen[op] = struct{}{}
		out = append(out, op)
	}
	// 稳定顺序:chat_completions 在前,embeddings 在后。
	sort.SliceStable(out, func(i, j int) bool {
		return operationRank(out[i]) < operationRank(out[j])
	})
	return out, nil
}

func operationRank(op string) int {
	switch op {
	case ModelOperationChatCompletions:
		return 0
	case ModelOperationEmbeddings:
		return 1
	default:
		return 100
	}
}

func validateModelCatalogID(id string) error {
	if id == "" {
		return fmt.Errorf("id is empty")
	}
	if len(id) > MaxModelCatalogIDLength {
		return fmt.Errorf("id exceeds max length %d", MaxModelCatalogIDLength)
	}
	for _, r := range id {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("id contains control character")
		}
	}
	return nil
}

// validateModelRoutes 保证每个 catalog model:
// 1) 容量完整且 max_output < context_window;
// 2) 唯一匹配一个 enabled provider,并写入 RouteOwner。
// 校验通过后 ModelCatalog 成为路由与 /v1/models 的唯一 authority;无 fallback 兜底。
func validateModelRoutes(cfg Config) error {
	// mutate via reassignment of map values
	// caller holds cfg by value but map is reference — safe to update entries.
	for id, info := range cfg.ModelCatalog {
		if info.ContextWindowTokens <= 0 || info.MaxOutputTokens <= 0 {
			return fmt.Errorf("model_catalog.%s: context_window_tokens and max_output_tokens must both be positive", id)
		}
		if info.MaxOutputTokens >= info.ContextWindowTokens {
			return fmt.Errorf("model_catalog.%s: max_output_tokens must be less than context_window_tokens", id)
		}
		matches := matchingEnabledProviders(cfg.Providers, id)
		switch len(matches) {
		case 0:
			return fmt.Errorf("model_catalog.%s: no enabled provider matches model; configure providers.*.models", id)
		case 1:
			info.RouteOwner = matches[0]
		default:
			return fmt.Errorf("model_catalog.%s: multiple enabled providers match model %v; disambiguate providers.*.models", id, matches)
		}
		primary, ok := cfg.Providers[info.RouteOwner]
		if !ok || primary.Disabled {
			return fmt.Errorf("model_catalog.%s: route owner %q is missing or disabled", id, info.RouteOwner)
		}
		if !ProviderMatchesModel(info.RouteOwner, primary, id) {
			return fmt.Errorf("model_catalog.%s: route owner %q models do not match model id", id, info.RouteOwner)
		}
		// catalog operations 必须可被 RouteOwner 在 canonical 入站 path 上服务。
		if err := validateModelOperationsAgainstProvider(id, info.Operations, primary); err != nil {
			return err
		}
		cfg.ModelCatalog[id] = info
	}
	return nil
}

// validateModelOperationsAgainstProvider 保证每个 operation 的 canonical path 可被 provider 服务。
// chat_completions → /v1/chat/completions; embeddings → /v1/embeddings。
// 校验依据与请求期 TransportPlan 矩阵一致:protocol × direct endpoint capability × 已实现 conversion。
func validateModelOperationsAgainstProvider(modelID string, operations []string, provider Provider) error {
	for _, op := range operations {
		path := OperationToPrimaryInboundPath(op)
		if path == "" {
			return fmt.Errorf("model_catalog.%s: unknown operation %q", modelID, op)
		}
		// ProviderSupportsInboundPath 编码了 canonical TransportPlan readiness。
		if !ProviderSupportsInboundPath(provider, path) {
			return fmt.Errorf("model_catalog.%s: operation %q requires route owner to support inbound path %q (check endpoint_capabilities)", modelID, op, path)
		}
	}
	return nil
}

// matchingEnabledProviders 返回匹配 model 的 enabled provider 名(稳定排序)。
func matchingEnabledProviders(providers map[string]Provider, model string) []string {
	model = strings.TrimSpace(model)
	if model == "" {
		return nil
	}
	matches := make([]string, 0, 1)
	for name, provider := range providers {
		if provider.Disabled {
			continue
		}
		if ProviderMatchesModel(name, provider, model) {
			matches = append(matches, name)
		}
	}
	sort.Strings(matches)
	return matches
}

// ProviderMatchesModel 判断 provider 的 models 模式是否匹配 model(区分大小写,仅 trim)。
func ProviderMatchesModel(_ string, provider Provider, model string) bool {
	for _, pattern := range provider.Models {
		if MatchModelPattern(model, pattern) {
			return true
		}
	}
	return false
}

// MatchModelPattern 精确或前缀通配(* 后缀);model 与 pattern 均区分大小写。
func MatchModelPattern(model, pattern string) bool {
	pattern = strings.TrimSpace(pattern)
	model = strings.TrimSpace(model)
	switch {
	case pattern == "":
		return false
	case pattern == "*":
		return true
	case strings.HasSuffix(pattern, "*"):
		return strings.HasPrefix(model, strings.TrimSuffix(pattern, "*"))
	default:
		return model == pattern
	}
}

// 允许的 provider.endpoint_capabilities 枚举。
const (
	EndpointCapabilityChatCompletions = "chat_completions"
	EndpointCapabilityMessages        = "messages"
	EndpointCapabilityResponses       = "responses"
	EndpointCapabilityCompletions     = "completions"
	EndpointCapabilityEmbeddings      = "embeddings"
)

func parseEndpointCapabilities(value string) ([]string, error) {
	raw := parseCSVList(value, true)
	return normalizeEndpointCapabilities(raw)
}

func normalizeEndpointCapabilities(values []string) ([]string, error) {
	if len(values) == 0 {
		return nil, nil
	}
	seen := map[string]struct{}{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		capName := strings.ToLower(strings.TrimSpace(value))
		if capName == "" {
			continue
		}
		switch capName {
		case EndpointCapabilityChatCompletions, EndpointCapabilityMessages, EndpointCapabilityResponses,
			EndpointCapabilityCompletions, EndpointCapabilityEmbeddings:
		default:
			return nil, fmt.Errorf("unknown endpoint capability %q", value)
		}
		if _, ok := seen[capName]; ok {
			continue
		}
		seen[capName] = struct{}{}
		out = append(out, capName)
	}
	sort.SliceStable(out, func(i, j int) bool {
		return endpointCapabilityRank(out[i]) < endpointCapabilityRank(out[j])
	})
	return out, nil
}

func endpointCapabilityRank(capName string) int {
	switch capName {
	case EndpointCapabilityChatCompletions:
		return 0
	case EndpointCapabilityMessages:
		return 1
	case EndpointCapabilityResponses:
		return 2
	case EndpointCapabilityCompletions:
		return 3
	case EndpointCapabilityEmbeddings:
		return 4
	default:
		return 100
	}
}

func validateProtocolEndpointCaps(protocol string, caps []string) error {
	for _, capName := range caps {
		switch protocol {
		case "openai":
			switch capName {
			case EndpointCapabilityChatCompletions, EndpointCapabilityResponses, EndpointCapabilityCompletions, EndpointCapabilityEmbeddings:
			case EndpointCapabilityMessages:
				return fmt.Errorf("endpoint_capabilities messages is invalid for openai protocol (use chat_completions; conversion serves /v1/messages)")
			default:
				return fmt.Errorf("unknown endpoint capability %q", capName)
			}
		case "anthropic":
			switch capName {
			case EndpointCapabilityMessages:
			case EndpointCapabilityChatCompletions, EndpointCapabilityResponses, EndpointCapabilityCompletions, EndpointCapabilityEmbeddings:
				return fmt.Errorf("endpoint_capabilities %q is invalid for anthropic protocol (use messages; conversion may serve /v1/chat/completions)", capName)
			default:
				return fmt.Errorf("unknown endpoint capability %q", capName)
			}
		}
	}
	return nil
}

// ProviderHasDirectEndpoint 判断 provider 是否显式声明了上游直连 endpoint capability。
// 只检查配置声明,不包含协议转换派生的客户端可服务 path。
func ProviderHasDirectEndpoint(provider Provider, capability string) bool {
	capability = strings.TrimSpace(capability)
	for _, item := range provider.EndpointCapabilities {
		if item == capability {
			return true
		}
	}
	return false
}

// ProviderHasEndpointCapability 是 ProviderHasDirectEndpoint 的历史别名。
// 新代码应使用 ProviderHasDirectEndpoint,以区分 direct endpoint 与 conversion 可服务 path。
func ProviderHasEndpointCapability(provider Provider, capability string) bool {
	return ProviderHasDirectEndpoint(provider, capability)
}

// ProviderSupportsInboundPath 根据固定转发矩阵判断 RouteOwner 是否可服务某入站 path。
// 内部只组合:upstream protocol × direct endpoint capability × 已实现转换。
// 不得仅因 protocol=openai 假定支持全部 OpenAI path。
// 注意:这是“可服务入站 path”(含 conversion),不是 direct endpoint capability。
func ProviderSupportsInboundPath(provider Provider, path string) bool {
	path = strings.TrimRight(strings.TrimSpace(path), "/")
	switch path {
	case "/v1/chat/completions":
		// openai 直通 chat_completions; anthropic 声明 messages 后可通过 conversion 服务。
		if provider.Protocol == "openai" {
			return ProviderHasDirectEndpoint(provider, EndpointCapabilityChatCompletions)
		}
		if provider.Protocol == "anthropic" {
			return ProviderHasDirectEndpoint(provider, EndpointCapabilityMessages)
		}
	case "/v1/messages":
		// anthropic 直通 messages; openai 声明 chat_completions 后可通过 conversion 服务。
		if provider.Protocol == "anthropic" {
			return ProviderHasDirectEndpoint(provider, EndpointCapabilityMessages)
		}
		if provider.Protocol == "openai" {
			return ProviderHasDirectEndpoint(provider, EndpointCapabilityChatCompletions)
		}
	case "/v1/responses":
		return provider.Protocol == "openai" && ProviderHasDirectEndpoint(provider, EndpointCapabilityResponses)
	case "/v1/completions":
		return provider.Protocol == "openai" && ProviderHasDirectEndpoint(provider, EndpointCapabilityCompletions)
	case "/v1/embeddings":
		return provider.Protocol == "openai" && ProviderHasDirectEndpoint(provider, EndpointCapabilityEmbeddings)
	}
	return false
}

// ServiceableInboundPaths 返回 provider 当前可服务的入站 path 列表(稳定排序)。
func ServiceableInboundPaths(provider Provider) []string {
	candidates := []string{
		"/v1/chat/completions",
		"/v1/messages",
		"/v1/responses",
		"/v1/completions",
		"/v1/embeddings",
	}
	out := make([]string, 0, len(candidates))
	for _, path := range candidates {
		if ProviderSupportsInboundPath(provider, path) {
			out = append(out, path)
		}
	}
	return out
}

func providersShareServiceablePath(a, b Provider) bool {
	for _, path := range ServiceableInboundPaths(a) {
		if ProviderSupportsInboundPath(b, path) {
			return true
		}
	}
	return false
}

// OperationToPrimaryInboundPath 返回 operation 的主入站 path(用于 capability 校验辅助)。
func OperationToPrimaryInboundPath(operation string) string {
	switch strings.TrimSpace(operation) {
	case ModelOperationChatCompletions:
		return "/v1/chat/completions"
	case ModelOperationEmbeddings:
		return "/v1/embeddings"
	default:
		return ""
	}
}

// LookupModel 按 exact id 查找 catalog 条目。
func LookupModel(cfg Config, modelID string) (ModelInfo, bool) {
	modelID = strings.TrimSpace(modelID)
	if modelID == "" {
		return ModelInfo{}, false
	}
	info, ok := cfg.ModelCatalog[modelID]
	return info, ok
}

// LookupResolvedModelRoute 从启动期 ModelCatalog authority 投影 ResolvedModelRoute。
// 与 LookupModel 同源,不持有第二份状态。
func LookupResolvedModelRoute(cfg Config, modelID string) (ResolvedModelRoute, bool) {
	info, ok := LookupModel(cfg, modelID)
	if !ok {
		return ResolvedModelRoute{}, false
	}
	return ResolvedModelRoute{
		ModelID:    info.ID,
		Operations: append([]string(nil), info.Operations...),
		RouteOwner: info.RouteOwner,
	}, true
}

// ModelSupportsOperation 判断 catalog 模型是否声明了 operation。
func ModelSupportsOperation(info ModelInfo, operation string) bool {
	operation = strings.TrimSpace(operation)
	for _, op := range info.Operations {
		if op == operation {
			return true
		}
	}
	return false
}

// ResolvedModelSupportsOperation 判断 ResolvedModelRoute 是否声明了 operation。
func ResolvedModelSupportsOperation(route ResolvedModelRoute, operation string) bool {
	return ModelSupportsOperation(ModelInfo{Operations: route.Operations}, operation)
}

// CatalogModelsSorted 返回 catalog 模型列表;排序键为 case-fold 仅用于稳定展示,不改变 exact id 语义。
func CatalogModelsSorted(catalog map[string]ModelInfo) []ModelInfo {
	items := make([]ModelInfo, 0, len(catalog))
	for _, info := range catalog {
		if strings.TrimSpace(info.ID) == "" {
			continue
		}
		items = append(items, info)
	}
	sort.SliceStable(items, func(i, j int) bool {
		leftFold := strings.ToLower(items[i].ID)
		rightFold := strings.ToLower(items[j].ID)
		if leftFold != rightFold {
			return leftFold < rightFold
		}
		return items[i].ID < items[j].ID
	})
	return items
}
