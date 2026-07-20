package proxy

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net/http"
	"net/textproto"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"ai-proxy/internal/pkg/aiproxyarchive"
	"ai-proxy/internal/pkg/aiproxyclientauth"
	"ai-proxy/internal/pkg/aiproxyconfig"
	"ai-proxy/internal/pkg/aiproxymetrics"
	"ai-proxy/internal/pkg/aiproxymetricsport"
	"ai-proxy/internal/pkg/aiproxyusage"
)

type Handler struct {
	cfgMu               sync.RWMutex
	cfg                 config.Config
	clientKeyIndex      atomic.Pointer[clientauth.Index]
	usageStore          usage.Store
	interactionRecorder *archive.Recorder
	metricsRegistry     metricsport.Port
	driftTracker        *FingerprintDriftTracker
	client              *http.Client
}

type archiveRoundKey struct{}

type usageCompletionKey struct{}

type usageCompletion struct{ done atomic.Bool }

func withArchiveRound(ctx context.Context, round *archive.Round) context.Context {
	return context.WithValue(ctx, archiveRoundKey{}, round)
}

func archiveRoundFromContext(ctx context.Context) *archive.Round {
	round, _ := ctx.Value(archiveRoundKey{}).(*archive.Round)
	return round
}

func withUsageCompletion(ctx context.Context, completion *usageCompletion) context.Context {
	return context.WithValue(ctx, usageCompletionKey{}, completion)
}

func usageCompletionFromContext(ctx context.Context) *usageCompletion {
	completion, _ := ctx.Value(usageCompletionKey{}).(*usageCompletion)
	return completion
}

// ConfigSnapshot 返回当前生效配置的独立快照，避免管理接口读取切片或 map 时
// 与后续配置切换共享可变底层数据。
func (h *Handler) ConfigSnapshot() config.Config {
	h.cfgMu.RLock()
	defer h.cfgMu.RUnlock()
	cfg := h.cfg
	cfg.MetricsAllowedCIDRs = append([]string(nil), h.cfg.MetricsAllowedCIDRs...)
	cfg.Providers = make(map[string]config.Provider, len(h.cfg.Providers))
	for name, provider := range h.cfg.Providers {
		provider.Models = append([]string(nil), provider.Models...)
		provider.EndpointCapabilities = append([]string(nil), provider.EndpointCapabilities...)
		cfg.Providers[name] = provider
	}
	cfg.ModelCatalog = make(map[string]config.ModelInfo, len(h.cfg.ModelCatalog))
	for id, info := range h.cfg.ModelCatalog {
		info.Operations = append([]string(nil), info.Operations...)
		cfg.ModelCatalog[id] = info
	}
	cfg.ClientAPIKeys = make(map[string]config.ClientAPIKey, len(h.cfg.ClientAPIKeys))
	for id, key := range h.cfg.ClientAPIKeys {
		cfg.ClientAPIKeys[id] = key
	}
	return cfg
}

// UpdateConfig 在完整请求边界之间原子切换运行时配置。
// 已进入代理处理的请求继续使用旧配置，新请求使用新配置。
// client_api_keys 可热更新(重建身份索引);usage_store 路径不热切换。
func (h *Handler) UpdateConfig(cfg config.Config) error {
	if err := requireResolvedConfig(cfg); err != nil {
		return err
	}
	idx := buildClientKeyIndex(cfg)
	h.cfgMu.Lock()
	defer h.cfgMu.Unlock()
	h.cfg = cfg
	h.clientKeyIndex.Store(idx)
	h.client = newHTTPClient(cfg.RequestTimeout)
	return nil
}

// NewHandler 装配代理处理器。usageStore 可为 nil(仅健康检查/测试),业务请求 Start 将失败。
func NewHandler(cfg config.Config, usageStore usage.Store, interactionRecorder *archive.Recorder, metricsSource any) *Handler {
	if err := requireResolvedConfig(cfg); err != nil {
		panic("proxy.NewHandler: " + err.Error() + "; call config.Load or tests.MustHandlerConfig")
	}
	h := &Handler{
		cfg:                 cfg,
		usageStore:          usageStore,
		interactionRecorder: interactionRecorder,
		metricsRegistry:     metricsport.AsPort(metricsSource),
		driftTracker:        NewFingerprintDriftTracker(2),
		client:              newHTTPClient(cfg.RequestTimeout),
	}
	h.clientKeyIndex.Store(buildClientKeyIndex(cfg))
	return h
}

func buildClientKeyIndex(cfg config.Config) *clientauth.Index {
	entries := config.ClientAPIKeyEntries(cfg)
	keys := make([]clientauth.KeyEntry, 0, len(entries))
	for _, e := range entries {
		keys = append(keys, clientauth.KeyEntry{ID: e.ID, APIKey: e.APIKey, APIKeyHash: e.APIKeyHash, Enabled: e.Enabled})
	}
	return clientauth.BuildIndex(keys)
}

// requireResolvedConfig 要求 Config 已通过 config.Load 的 authority 合同。
// Handler 不合成 model、不补默认容量/operations、不猜测 RouteOwner。
// 对绕过 Load 的调用方做 fail-fast 全量校验。
func requireResolvedConfig(cfg config.Config) error {
	if cfg.Providers == nil {
		return fmt.Errorf("providers is nil")
	}
	if cfg.ModelCatalog == nil {
		cfg.ModelCatalog = map[string]config.ModelInfo{}
	}
	for name, provider := range cfg.Providers {
		if provider.Disabled {
			continue
		}
		switch provider.Protocol {
		case "openai", "anthropic":
		case "":
			return fmt.Errorf("provider %q: protocol unresolved", name)
		default:
			return fmt.Errorf("provider %q: unknown protocol %q", name, provider.Protocol)
		}
		if len(provider.Models) == 0 {
			return fmt.Errorf("provider %q: models unresolved", name)
		}
		if len(provider.EndpointCapabilities) == 0 {
			return fmt.Errorf("provider %q: endpoint_capabilities unresolved", name)
		}
		if err := assertUniqueSortedKnownList("provider "+name+" endpoint_capabilities", provider.EndpointCapabilities, knownEndpointCapabilities()); err != nil {
			return err
		}
		if provider.Protocol == "openai" {
			for _, capName := range provider.EndpointCapabilities {
				if capName == config.EndpointCapabilityMessages {
					return fmt.Errorf("provider %q: endpoint_capabilities messages invalid for openai protocol", name)
				}
			}
		}
		if provider.Protocol == "anthropic" {
			for _, capName := range provider.EndpointCapabilities {
				if capName != config.EndpointCapabilityMessages {
					return fmt.Errorf("provider %q: endpoint_capabilities %q invalid for anthropic protocol", name, capName)
				}
			}
		}
	}
	for id, info := range cfg.ModelCatalog {
		if strings.TrimSpace(info.ID) == "" {
			return fmt.Errorf("model_catalog.%s: missing id", id)
		}
		if info.ID != id {
			return fmt.Errorf("model_catalog.%s: id mismatch %q", id, info.ID)
		}
		if info.ContextWindowTokens <= 0 || info.MaxOutputTokens <= 0 {
			return fmt.Errorf("model_catalog.%s: capacity unresolved", id)
		}
		if info.MaxOutputTokens >= info.ContextWindowTokens {
			return fmt.Errorf("model_catalog.%s: max_output_tokens must be less than context_window_tokens", id)
		}
		if len(info.Operations) == 0 {
			return fmt.Errorf("model_catalog.%s: operations unresolved", id)
		}
		if err := assertUniqueSortedKnownList("model_catalog."+id+" operations", info.Operations, knownModelOperations()); err != nil {
			return err
		}
		owner := strings.TrimSpace(info.RouteOwner)
		if owner == "" {
			return fmt.Errorf("model_catalog.%s: RouteOwner unresolved", id)
		}
		provider, ok := cfg.Providers[owner]
		if !ok || provider.Disabled {
			return fmt.Errorf("model_catalog.%s: RouteOwner %q missing or disabled", id, owner)
		}
		if !config.ProviderMatchesModel(owner, provider, id) {
			return fmt.Errorf("model_catalog.%s: RouteOwner %q does not match model", id, owner)
		}
		for _, op := range info.Operations {
			path := config.OperationToPrimaryInboundPath(op)
			if path == "" || !config.ProviderSupportsInboundPath(provider, path) {
				return fmt.Errorf("model_catalog.%s: operation %q not serviceable by RouteOwner %q", id, op, owner)
			}
		}
	}
	return nil
}

func knownEndpointCapabilities() map[string]int {
	return map[string]int{
		config.EndpointCapabilityChatCompletions: 0,
		config.EndpointCapabilityMessages:        1,
		config.EndpointCapabilityResponses:       2,
		config.EndpointCapabilityCompletions:     3,
		config.EndpointCapabilityEmbeddings:      4,
	}
}

func knownModelOperations() map[string]int {
	return map[string]int{
		config.ModelOperationChatCompletions: 0,
		config.ModelOperationEmbeddings:      1,
	}
}

// assertUniqueSortedKnownList 要求 values 仅含 known 键、无重复、且按 known 秩稳定升序。
func assertUniqueSortedKnownList(label string, values []string, known map[string]int) error {
	seen := map[string]struct{}{}
	prevRank := -1
	for _, value := range values {
		rank, ok := known[value]
		if !ok {
			return fmt.Errorf("%s: unknown value %q", label, value)
		}
		if _, dup := seen[value]; dup {
			return fmt.Errorf("%s: duplicate value %q", label, value)
		}
		if rank < prevRank {
			return fmt.Errorf("%s: not in stable sorted order", label)
		}
		seen[value] = struct{}{}
		prevRank = rank
	}
	return nil
}

func newHTTPClient(requestTimeout time.Duration) *http.Client {
	client := &http.Client{}
	if transport, ok := http.DefaultTransport.(*http.Transport); ok {
		cloned := transport.Clone()
		if requestTimeout > 0 {
			cloned.ResponseHeaderTimeout = requestTimeout
		}
		client.Transport = cloned
	}
	return client
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.cfgMu.RLock()
	defer h.cfgMu.RUnlock()

	requestID := ensureRequestID(r)
	r = attachRequestID(w, r, requestID)

	if r.URL.Path == "/healthz" {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
		return
	}
	if !isSupportedInbound(r.Method, r.URL.Path) {
		http.NotFound(w, r)
		return
	}
	// 客户端身份解析:缺失、未知、禁用或冲突 Key 均返回 401(不计 usage)。
	identity, err := clientauth.ResolveHeaders(r.Header, h.clientKeyIndex.Load())
	if err != nil {
		writeClientProtocolError(w, http.StatusUnauthorized, clientProtocolFromRequest(r), APIError{
			Code:           ErrorCodeAuthenticationFailed,
			Message:        "missing or invalid client api key",
			ClientProtocol: clientProtocolFromRequest(r),
			ClientEndpoint: NormalizeClientEndpoint(r.URL.Path),
			Operation:      OperationForPath(r.URL.Path),
		})
		return
	}
	r = r.WithContext(clientauth.WithClientIdentity(r.Context(), identity))

	// round 与 event 在读取 body / 访问上游前建立。event ID 不复用客户端可控的
	// X-Request-ID；round_id 用于将 usage_events 和本地归档精确关联。
	round, err := h.startRound()
	if err != nil {
		writeClientProtocolError(w, http.StatusInternalServerError, clientProtocolFromRequest(r), APIError{
			Code: ErrorCodeProxyInternalError, Message: "start interaction archive failed",
			ClientProtocol: clientProtocolFromRequest(r), ClientEndpoint: NormalizeClientEndpoint(r.URL.Path), Operation: OperationForPath(r.URL.Path),
		})
		return
	}
	if round != nil {
		round.SetRequestID(requestID)
		round.SetAPIKeyID(identity.KeyID)
		defer round.Abort()
	}
	r = r.WithContext(withArchiveRound(r.Context(), round))
	eventID := newRequestID()
	if eventID == "" {
		writeClientProtocolError(w, http.StatusServiceUnavailable, clientProtocolFromRequest(r), APIError{
			Code: ErrorCodeUsageStoreUnavailable, Message: "usage store unavailable",
			ClientProtocol: clientProtocolFromRequest(r), ClientEndpoint: NormalizeClientEndpoint(r.URL.Path), Operation: OperationForPath(r.URL.Path),
		})
		return
	}
	r = r.WithContext(withUsageCompletion(withUsageEventID(r.Context(), eventID), &usageCompletion{}))

	// 在读取 body / 访问上游前持久化 started;失败则 503。
	if !h.beginUsage(w, r, eventID, round) {
		return
	}
	// 所有已 Start 的请求都有兜底 Complete，处理器内的正常路径会先完成它。
	defer h.completePendingUsage(r, round)

	switch {
	case r.URL.Path == "/v1/chat/completions" && r.Method == http.MethodPost:
		h.handleChatCompletions(w, r, requestID)
	case r.URL.Path == "/v1/messages" && r.Method == http.MethodPost:
		h.handleAnthropicMessages(w, r, requestID)
	case r.URL.Path == "/v1/models" && (r.Method == http.MethodGet || r.Method == http.MethodPost):
		h.handleModels(w, r, requestID)
	default:
		// OpenAI 白名单透传:/v1/responses,/v1/completions,/v1/embeddings
		h.forwardRaw(w, r, requestID)
	}
}

// beginUsage 同步写入 usage started event。失败时写 503 并返回 false。
// 注意:round 在各 handler 内创建;此处先用 round_id=0 登记,Complete 时不依赖 round。
func (h *Handler) beginUsage(w http.ResponseWriter, r *http.Request, eventID string, round *archive.Round) bool {
	if h.usageStore == nil {
		writeClientProtocolError(w, http.StatusServiceUnavailable, clientProtocolFromRequest(r), APIError{
			Code:           ErrorCodeUsageStoreUnavailable,
			Message:        "usage store unavailable",
			ClientProtocol: clientProtocolFromRequest(r),
			ClientEndpoint: NormalizeClientEndpoint(r.URL.Path),
			Operation:      OperationForPath(r.URL.Path),
		})
		return false
	}
	identity := clientauth.ClientIdentityFromContext(r.Context())
	path := NormalizeClientEndpoint(r.URL.Path)
	rec := usage.StartRecord{
		EventID:        eventID,
		StartedAt:      time.Now().UTC(),
		APIKeyID:       identity.KeyID,
		Operation:      OperationForPath(path),
		Route:          RouteLabel(r),
		ClientEndpoint: path,
		ClientProtocol: ClientProtocolForPath(path),
	}
	if round != nil {
		rec.RoundID = int64(round.ID)
	}
	if err := h.usageStore.Start(r.Context(), rec); err != nil {
		if h.metricsRegistry != nil {
			h.metricsRegistry.RecordUsageStoreWriteError("start")
		}
		writeClientProtocolError(w, http.StatusServiceUnavailable, clientProtocolFromRequest(r), APIError{
			Code:           ErrorCodeUsageStoreUnavailable,
			Message:        "usage store unavailable",
			ClientProtocol: clientProtocolFromRequest(r),
			ClientEndpoint: path,
			Operation:      rec.Operation,
		})
		return false
	}
	if h.metricsRegistry != nil {
		h.metricsRegistry.SetUsageStoreHealthy(h.usageStore.Healthy())
	}
	return true
}

// completePendingUsage 仅在处理器漏掉结算（如未来新增的早退路径）时执行。
// 已完成 event 的条件更新会返回 ErrEventNotStarted，因此不会产生重复入账。
func (h *Handler) completePendingUsage(r *http.Request, round *archive.Round) {
	if completion := usageCompletionFromContext(r.Context()); completion != nil && completion.done.Load() {
		return
	}
	eventID := usageEventIDFromContext(r.Context())
	if eventID == "" || h.usageStore == nil {
		return
	}
	startedAt := time.Now()
	upstreamDuration := time.Duration(0)
	if round != nil {
		if !round.StartedAt.IsZero() {
			startedAt = round.StartedAt
		}
		upstreamDuration = round.UpstreamDuration
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := h.usageStore.Complete(ctx, usage.CompleteRecord{
		EventID: eventID, CompletedAt: time.Now().UTC(), HTTPStatus: http.StatusInternalServerError,
		Outcome: "error", ErrorCode: ErrorCodeProxyInternalError,
		Duration: time.Since(startedAt), UpstreamDuration: upstreamDuration,
	})
	if err == nil && h.metricsRegistry != nil {
		h.metricsRegistry.RecordClientUsage(clientauth.ClientIdentityFromContext(r.Context()).KeyID, 0, 0)
		h.metricsRegistry.SetUsageStoreHealthy(h.usageStore.Healthy())
	}
	if err != nil && !errors.Is(err, usage.ErrEventNotStarted) {
		if h.metricsRegistry != nil {
			h.metricsRegistry.RecordUsageStoreWriteError("complete")
		}
		slog.Error("usage store fallback complete failed", slog.String("event_id", eventID), slog.Any("error", err))
	}
}

// completeUsage 结算已 Start 的 event;失败只记日志/降级,不改变已写出的 HTTP 响应。
func (h *Handler) completeUsage(r *http.Request, requestID string, provider, model string, stream bool, status int, duration time.Duration, tok tokenUsage, outcome, errorCode string, round *archive.Round) bool {
	if h.usageStore == nil || requestID == "" {
		return false
	}
	if r != nil {
		if completion := usageCompletionFromContext(r.Context()); completion != nil {
			if !completion.done.CompareAndSwap(false, true) {
				return false
			}
		}
	}
	rec := usage.CompleteRecord{
		EventID:                  requestID,
		CompletedAt:              time.Now().UTC(),
		Provider:                 provider,
		Model:                    model,
		InputTokens:              int64(tok.PromptTokens),
		OutputTokens:             int64(tok.CompletionTokens),
		CachedInputTokens:        int64(tok.CachedInputTokens),
		CacheCreationInputTokens: int64(tok.CacheCreationInputTokens),
		HTTPStatus:               status,
		Outcome:                  outcome,
		ErrorCode:                errorCode,
		Duration:                 duration,
		Stream:                   stream,
		Estimated:                tok.Estimated,
	}
	if round != nil {
		rec.UpstreamProtocol = round.UpstreamProtocol
		rec.UpstreamEndpoint = round.UpstreamEndpoint
		rec.ConversionMode = round.ConversionMode
		rec.UpstreamDuration = round.UpstreamDuration
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := h.usageStore.Complete(ctx, rec); err != nil {
		if h.metricsRegistry != nil {
			h.metricsRegistry.RecordUsageStoreWriteError("complete")
		}
		slog.Error("usage store complete failed",
			slog.String("event_id", requestID),
			slog.String("api_key_id", clientauth.ClientIdentityFromContext(r.Context()).KeyID),
			slog.Any("error", err),
		)
		return false
	}
	if h.metricsRegistry != nil {
		h.metricsRegistry.SetUsageStoreHealthy(h.usageStore.Healthy())
	}
	return true
}

func isRequestTooLarge(err error) bool {
	var maxErr *http.MaxBytesError
	if errors.As(err, &maxErr) {
		return true
	}
	return err != nil && strings.Contains(err.Error(), "exceeds limit")
}

// readLimitedBody 使用 MaxBytesReader 读取请求体,超限返回明确错误。
func (h *Handler) readLimitedBody(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	limit := h.cfg.MaxRequestBodyBytes
	if limit <= 0 {
		limit = config.DefaultMaxRequestBodyBytes
	}
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			return nil, fmt.Errorf("request body exceeds limit of %d bytes", limit)
		}
		return nil, err
	}
	return body, nil
}

// streamLimits 返回流式累计与单行上限。
func (h *Handler) streamLimits() (maxStream, maxLine int64) {
	maxStream = h.cfg.MaxStreamBytes
	if maxStream <= 0 {
		maxStream = config.DefaultMaxStreamBytes
	}
	maxLine = h.cfg.MaxSSELineBytes
	if maxLine <= 0 {
		maxLine = config.DefaultMaxSSELineBytes
	}
	return maxStream, maxLine
}

// readSSELine 读取一行 SSE,单行超过 maxLine 返回错误。
func readSSELine(reader *bufio.Reader, maxLine int64) ([]byte, error) {
	if maxLine <= 0 {
		maxLine = config.DefaultMaxSSELineBytes
	}
	var buf []byte
	for {
		chunk, err := reader.ReadSlice('\n')
		if len(chunk) > 0 {
			if int64(len(buf)+len(chunk)) > maxLine {
				return nil, fmt.Errorf("SSE line exceeds limit of %d bytes", maxLine)
			}
			buf = append(buf, chunk...)
		}
		if err == nil {
			return buf, nil
		}
		if err == bufio.ErrBufferFull {
			// ReadSlice 缓冲满,继续累积直到换行或超限。
			continue
		}
		if len(buf) > 0 {
			return buf, err
		}
		return nil, err
	}
}

// readLimitedUpstream 读取上游响应体并施加大小上限。
func (h *Handler) readLimitedUpstream(body io.Reader) ([]byte, error) {
	limit := h.cfg.MaxUpstreamResponseBytes
	if limit <= 0 {
		limit = config.DefaultMaxUpstreamResponseBytes
	}
	limited := io.LimitReader(body, limit+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("upstream response exceeds limit of %d bytes", limit)
	}
	return data, nil
}

// isSupportedInbound 限制客户端只能访问标准 OpenAI / Anthropic path。
func isSupportedInbound(method, path string) bool {
	switch path {
	case "/v1/chat/completions", "/v1/messages", "/v1/responses", "/v1/completions", "/v1/embeddings":
		return method == http.MethodPost
	case "/v1/models":
		return method == http.MethodGet || method == http.MethodPost
	default:
		return false
	}
}

func (h *Handler) handleChatCompletions(w http.ResponseWriter, r *http.Request, requestID string) {
	start := time.Now()
	round := archiveRoundFromContext(r.Context())
	bodyBytes, err := h.readLimitedBody(w, r)
	if err != nil {
		status := http.StatusBadRequest
		code := ErrorCodeInvalidRequest
		if isRequestTooLarge(err) {
			status = http.StatusRequestEntityTooLarge
			code = ErrorCodeRequestTooLarge
		}
		h.writeArchivedAPIError(w, round, r, start, "", "", false, status, APIError{
			Code: code, Message: err.Error(), ClientProtocol: clientProtocolFromRequest(r),
			ClientEndpoint: NormalizeClientEndpoint(r.URL.Path), Operation: OperationForPath(r.URL.Path),
		})
		return
	}
	defer r.Body.Close()

	if len(bodyBytes) > 0 {
		stableHash, fingerprint := ComputeRequestFingerprint(bodyBytes)
		round.SetFingerprint(stableHash, fingerprint)
	}
	if err := round.WriteRequest(bodyBytes); err != nil {
		log.Printf("archive request: %v", err)
	}
	h.archiveAndLogClientRequest(round, r, len(bodyBytes))

	var body map[string]any
	if err := json.Unmarshal(bodyBytes, &body); err != nil {
		h.writeArchivedError(w, round, r, start, "", "", false, http.StatusBadRequest, "invalid JSON request body")
		return
	}

	model, _ := body["model"].(string)
	stream, _ := body["stream"].(bool)
	plan, apiErr := h.resolveTransportPlan(r, model)
	if apiErr != nil {
		h.writeArchivedAPIError(w, round, r, start, "", model, stream, statusForAPIError(apiErr), *apiErr)
		return
	}
	provider, ok := h.cfg.Providers[plan.RouteOwner]
	if !ok {
		h.writeArchivedAPIError(w, round, r, start, plan.RouteOwner, model, stream, http.StatusServiceUnavailable, APIError{
			Code:             ErrorCodeProviderUnavailable,
			Message:          fmt.Sprintf("provider %q is not configured", plan.RouteOwner),
			Model:            model,
			Operation:        plan.Operation,
			ClientEndpoint:   plan.ClientEndpoint,
			ClientProtocol:   plan.ClientProtocol,
			UpstreamProtocol: plan.UpstreamProtocol,
		})
		return
	}
	// model 路由仅使用 body.model + ResolvedModelRoute + TransportPlan authority。
	h.archiveAndLogTransportPlan(round, r, plan, provider, stream)

	switch plan.Mode {
	case TransportModeOpenAIToAnthropic:
		if apiErr := ValidateConversionRequest(plan, body); apiErr != nil {
			h.writeArchivedAPIError(w, round, r, start, plan.RouteOwner, model, stream, statusForAPIError(apiErr), *apiErr)
			return
		}
		h.handleAnthropicChatCompletions(w, r, round, start, plan, provider, bodyBytes, body, model, stream)
		return
	case TransportModeNative:
		// OpenAI client → OpenAI upstream native
	default:
		h.writeArchivedAPIError(w, round, r, start, plan.RouteOwner, model, stream, http.StatusBadRequest, APIError{
			Code:             ErrorCodeEndpointUnsupported,
			Message:          fmt.Sprintf("transport mode %q is not valid for %s", plan.Mode, plan.ClientEndpoint),
			Model:            model,
			Operation:        plan.Operation,
			ClientEndpoint:   plan.ClientEndpoint,
			ClientProtocol:   plan.ClientProtocol,
			UpstreamProtocol: plan.UpstreamProtocol,
		})
		return
	}

	result, err := h.doUpstreamPath(r, round, plan.RouteOwner, provider, bodyBytes, len(bodyBytes), stream, plan.UpstreamEndpoint, r.URL.RawQuery, r.Method)
	if err != nil {
		h.writeArchivedError(w, round, r, start, plan.RouteOwner, model, stream, http.StatusBadGateway, err.Error())
		return
	}
	resp := result.Response
	providerName := result.ProviderName
	if result.Cancel != nil {
		defer result.Cancel()
	}
	defer resp.Body.Close()

	if stream && resp.StatusCode < 400 {
		h.handleStreamResponse(w, resp, round, start, providerName, model, body, r.Context(), result.Cancel, r)
		return
	}
	h.handleBufferedResponse(w, resp, round, start, providerName, model, stream, body, r)
}

func (h *Handler) startRound() (*archive.Round, error) {
	if h.interactionRecorder == nil {
		return nil, nil
	}
	return h.interactionRecorder.Start()
}

func (h *Handler) writeArchivedError(w http.ResponseWriter, round *archive.Round, r *http.Request, start time.Time, provider, model string, stream bool, status int, message string) {
	// 自由文本失败统一收敛为 typed envelope,避免 text/plain。
	code := ErrorCodeInvalidRequest
	switch status {
	case http.StatusRequestEntityTooLarge:
		code = ErrorCodeRequestTooLarge
	case http.StatusInternalServerError:
		code = ErrorCodeProxyInternalError
	case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		code = ErrorCodeUpstreamUnavailable
	default:
		if strings.Contains(strings.ToLower(message), "conversion") {
			code = ErrorCodeConversionUnsupported
		} else if status >= 500 {
			code = ErrorCodeProxyInternalError
		}
	}
	apiErr := APIError{
		Code:           code,
		Message:        message,
		Model:          model,
		ClientEndpoint: "",
		ClientProtocol: clientProtocolFromRequest(r),
	}
	if r != nil && r.URL != nil {
		apiErr.ClientEndpoint = NormalizeClientEndpoint(r.URL.Path)
		apiErr.Operation = OperationForPath(r.URL.Path)
	}
	if round != nil {
		if apiErr.Operation == "" {
			apiErr.Operation = round.Operation
		}
		if apiErr.ClientEndpoint == "" {
			apiErr.ClientEndpoint = round.ClientEndpoint
		}
		if round.ClientProtocol != "" {
			apiErr.ClientProtocol = round.ClientProtocol
		}
		apiErr.UpstreamProtocol = round.UpstreamProtocol
	}
	h.writeArchivedAPIError(w, round, r, start, provider, model, stream, status, apiErr)
}

func (h *Handler) writeArchivedAPIError(w http.ResponseWriter, round *archive.Round, r *http.Request, start time.Time, provider, model string, stream bool, status int, apiErr APIError) {
	if apiErr.ClientProtocol == "" {
		apiErr.ClientProtocol = clientProtocolFromRequest(r)
	}
	if apiErr.ClientEndpoint == "" && r != nil && r.URL != nil {
		apiErr.ClientEndpoint = NormalizeClientEndpoint(r.URL.Path)
	}
	if apiErr.Operation == "" && apiErr.ClientEndpoint != "" {
		apiErr.Operation = OperationForPath(apiErr.ClientEndpoint)
	}
	if apiErr.Model == "" {
		apiErr.Model = model
	}
	writeClientProtocolError(w, status, apiErr.ClientProtocol, apiErr)
	var body []byte
	if strings.EqualFold(apiErr.ClientProtocol, ClientProtocolAnthropic) {
		msg := apiErr.Message
		if apiErr.Code != "" && !strings.Contains(msg, apiErr.Code) {
			msg = apiErr.Code + ": " + msg
		}
		body, _ = json.Marshal(AnthropicErrorResponse{
			Type: "error",
			Error: AnthropicError{
				Type:    anthropicErrorType(apiErr.Code),
				Message: msg,
			},
		})
	} else {
		if apiErr.Type == "" {
			apiErr.Type = openAIErrorType(apiErr.Code)
		}
		body, _ = json.Marshal(APIErrorResponse{Error: apiErr})
	}
	body = append(body, '\n')
	if err := round.WriteResponse("response.json", body); err != nil {
		log.Printf("archive api error response: %v", err)
	}
	duration := time.Since(start)
	usage := tokenUsage{}
	msg := apiErr.Code + ": " + apiErr.Message
	h.recordAndPrint(round, r, provider, model, stream, status, duration, usage, msg)
	h.writeArchiveMetadata(round, provider, model, stream, status, duration, usage, "response.json", msg, "", "")
}

func (h *Handler) forwardRaw(w http.ResponseWriter, r *http.Request, requestID string) {
	start := time.Now()
	round := archiveRoundFromContext(r.Context())
	body, err := h.readLimitedBody(w, r)
	if err != nil {
		status := http.StatusBadRequest
		code := ErrorCodeInvalidRequest
		if isRequestTooLarge(err) {
			status = http.StatusRequestEntityTooLarge
			code = ErrorCodeRequestTooLarge
		}
		h.writeArchivedAPIError(w, round, r, start, "", "", false, status, APIError{
			Code: code, Message: err.Error(), ClientProtocol: clientProtocolFromRequest(r),
			ClientEndpoint: NormalizeClientEndpoint(r.URL.Path), Operation: OperationForPath(r.URL.Path),
		})
		return
	}
	defer r.Body.Close()
	rawBody, rawModel, rawStream := parseRawRequestBody(body)
	if err := round.WriteRequest(body); err != nil {
		log.Printf("archive raw request: %v", err)
	}
	h.archiveAndLogClientRequest(round, r, len(body))
	// responses/completions/embeddings 仅允许矩阵中的 native 组合;TransportPlan 统一裁决。
	plan, apiErr := h.resolveTransportPlan(r, rawModel)
	if apiErr != nil {
		h.writeArchivedAPIError(w, round, r, start, "", rawModel, rawStream, statusForAPIError(apiErr), *apiErr)
		return
	}
	if plan.Mode != TransportModeNative {
		h.writeArchivedAPIError(w, round, r, start, plan.RouteOwner, rawModel, rawStream, http.StatusBadRequest, APIError{
			Code:             ErrorCodeEndpointUnsupported,
			Message:          fmt.Sprintf("endpoint %q only supports native transport; conversion is not available", plan.ClientEndpoint),
			Model:            rawModel,
			Operation:        plan.Operation,
			ClientEndpoint:   plan.ClientEndpoint,
			ClientProtocol:   plan.ClientProtocol,
			UpstreamProtocol: plan.UpstreamProtocol,
		})
		return
	}
	provider, ok := h.cfg.Providers[plan.RouteOwner]
	if !ok {
		h.writeArchivedAPIError(w, round, r, start, plan.RouteOwner, rawModel, rawStream, http.StatusServiceUnavailable, APIError{
			Code:             ErrorCodeProviderUnavailable,
			Message:          fmt.Sprintf("provider %q is not configured", plan.RouteOwner),
			Model:            rawModel,
			Operation:        plan.Operation,
			ClientEndpoint:   plan.ClientEndpoint,
			ClientProtocol:   plan.ClientProtocol,
			UpstreamProtocol: plan.UpstreamProtocol,
		})
		return
	}
	providerName := plan.RouteOwner
	h.debugfRound(round, r, "raw proxy client request method=%s path=%s query=%q provider=%s mode=%s remote=%s body_bytes=%d headers=%s",
		r.Method,
		r.URL.Path,
		r.URL.RawQuery,
		providerName,
		plan.Mode,
		r.RemoteAddr,
		len(body),
		headerSummary(sanitizeHeaders(r.Header)),
	)
	h.archiveAndLogTransportPlan(round, r, plan, provider, rawStream)
	streamRequest := rawStream || acceptsEventStream(r.Header)
	result, err := h.doUpstreamPath(r, round, providerName, provider, body, len(body), streamRequest, plan.UpstreamEndpoint, r.URL.RawQuery, r.Method)
	if err != nil {
		h.writeArchivedError(w, round, r, start, providerName, rawModel, rawStream, http.StatusBadGateway, err.Error())
		return
	}
	resp := result.Response
	providerName = result.ProviderName
	provider = result.Provider
	if result.Cancel != nil {
		defer result.Cancel()
	}
	defer resp.Body.Close()
	h.debugfRound(round, r, "raw proxy upstream response provider=%s protocol=%s status=%d upstream_duration=%s total_duration=%s content_type=%q",
		providerName,
		provider.Protocol,
		resp.StatusCode,
		result.Duration.Truncate(time.Millisecond),
		time.Since(start).Truncate(time.Millisecond),
		resp.Header.Get("Content-Type"),
	)
	responsePath := responseFileName(resp.Header.Get("Content-Type"), strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "event-stream"))
	if strings.HasSuffix(responsePath, ".sse") {
		copyResponseHeader(w.Header(), resp.Header)
		w.WriteHeader(resp.StatusCode)
		usage, fullPath, streamErr := h.copyAndArchiveRawStream(w, resp, round, providerName, provider, rawModel, rawBody, r.Context(), result.Cancel, plan.UpstreamEndpoint)
		duration := time.Since(start)
		errMsg := ""
		if streamErr != nil {
			errMsg = streamErr.Error()
		}
		h.recordAndPrintFail(round, r, providerName, rawModel, true, resp.StatusCode, duration, usage, streamErr)
		h.writeArchiveMetadata(round, providerName, rawModel, true, resp.StatusCode, duration, usage, responsePath, errMsg, fullPath, outcomeFromStreamFail(streamErr, resp.StatusCode))
		return
	}
	responseBody, err := h.readLimitedUpstream(resp.Body)
	if err != nil {
		h.writeArchivedError(w, round, r, start, providerName, rawModel, rawStream, http.StatusBadGateway, err.Error())
		return
	}
	responseBody, responseHeader, err := h.decodedResponseBodyAndHeader(responseBody, resp.Header)
	if err != nil {
		h.writeArchivedError(w, round, r, start, providerName, rawModel, rawStream, http.StatusBadGateway, err.Error())
		return
	}
	copyResponseHeader(w.Header(), responseHeader)
	w.WriteHeader(resp.StatusCode)
	if len(responseBody) > 0 {
		_, _ = w.Write(responseBody)
	}
	if err := round.WriteResponse(responsePath, responseBody); err != nil {
		log.Printf("archive raw response: %v", err)
	}
	usage := tokenUsage{}
	if resp.StatusCode < 400 {
		usage = usageFromRawResponse(provider, responseBody, rawBody)
	}
	duration := time.Since(start)
	h.recordAndPrint(round, r, providerName, rawModel, rawStream, resp.StatusCode, duration, usage, "")
	h.writeArchiveMetadata(round, providerName, rawModel, rawStream, resp.StatusCode, duration, usage, responsePath, "", "", "")
}

func (h *Handler) handleBufferedResponse(w http.ResponseWriter, resp *http.Response, round *archive.Round, start time.Time, providerName, model string, stream bool, requestBody map[string]any, r *http.Request) {
	responseBody, readErr := h.readLimitedUpstream(resp.Body)
	if readErr != nil {
		h.writeArchivedError(w, round, r, start, providerName, model, stream, http.StatusBadGateway, readErr.Error())
		return
	}
	var responseHeader http.Header
	responseBody, responseHeader, readErr = h.decodedResponseBodyAndHeader(responseBody, resp.Header)
	if readErr != nil {
		h.writeArchivedError(w, round, r, start, providerName, model, stream, http.StatusBadGateway, readErr.Error())
		return
	}
	copyResponseHeader(w.Header(), responseHeader)
	w.WriteHeader(resp.StatusCode)
	if len(responseBody) > 0 {
		_, _ = w.Write(responseBody)
	}

	usage := tokenUsage{}
	if resp.StatusCode < 400 {
		var payload struct {
			Usage json.RawMessage `json:"usage"`
		}
		if err := json.Unmarshal(responseBody, &payload); err == nil {
			usage, _ = usageFromRaw(payload.Usage)
		}
		if !usage.Known {
			usage = tokenUsage{
				PromptTokens:     estimatePromptTokens(requestBody),
				CompletionTokens: estimateCompletionTokensFromResponse(responseBody),
				Estimated:        true,
				Known:            true,
			}
		}
	}
	responsePath := responseFileName(resp.Header.Get("Content-Type"), false)
	if err := round.WriteResponse(responsePath, responseBody); err != nil {
		log.Printf("archive response: %v", err)
	}
	duration := time.Since(start)
	h.recordAndPrint(round, r, providerName, model, stream, resp.StatusCode, duration, usage, "")
	h.writeArchiveMetadata(round, providerName, model, stream, resp.StatusCode, duration, usage, responsePath, "", "", "")
}

func (h *Handler) handleStreamResponse(w http.ResponseWriter, resp *http.Response, round *archive.Round, start time.Time, providerName, model string, requestBody map[string]any, requestContext context.Context, cancel context.CancelFunc, r *http.Request) {
	copyResponseHeader(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	flusher, _ := w.(http.Flusher)
	archiveWriter, err := round.CreateResponseWriter("response.sse")
	if err != nil {
		log.Printf("archive stream response: %v", err)
	}
	if archiveWriter != nil {
		defer archiveWriter.Close()
	}

	reader := bufio.NewReader(resp.Body)
	accumulator := newOpenAIStreamAccumulator(model)
	maxStream, maxLine := h.streamLimits()
	accumulator.SetMaxContent(maxStream)
	idleTimer, stopIdleTimer := h.startStreamIdleTimer(cancel)
	defer stopIdleTimer()
	var streamErr *streamFail
	var totalBytes int64
	sawTerminal := false
	proto := streamProtocolForPath(r.URL.Path)
	for {
		line, err := readSSELine(reader, maxLine)
		if len(line) > 0 {
			resetStreamIdleTimer(idleTimer, h.cfg.StreamIdleTimeout)
			totalBytes += int64(len(line))
			if totalBytes > maxStream {
				streamErr = h.logStreamIssue(round, providerName, model, "read upstream stream limit", fmt.Errorf("stream exceeds limit of %d bytes", maxStream), requestContext, nil)
				break
			}
			term := parseTerminalSSELine(proto, line)
			if term.Terminal {
				sawTerminal = true
				if fail := streamFailFromTerminal(term); fail != nil {
					streamErr = fail
				}
			}
			accumulator.TrackSSELine(line)
			if _, writeErr := w.Write(line); writeErr != nil {
				streamErr = h.logStreamIssue(round, providerName, model, "write client stream", writeErr, requestContext, nil)
				break
			}
			if archiveWriter != nil {
				if _, writeErr := archiveWriter.Write(line); writeErr != nil {
					h.logStreamIssue(round, providerName, model, "write archive stream", writeErr, nil, nil)
				}
			}
			if flusher != nil {
				flusher.Flush()
			}
			if sawTerminal {
				break
			}
		}
		if err != nil {
			if err != io.EOF {
				streamErr = h.logStreamIssue(round, providerName, model, "read upstream stream", err, requestContext, idleTimer)
			} else if requiresTerminalEvent(proto) && !sawTerminal {
				// 干净 EOF 但未收到协议终止事件:视为上游截断。
				streamErr = h.logStreamIssue(round, providerName, model, "read upstream stream", fmt.Errorf("upstream stream ended without terminal event"), requestContext, idleTimer)
			}
			break
		}
	}

	usage := accumulator.FinalizeUsage(requestBody)
	if accumulator.Truncated() && streamErr == nil {
		streamErr = newStreamFail(streamKindLimitExceeded, fmt.Sprintf("stream full response truncated at %d bytes", maxStream), fmt.Errorf("truncated at %d bytes", maxStream), false)
	}
	fullPath := ""
	if streamErr == nil && !accumulator.Truncated() {
		if fullResponse, err := accumulator.ResponseJSON(); err != nil {
			log.Printf("build stream full response: %v", err)
		} else if err := round.WriteResponse("response.json", append(fullResponse, '\n')); err != nil {
			log.Printf("archive stream full response: %v", err)
		} else {
			fullPath = "response.json"
		}
	}
	duration := time.Since(start)
	errMsg := ""
	if streamErr != nil {
		errMsg = streamErr.Error()
	}
	h.recordAndPrintFail(round, r, providerName, model, true, resp.StatusCode, duration, usage, streamErr)
	h.writeArchiveMetadata(round, providerName, model, true, resp.StatusCode, duration, usage, "response.sse", errMsg, fullPath, outcomeFromStreamFail(streamErr, resp.StatusCode))
}

func trackSSELine(line []byte, usage *tokenUsage, content *strings.Builder) {
	trimmed := strings.TrimSpace(string(line))
	if !strings.HasPrefix(trimmed, "data:") {
		return
	}
	payload := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
	if payload == "" || payload == "[DONE]" {
		return
	}
	var event map[string]any
	if err := json.Unmarshal([]byte(payload), &event); err != nil {
		return
	}
	if parsed, ok := usageFromMap(event["usage"]); ok {
		*usage = parsed
	}
	choices, _ := event["choices"].([]any)
	for _, item := range choices {
		choice, _ := item.(map[string]any)
		if delta, ok := choice["delta"].(map[string]any); ok {
			content.WriteString(flattenValue(delta["content"]))
		}
		if text, ok := choice["text"].(string); ok {
			content.WriteString(text)
		}
	}
}

func parseRawRequestBody(body []byte) (map[string]any, string, bool) {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, "", false
	}
	model, _ := payload["model"].(string)
	stream, _ := payload["stream"].(bool)
	return payload, model, stream
}

func (h *Handler) copyAndArchiveRawStream(w http.ResponseWriter, resp *http.Response, round *archive.Round, providerName string, provider config.Provider, model string, requestBody map[string]any, requestContext context.Context, cancel context.CancelFunc, requestPath string) (tokenUsage, string, *streamFail) {
	archiveWriter, err := round.CreateResponseWriter("response.sse")
	if err != nil {
		log.Printf("archive raw stream response: %v", err)
	}
	if archiveWriter != nil {
		defer archiveWriter.Close()
	}
	var openAIAccumulator *openAIStreamAccumulator
	var responsesAccumulator *responsesStreamAccumulator
	var anthropicAccumulator *anthropicRawStreamAccumulator
	maxStream, maxLine := h.streamLimits()

	// 按入站 path 选择终止语义;Anthropic provider 强制 anthropic 语义。
	proto := streamProtocolForPath(requestPath)
	if provider.Protocol == "anthropic" {
		proto = streamProtoAnthropic
	}

	switch {
	case proto == streamProtoAnthropic || provider.Protocol == "anthropic":
		anthropicAccumulator = newAnthropicRawStreamAccumulator(model)
		anthropicAccumulator.SetMaxContent(maxStream)
	case proto == streamProtoResponses:
		responsesAccumulator = newResponsesStreamAccumulator(model)
		responsesAccumulator.SetMaxContent(maxStream)
	default:
		openAIAccumulator = newOpenAIStreamAccumulator(model)
		openAIAccumulator.SetMaxContent(maxStream)
	}

	reader := bufio.NewReader(resp.Body)
	idleTimer, stopIdleTimer := h.startStreamIdleTimer(cancel)
	defer stopIdleTimer()
	var streamErr *streamFail
	var totalBytes int64
	sawTerminal := false
	for {
		line, err := readSSELine(reader, maxLine)
		if len(line) > 0 {
			resetStreamIdleTimer(idleTimer, h.cfg.StreamIdleTimeout)
			totalBytes += int64(len(line))
			if totalBytes > maxStream {
				streamErr = h.logStreamIssue(round, providerName, model, "read raw stream limit", fmt.Errorf("stream exceeds limit of %d bytes", maxStream), requestContext, nil)
				break
			}
			term := parseTerminalSSELine(proto, line)
			if term.Terminal {
				sawTerminal = true
				if fail := streamFailFromTerminal(term); fail != nil {
					streamErr = fail
				}
			}
			if openAIAccumulator != nil {
				openAIAccumulator.TrackSSELine(line)
			}
			if responsesAccumulator != nil {
				responsesAccumulator.TrackSSELine(line)
			}
			if anthropicAccumulator != nil {
				anthropicAccumulator.TrackSSELine(line)
			}
			if _, writeErr := w.Write(line); writeErr != nil {
				streamErr = h.logStreamIssue(round, providerName, model, "write raw stream client", writeErr, requestContext, nil)
				break
			}
			if archiveWriter != nil {
				if _, writeErr := archiveWriter.Write(line); writeErr != nil {
					h.logStreamIssue(round, providerName, model, "write raw stream archive", writeErr, nil, nil)
				}
			}
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
			// 终止事件已写出后立即结束,避免 idle timeout 或后续脏数据覆盖结果。
			if sawTerminal {
				break
			}
		}
		if err != nil {
			if err != io.EOF {
				streamErr = h.logStreamIssue(round, providerName, model, "read raw stream", err, requestContext, idleTimer)
			} else if requiresTerminalEvent(proto) && !sawTerminal {
				streamErr = h.logStreamIssue(round, providerName, model, "read raw stream", fmt.Errorf("upstream stream ended without terminal event"), requestContext, idleTimer)
			}
			break
		}
	}

	if openAIAccumulator != nil {
		usage := openAIAccumulator.FinalizeUsage(requestBody)
		if openAIAccumulator.Truncated() && streamErr == nil {
			streamErr = newStreamFail(streamKindLimitExceeded, fmt.Sprintf("stream full response truncated at %d bytes", maxStream), fmt.Errorf("truncated at %d bytes", maxStream), false)
		}
		fullPath := ""
		if streamErr == nil && !openAIAccumulator.Truncated() && proto == streamProtoChatCompletions {
			if fullResponse, err := openAIAccumulator.ResponseJSON(); err != nil {
				log.Printf("build raw stream full response: %v", err)
			} else if err := round.WriteResponse("response.json", append(fullResponse, '\n')); err != nil {
				log.Printf("archive raw stream full response: %v", err)
			} else {
				fullPath = "response.json"
			}
		}
		return usage, fullPath, streamErr
	}
	if responsesAccumulator != nil {
		usage := responsesAccumulator.FinalizeUsage(requestBody)
		if responsesAccumulator.Truncated() && streamErr == nil {
			streamErr = newStreamFail(streamKindLimitExceeded, fmt.Sprintf("stream full response truncated at %d bytes", maxStream), fmt.Errorf("truncated at %d bytes", maxStream), false)
		}
		return usage, "", streamErr
	}
	usage := anthropicAccumulator.FinalizeUsage(requestBody)
	if anthropicAccumulator.Truncated() && streamErr == nil {
		streamErr = newStreamFail(streamKindLimitExceeded, fmt.Sprintf("stream full response truncated at %d bytes", maxStream), fmt.Errorf("truncated at %d bytes", maxStream), false)
	}
	fullPath := ""
	if streamErr == nil && !anthropicAccumulator.Truncated() {
		if fullResponse, err := anthropicAccumulator.ResponseJSON(usage); err != nil {
			log.Printf("build anthropic raw stream full response: %v", err)
		} else if err := round.WriteResponse("response.json", append(fullResponse, '\n')); err != nil {
			log.Printf("archive anthropic raw stream full response: %v", err)
		} else {
			fullPath = "response.json"
		}
	}
	return usage, fullPath, streamErr
}

type upstreamResult struct {
	ProviderName string
	Provider     config.Provider
	Response     *http.Response
	Duration     time.Duration
	Cancel       context.CancelFunc
}

func (h *Handler) doUpstream(r *http.Request, round *archive.Round, providerName string, provider config.Provider, body []byte, bodyBytes int, stream bool) (upstreamResult, error) {
	return h.doUpstreamPath(r, round, providerName, provider, body, bodyBytes, stream, r.URL.Path, r.URL.RawQuery, r.Method)
}

func (h *Handler) doUpstreamPath(r *http.Request, round *archive.Round, providerName string, provider config.Provider, body []byte, bodyBytes int, stream bool, path, rawQuery, method string) (upstreamResult, error) {
	// catalog RouteOwner 是唯一的上游；任何错误都直接返回给客户端，不会尝试其他 provider。
	ctx, cancel := h.upstreamContext(r.Context(), stream)
	req, err := h.newUpstreamRequestForPath(ctx, r, provider, body, path, rawQuery, method, stream)
	if err != nil {
		if cancel != nil {
			cancel()
		}
		return upstreamResult{}, err
	}
	h.archiveAndLogUpstreamRequest(round, r, providerName, provider, req, bodyBytes)
	h.debugfRound(round, r, "upstream request provider=%s protocol=%s method=%s url=%s body_bytes=%d",
		providerName,
		provider.Protocol,
		req.Method,
		req.URL.String(),
		bodyBytes,
	)

	upstreamStart := time.Now()
	resp, err := h.client.Do(req)
	duration := time.Since(upstreamStart)
	if round != nil {
		round.SetUpstreamDuration(duration)
	}
	h.archiveAndLogUpstreamResponse(round, r, providerName, provider, resp, duration, err)
	if resp != nil && resp.StatusCode >= 400 {
		h.metricsRegistry.RecordUpstreamError(providerName, resp.StatusCode)
	}
	if err != nil {
		if cancel != nil {
			cancel()
		}
		h.metricsRegistry.RecordUpstreamAttempt(providerName, duration, metrics.AttemptHeader)
		h.metricsRegistry.RecordUpstreamError(providerName, -1)
		return upstreamResult{}, err
	}

	// 流式请求在写出首包前探测完整首行；失败直接返回，绝不切换 RouteOwner。
	if stream && resp.StatusCode < 400 {
		_, maxLine := h.streamLimits()
		primed, peekErr := primeStreamBody(resp, h.cfg.StreamIdleTimeout, maxLine)
		duration = time.Since(upstreamStart)
		if peekErr != nil {
			_ = resp.Body.Close()
			if cancel != nil {
				cancel()
			}
			h.metricsRegistry.RecordUpstreamAttempt(providerName, duration, metrics.AttemptFirstEvent)
			h.metricsRegistry.RecordUpstreamError(providerName, -1)
			return upstreamResult{}, peekErr
		}
		resp = primed
	}

	// 流式成功: first_event;非流式: header。
	kind := metrics.AttemptHeader
	if stream {
		kind = metrics.AttemptFirstEvent
	}
	h.metricsRegistry.RecordUpstreamAttempt(providerName, duration, kind)
	return upstreamResult{ProviderName: providerName, Provider: provider, Response: resp, Duration: duration, Cancel: cancel}, nil
}

// primeStreamBody 在 timeout 内读取上游流式响应的首行(必须含 \n),成功后把字节回填到 Body。
// 复用 readSSELine 施加单行上限; partial EOF(无换行)视为首事件失败。
// timeout<=0 时使用 30s 兜底,避免永久阻塞。
func primeStreamBody(resp *http.Response, timeout time.Duration, maxLine int64) (*http.Response, error) {
	if resp == nil || resp.Body == nil {
		return resp, fmt.Errorf("empty upstream stream body")
	}
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	if maxLine <= 0 {
		maxLine = config.DefaultMaxSSELineBytes
	}

	type peekResult struct {
		prefix []byte
		err    error
	}
	ch := make(chan peekResult, 1)
	go func() {
		reader := bufio.NewReader(resp.Body)
		line, err := readSSELine(reader, maxLine)
		// 必须读到完整换行; partial EOF 不视为成功。
		if err != nil {
			ch <- peekResult{prefix: line, err: err}
			return
		}
		if len(line) == 0 || line[len(line)-1] != '\n' {
			ch <- peekResult{err: fmt.Errorf("upstream stream closed before complete first SSE line")}
			return
		}
		// 把 reader 缓冲中已预读的内容拼进 prefix,再接回原始 Body。
		var extra []byte
		if buffered := reader.Buffered(); buffered > 0 {
			extra = make([]byte, buffered)
			_, _ = io.ReadFull(reader, extra)
		}
		prefix := append(append([]byte{}, line...), extra...)
		ch <- peekResult{prefix: prefix}
	}()

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case res := <-ch:
		if res.err != nil {
			// 超限/EOF/其它读错误:不接受 partial 数据。
			if res.err == io.EOF {
				return nil, fmt.Errorf("upstream stream closed before complete first SSE line")
			}
			return nil, res.err
		}
		if len(res.prefix) == 0 {
			return nil, fmt.Errorf("upstream stream closed before first SSE line")
		}
		resp.Body = &prefixReadCloser{prefix: res.prefix, rest: resp.Body}
		return resp, nil
	case <-timer.C:
		_ = resp.Body.Close()
		return nil, fmt.Errorf("upstream stream first byte timeout after %s", timeout.Truncate(time.Millisecond))
	}
}

type prefixReadCloser struct {
	prefix []byte
	rest   io.ReadCloser
}

func (p *prefixReadCloser) Read(b []byte) (int, error) {
	if len(p.prefix) > 0 {
		n := copy(b, p.prefix)
		p.prefix = p.prefix[n:]
		return n, nil
	}
	return p.rest.Read(b)
}

func (p *prefixReadCloser) Close() error {
	if p.rest == nil {
		return nil
	}
	return p.rest.Close()
}

func providerSupportsInboundPath(provider config.Provider, path string) bool {
	return config.ProviderSupportsInboundPath(provider, path)
}

func (h *Handler) upstreamContext(parent context.Context, stream bool) (context.Context, context.CancelFunc) {
	if stream {
		return context.WithCancel(parent)
	}
	if h.cfg.RequestTimeout > 0 {
		return context.WithTimeout(parent, h.cfg.RequestTimeout)
	}
	return parent, nil
}

func (h *Handler) newUpstreamRequest(ctx context.Context, r *http.Request, provider config.Provider, body []byte) (*http.Request, error) {
	return h.newUpstreamRequestForPath(ctx, r, provider, body, r.URL.Path, r.URL.RawQuery, r.Method, false)
}

// newUpstreamRequestForPath 按指定上游 path 构建请求,用于协议转换时改写目标路径。
// 请求头使用 upstream protocol allowlist 构造,不得先复制全部入站 header 再 blocklist 删除。
func (h *Handler) newUpstreamRequestForPath(ctx context.Context, r *http.Request, provider config.Provider, body []byte, path, rawQuery, method string, stream bool) (*http.Request, error) {
	incoming := *r.URL
	incoming.Path = path
	incoming.RawQuery = rawQuery
	target, err := buildUpstreamURL(provider.BaseURL, &incoming)
	if err != nil {
		return nil, err
	}
	if method == "" {
		method = r.Method
	}
	req, err := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	applyUpstreamHeaders(req, r, provider, body, stream)
	req.ContentLength = int64(len(body))
	return req, nil
}

// applyUpstreamHeaders 按 upstream protocol 重建认证与版本头,仅透传白名单语义 header。
// 允许透传: Content-Type、Accept、X-Request-ID(已校验)。其它 header 不从客户端复制。
func applyUpstreamHeaders(req *http.Request, client *http.Request, provider config.Provider, body []byte, stream bool) {
	if req == nil {
		return
	}
	// 从干净 header 开始。
	req.Header = make(http.Header)

	contentType := ""
	accept := ""
	requestID := ""
	if client != nil {
		contentType = strings.TrimSpace(client.Header.Get("Content-Type"))
		accept = strings.TrimSpace(client.Header.Get("Accept"))
		requestID = strings.TrimSpace(client.Header.Get("X-Request-ID"))
	}
	if contentType == "" && len(body) > 0 {
		contentType = "application/json"
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if requestID != "" && isSafeRequestID(requestID) {
		req.Header.Set("X-Request-ID", requestID)
	}

	switch strings.ToLower(strings.TrimSpace(provider.Protocol)) {
	case "anthropic":
		// Anthropic-Version 由 ai-proxy 固定生成,不信任客户端。
		req.Header.Set("Anthropic-Version", "2023-06-01")
		if stream {
			req.Header.Set("Accept", "text/event-stream")
		} else if accept != "" && isSafeAccept(accept) {
			req.Header.Set("Accept", accept)
		} else {
			req.Header.Set("Accept", "application/json")
		}
		if strings.TrimSpace(provider.APIKey) != "" {
			req.Header.Set("X-API-Key", provider.APIKey)
		}
	default: // openai
		if stream {
			req.Header.Set("Accept", "text/event-stream")
		} else if accept != "" && isSafeAccept(accept) {
			req.Header.Set("Accept", accept)
		} else if accept == "" {
			req.Header.Set("Accept", "application/json")
		}
		if strings.TrimSpace(provider.APIKey) != "" {
			req.Header.Set("Authorization", "Bearer "+provider.APIKey)
		}
	}
}

func isSafeRequestID(id string) bool {
	if id == "" || len(id) > 128 {
		return false
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-' || r == '_' || r == '.':
		default:
			return false
		}
	}
	return true
}

func isSafeAccept(accept string) bool {
	// 仅允许常见 JSON/SSE accept,避免透传任意客户端值。
	lower := strings.ToLower(accept)
	return strings.Contains(lower, "application/json") ||
		strings.Contains(lower, "text/event-stream") ||
		strings.Contains(lower, "*/*")
}

func acceptsEventStream(header http.Header) bool {
	return strings.Contains(strings.ToLower(header.Get("Accept")), "text/event-stream")
}

func buildUpstreamURL(base string, incoming *url.URL) (string, error) {
	parsed, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	parsed.Path = joinUpstreamPath(parsed.Path, incoming.Path)
	query := incoming.Query()
	query.Del("provider")
	parsed.RawQuery = query.Encode()
	return parsed.String(), nil
}

func joinUpstreamPath(basePath, incomingPath string) string {
	basePath = strings.TrimRight(basePath, "/")
	if basePath == "" {
		if incomingPath == "" {
			return "/"
		}
		return incomingPath
	}
	if incomingPath == "" || incomingPath == "/" {
		return basePath
	}
	if strings.HasSuffix(basePath, "/v1") && (incomingPath == "/v1" || strings.HasPrefix(incomingPath, "/v1/")) {
		return basePath + strings.TrimPrefix(incomingPath, "/v1")
	}
	return basePath + incomingPath
}

// resolveTransportPlan 是执行端点的唯一路由入口:解析 ResolvedModelRoute + TransportPlan。
// 已禁用 X-AI-Provider / ?provider= / provider/model 显式选择;不允许修改 RouteOwner。
func (h *Handler) resolveTransportPlan(r *http.Request, model string) (TransportPlan, *APIError) {
	method := ""
	path := ""
	if r != nil {
		method = r.Method
		if r.URL != nil {
			path = r.URL.Path
		}
	}
	return ResolveTransportPlan(h.cfg, method, path, model)
}

// resolveProviderName 保留为兼容包装:返回 RouteOwner 与 operation。
// 新代码应使用 resolveTransportPlan。
func (h *Handler) resolveProviderName(r *http.Request, model string) (string, string, *APIError) {
	plan, apiErr := h.resolveTransportPlan(r, model)
	if apiErr != nil {
		return "", apiErr.Operation, apiErr
	}
	return plan.RouteOwner, plan.Operation, nil
}

func providerMatchesModel(name string, provider config.Provider, model string) bool {
	return config.ProviderMatchesModel(name, provider, model)
}

func matchModelPattern(model, pattern string) bool {
	return config.MatchModelPattern(model, pattern)
}

func copyHeader(dst, src http.Header) {
	for key, values := range src {
		for _, value := range values {
			dst.Add(key, value)
		}
	}
}

// decodedResponseBodyAndHeader 在需要时解压 gzip,并使用配置的上游响应上限。
// 解压失败或超限返回 error,调用方应在写响应头前处理。
func (h *Handler) decodedResponseBodyAndHeader(body []byte, header http.Header) ([]byte, http.Header, error) {
	decodedHeader := header.Clone()
	if !strings.EqualFold(strings.TrimSpace(decodedHeader.Get("Content-Encoding")), "gzip") {
		return body, decodedHeader, nil
	}
	reader, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		return nil, decodedHeader, fmt.Errorf("gzip decode failed: %w", err)
	}
	defer reader.Close()
	limit := h.cfg.MaxUpstreamResponseBytes
	if limit <= 0 {
		limit = config.DefaultMaxUpstreamResponseBytes
	}
	limited := io.LimitReader(reader, limit+1)
	decodedBody, err := io.ReadAll(limited)
	if err != nil {
		return nil, decodedHeader, fmt.Errorf("gzip decode read failed: %w", err)
	}
	if int64(len(decodedBody)) > limit {
		return nil, decodedHeader, fmt.Errorf("decompressed upstream response exceeds limit of %d bytes", limit)
	}
	decodedHeader.Del("Content-Encoding")
	decodedHeader.Del("Content-Length")
	return decodedBody, decodedHeader, nil
}

// hopByHopHeaders 是 RFC 9110 定义的标准 hop-by-hop 头。
var hopByHopHeaders = []string{
	"Connection",
	"Keep-Alive",
	"Proxy-Authenticate",
	"Proxy-Authorization",
	"Proxy-Connection", // 非标准但旧式代理常见
	"TE",
	"Trailer",
	"Transfer-Encoding",
	"Upgrade",
}

// removeHopByHop 删除标准 hop-by-hop 头,以及 Connection 中动态列出的扩展头。
// 请求与响应方向均应调用,避免双重编码/错误连接复用等问题。
func removeHopByHop(header http.Header) {
	if header == nil {
		return
	}
	// 先解析 Connection 中列出的动态 header 名并删除。
	for _, connVal := range header.Values("Connection") {
		for _, token := range strings.Split(connVal, ",") {
			name := textproto.CanonicalMIMEHeaderKey(strings.TrimSpace(token))
			if name == "" || strings.EqualFold(name, "close") || strings.EqualFold(name, "keep-alive") {
				// close/keep-alive 是连接指令,不是要额外删除的扩展头名(标准列表已含 Keep-Alive)。
				continue
			}
			header.Del(name)
		}
	}
	for _, key := range hopByHopHeaders {
		header.Del(key)
	}
}

// copyResponseHeader 复制上游响应头并剥离 hop-by-hop,供回写客户端。
func copyResponseHeader(dst, src http.Header) {
	copyHeader(dst, src)
	removeHopByHop(dst)
}

func (h *Handler) recordAndPrint(round *archive.Round, r *http.Request, provider, model string, stream bool, status int, duration time.Duration, tok tokenUsage, errMessage string) {
	h.recordAndPrintFail(round, r, provider, model, stream, status, duration, tok, streamFailFromMessage(errMessage))
}

func (h *Handler) recordAndPrintFail(round *archive.Round, r *http.Request, provider, model string, stream bool, status int, duration time.Duration, tok tokenUsage, fail *streamFail) {
	outcome := outcomeFromStreamFail(fail, status)
	errMessage := ""
	errorCode := ""
	if fail != nil {
		errMessage = fail.Error()
		if fail.Kind != "" {
			errorCode = string(fail.Kind)
		}
	}
	clientEndpoint, upstreamProtocol, upstreamEndpoint, conversionMode := "", "", "", ""
	if round != nil {
		clientEndpoint = round.ClientEndpoint
		upstreamProtocol = round.UpstreamProtocol
		upstreamEndpoint = round.UpstreamEndpoint
		conversionMode = round.ConversionMode
	}
	// 结算 DuckDB usage event(ServeHTTP 已 Start)。
	eventID := ""
	if r != nil {
		eventID = usageEventIDFromContext(r.Context())
	}
	if h.completeUsage(r, eventID, provider, model, stream, status, duration, tok, outcome, errorCode, round) && h.metricsRegistry != nil && r != nil {
		h.metricsRegistry.RecordClientUsage(clientauth.ClientIdentityFromContext(r.Context()).KeyID, tok.PromptTokens, tok.CompletionTokens)
	}

	route := RouteLabel(r)
	h.metricsRegistry.RecordRequestPlan(provider, model, route, status, duration, outcome,
		clientEndpoint, upstreamProtocol, upstreamEndpoint, conversionMode)
	// 仅流式 midflight 上游读失败计入 provider 错误率,避免非流式状态码/本地错误重复计数。
	if shouldCountUpstreamError(fail, stream) {
		h.metricsRegistry.RecordUpstreamError(provider, -2) // -2 = stream_midflight
	}
	h.metricsRegistry.RecordTokens(provider, model, tok.PromptTokens, tok.CompletionTokens, tok.CachedInputTokens, tok.CacheCreationInputTokens)
	h.printSummary(round, provider, model, stream, status, duration, tok, errMessage)
}

func (h *Handler) writeArchiveMetadata(round *archive.Round, provider, model string, stream bool, status int, duration time.Duration, usage tokenUsage, responsePath, message, fullResponsePath, outcome string) {
	stableHash, fingerprint, drift, driftCount := h.driftInfo(round)
	fullContent := round == nil || round.FullContent()
	if outcome == "" {
		outcome = outcomeFromStreamFail(streamFailFromMessage(message), status)
	}
	// 转换路径中途失败时 outcome 必须显式为 conversion,避免伪造成功终止。
	if outcome == "" || outcome == "error" {
		if round != nil && round.ConversionMode != "" && round.ConversionMode != TransportModeNative && message != "" {
			if strings.Contains(message, "conversion") || strings.Contains(message, ErrorCodeConversionUnsupported) {
				outcome = "conversion"
			}
		}
	}
	meta := archive.Metadata{
		FinishedAt:               time.Now(),
		Provider:                 provider,
		Model:                    model,
		StablePrefixHash:         stableHash,
		RequestFingerprint:       fingerprint,
		StablePrefixDrift:        drift,
		StablePrefixDriftCount:   driftCount,
		Stream:                   stream,
		HTTPStatus:               status,
		Outcome:                  outcome,
		DurationMS:               duration.Milliseconds(),
		InputTokens:              usage.PromptTokens,
		OutputTokens:             usage.CompletionTokens,
		TotalTokens:              usage.PromptTokens + usage.CompletionTokens,
		CachedInputTokens:        usage.CachedInputTokens,
		CacheCreationInputTokens: usage.CacheCreationInputTokens,
		CacheHitRate:             usage.CacheHitRate(),
		Estimated:                usage.Estimated,
		FullContentEnabled:       fullContent,
		Error:                    message,
	}
	if round != nil {
		meta.RequestID = round.RequestID
		meta.EventID = round.RequestID
		meta.APIKeyID = round.APIKeyID
		meta.Operation = round.Operation
		meta.ClientEndpoint = round.ClientEndpoint
		meta.ClientProtocol = round.ClientProtocol
		meta.UpstreamProtocol = round.UpstreamProtocol
		meta.UpstreamEndpoint = round.UpstreamEndpoint
		meta.ConversionMode = round.ConversionMode
	}
	if round != nil {
		if round.HasFile("request.meta.json") {
			meta.RequestMetaPath = "request.meta.json"
		}
		if round.HasFile("request.json") {
			meta.RequestPath = "request.json"
		}
		if round.HasFile("upstream_request.json") {
			meta.UpstreamRequestPath = "upstream_request.json"
		}
		if round.HasFile("upstream_response.json") {
			meta.UpstreamResponsePath = "upstream_response.json"
		}
		if responsePath != "" && round.HasFile(responsePath) {
			meta.ResponsePath = responsePath
		}
		if fullResponsePath != "" && round.HasFile(fullResponsePath) {
			meta.FullResponsePath = fullResponsePath
		}
		if err := round.WriteMetadata(meta); err != nil {
			log.Printf("archive metadata: %v", err)
		}
	}
}

func (h *Handler) driftInfo(round *archive.Round) (stableHash, fingerprint string, drift bool, driftCount int) {
	if round == nil || h.driftTracker == nil {
		return "", "", false, 0
	}
	stableHash = round.StablePrefixHash
	fingerprint = round.RequestFingerprint
	if stableHash == "" {
		return
	}
	drift, driftCount = h.driftTracker.Observe(stableHash)
	return
}

func responseFileName(contentType string, stream bool) string {
	if stream {
		return "response.sse"
	}
	contentType = strings.ToLower(contentType)
	switch {
	case strings.Contains(contentType, "json"):
		return "response.json"
	case strings.Contains(contentType, "text"):
		return "response.txt"
	default:
		return "response.bin"
	}
}

func (h *Handler) printSummary(round *archive.Round, provider, model string, stream bool, status int, duration time.Duration, usage tokenUsage, errMessage string) {
	level := slog.LevelInfo
	label := "ok"
	clientCanceled := isClientCanceledStreamIssue(errMessage)
	if status >= 500 {
		level = slog.LevelError
		label = "error"
	} else if status >= 400 || (errMessage != "" && !clientCanceled) || usage.Estimated {
		level = slog.LevelWarn
		if status >= 400 || (errMessage != "" && !clientCanceled) {
			label = "warn"
		} else {
			label = "estimated"
		}
	} else if clientCanceled {
		label = "canceled"
	}
	roundID := roundIDValue(round)
	attrs := []any{
		slog.String("label", label),
		slog.String("provider", provider),
		slog.String("model", model),
		slog.Int("round", roundID),
		slog.Int("status", status),
		slog.Bool("stream", stream),
		slog.Duration("duration", duration.Truncate(time.Millisecond)),
		slog.Int("input_tokens", usage.PromptTokens),
		slog.Int("output_tokens", usage.CompletionTokens),
		slog.Int("total_tokens", usage.PromptTokens+usage.CompletionTokens),
		slog.Int("cached_input_tokens", usage.CachedInputTokens),
		slog.Int("cache_creation_input_tokens", usage.CacheCreationInputTokens),
		slog.Float64("cache_hit_rate", usage.CacheHitRate()),
		slog.Bool("estimated", usage.Estimated),
	}
	if errMessage != "" {
		key := "error"
		if clientCanceled {
			key = "reason"
		}
		attrs = append(attrs, slog.String(key, errMessage))
	}
	slog.LogAttrs(context.Background(), level, "ai-proxy", toAttrs(attrs)...)
}

func roundIDValue(round *archive.Round) int {
	if round == nil {
		return 0
	}
	return round.ID
}

type streamIdleTimer struct {
	timer   *time.Timer
	timeout time.Duration
	expired atomic.Bool
}

func (h *Handler) startStreamIdleTimer(cancel context.CancelFunc) (*streamIdleTimer, func()) {
	if cancel == nil || h.cfg.StreamIdleTimeout <= 0 {
		return nil, func() {}
	}
	idle := &streamIdleTimer{timeout: h.cfg.StreamIdleTimeout}
	idle.timer = time.AfterFunc(h.cfg.StreamIdleTimeout, func() {
		idle.expired.Store(true)
		cancel()
	})
	return idle, func() {
		idle.timer.Stop()
	}
}

func resetStreamIdleTimer(idle *streamIdleTimer, timeout time.Duration) {
	if idle == nil || idle.timer == nil || timeout <= 0 {
		return
	}
	idle.expired.Store(false)
	idle.timer.Reset(idle.timeout)
}

// logStreamFail 直接记录已构造的 streamFail,不再二次推断 kind。
// 用于在错误产生点已明确 protocol/conversion 等类型的场景。
func (h *Handler) logStreamFail(round *archive.Round, provider, model string, fail *streamFail) {
	if fail == nil {
		return
	}
	level := slog.LevelWarn
	if fail.Kind == streamKindClientCanceled {
		level = slog.LevelInfo
	}
	slog.LogAttrs(context.Background(), level, "stream issue",
		slog.String("event", "STREAM"),
		slog.Int("round", roundID(round)),
		slog.String("provider", provider),
		slog.String("model", model),
		slog.String("outcome", string(fail.Kind)),
		slog.String("message", fail.Error()),
	)
}

// logStreamIssue 记录流式问题并返回 typed streamFail。
// kind 由 operation + 错误上下文决定,不再依赖最终字符串匹配。
func (h *Handler) logStreamIssue(round *archive.Round, provider, model, operation string, err error, requestContext context.Context, idleTimer *streamIdleTimer) *streamFail {
	if err == nil {
		return nil
	}
	kind := streamKindError
	countUpstream := false
	level := slog.LevelWarn
	message := fmt.Sprintf("%s: %v", operation, err)
	errText := strings.ToLower(err.Error())
	op := strings.ToLower(operation)

	clientCanceled := errors.Is(err, context.Canceled) ||
		(requestContext != nil && errors.Is(requestContext.Err(), context.Canceled))
	deadlineExceeded := errors.Is(err, context.DeadlineExceeded) ||
		(requestContext != nil && errors.Is(requestContext.Err(), context.DeadlineExceeded))

	switch {
	case idleTimer != nil && idleTimer.expired.Load():
		kind = streamKindIdleTimeout
		countUpstream = true
		message = fmt.Sprintf("%s: stream idle timeout exceeded after %s", operation, idleTimer.timeout.Truncate(time.Millisecond))
	case clientCanceled:
		kind = streamKindClientCanceled
		level = slog.LevelInfo
		// 保持与历史测试/日志一致的消息格式。
		message = fmt.Sprintf("%s: client canceled downstream request", operation)
	case deadlineExceeded:
		kind = streamKindError
		message = fmt.Sprintf("%s: downstream request deadline exceeded", operation)
	case strings.Contains(op, "write") && (strings.Contains(op, "client") || strings.Contains(op, "downstream")):
		kind = streamKindClientWrite
	case strings.Contains(op, "convert") || strings.Contains(op, "conversion"):
		kind = streamKindConversion
	case strings.Contains(op, "protocol") || strings.Contains(errText, "invalid json") || strings.Contains(errText, "invalid sse") || strings.Contains(errText, "unmarshal"):
		kind = streamKindProtocol
		countUpstream = true
	case strings.Contains(op, "limit") || strings.Contains(errText, "exceeds limit") || strings.Contains(errText, "truncated"):
		kind = streamKindLimitExceeded
		countUpstream = false
	case strings.Contains(op, "read") && (strings.Contains(op, "upstream") || strings.Contains(op, "stream") || strings.Contains(op, "raw")):
		kind = streamKindUpstreamTrunc
		countUpstream = true
	default:
		if strings.Contains(op, "upstream") || strings.Contains(op, "read") {
			kind = streamKindUpstreamTrunc
			countUpstream = true
		}
	}

	slog.LogAttrs(context.Background(), level, "stream issue",
		slog.String("event", "STREAM"),
		slog.Int("round", roundID(round)),
		slog.String("provider", provider),
		slog.String("model", model),
		slog.String("outcome", string(kind)),
		slog.String("message", message),
	)
	return newStreamFail(kind, message, err, countUpstream)
}

func isClientCanceledStreamIssue(message string) bool {
	return strings.Contains(message, "client canceled downstream request")
}

func roundID(round *archive.Round) int {
	if round == nil {
		return 0
	}
	return round.ID
}
