package proxy

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/subtle"
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
	"sync/atomic"
	"time"

	"ai-proxy/internal/archive"
	"ai-proxy/internal/config"
	"ai-proxy/internal/metrics"
	"ai-proxy/internal/stats"
)

type Handler struct {
	cfg                 config.Config
	recorder            stats.Recorder
	interactionRecorder *archive.Recorder
	metricsRegistry     *metrics.Registry
	driftTracker        *FingerprintDriftTracker
	client              *http.Client
}

func NewHandler(cfg config.Config, recorder stats.Recorder, interactionRecorder *archive.Recorder, metricsRegistry *metrics.Registry) *Handler {
	return &Handler{
		cfg:                 cfg,
		recorder:            recorder,
		interactionRecorder: interactionRecorder,
		metricsRegistry:     metricsRegistry,
		driftTracker:        NewFingerprintDriftTracker(2),
		client:              newHTTPClient(cfg.RequestTimeout),
	}
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
	// 入站认证:配置了 InboundAPIKey 时,所有 /v1/* 代理入口均需校验。
	if !h.authorizeInbound(r) {
		http.Error(w, "unauthorized: missing or invalid inbound api key", http.StatusUnauthorized)
		return
	}
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

// authorizeInbound 校验入站 API Key。
// 未配置 InboundAPIKey 时放行(仅 loopback 监听时允许此状态,由 config 校验保证)。
// 客户端可通过 Authorization: Bearer <key> 或 X-API-Key 提交。
func (h *Handler) authorizeInbound(r *http.Request) bool {
	expected := strings.TrimSpace(h.cfg.InboundAPIKey)
	if expected == "" {
		return true
	}
	if key := strings.TrimSpace(r.Header.Get("X-API-Key")); key != "" {
		return subtle.ConstantTimeCompare([]byte(key), []byte(expected)) == 1
	}
	auth := strings.TrimSpace(r.Header.Get("Authorization"))
	if auth == "" {
		return false
	}
	const bearer = "Bearer "
	if len(auth) > len(bearer) && strings.EqualFold(auth[:len(bearer)], bearer) {
		token := strings.TrimSpace(auth[len(bearer):])
		return subtle.ConstantTimeCompare([]byte(token), []byte(expected)) == 1
	}
	return false
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
	bodyBytes, err := h.readLimitedBody(w, r)
	if err != nil {
		status := http.StatusBadRequest
		if isRequestTooLarge(err) {
			status = http.StatusRequestEntityTooLarge
		}
		http.Error(w, err.Error(), status)
		return
	}
	defer r.Body.Close()

	round, err := h.startRound()
	if err != nil {
		http.Error(w, "start interaction archive failed", http.StatusInternalServerError)
		return
	}
	if round != nil {
		// panic 或遗漏路径也释放 active;WriteMetadata/Abort 幂等。
		defer round.Abort()
	}
	round.SetRequestID(requestID)
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
	providerName, err := h.resolveProviderName(r, model)
	if err != nil {
		h.writeArchivedError(w, round, r, start, "", model, stream, http.StatusBadRequest, err.Error())
		return
	}
	provider, ok := h.cfg.Providers[providerName]
	if !ok {
		h.writeArchivedError(w, round, r, start, providerName, model, stream, http.StatusBadRequest, fmt.Sprintf("provider %q is not configured", providerName))
		return
	}
	// model 路由仅使用 body.model,不再剥离 provider/model 前缀。
	h.archiveAndLogProviderSelection(round, r, providerName, provider, model, stream)

	if provider.Protocol == "anthropic" {
		h.handleAnthropicChatCompletions(w, r, round, start, providerName, provider, bodyBytes, body, model, stream)
		return
	}

	result, err := h.doUpstreamWithFallback(r, round, providerName, provider, bodyBytes, len(bodyBytes), stream)
	if err != nil {
		h.writeArchivedError(w, round, r, start, providerName, model, stream, http.StatusBadGateway, err.Error())
		return
	}
	resp := result.Response
	providerName = result.ProviderName
	provider = result.Provider
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
	http.Error(w, message, status)
	responseBody := []byte(message + "\n")
	if err := round.WriteResponse("response.txt", responseBody); err != nil {
		log.Printf("archive error response: %v", err)
	}
	duration := time.Since(start)
	usage := tokenUsage{}
	h.recordAndPrint(round, r, provider, model, stream, status, duration, usage, message)
	h.writeArchiveMetadata(round, provider, model, stream, status, duration, usage, "response.txt", message, "", "")
}

func (h *Handler) forwardRaw(w http.ResponseWriter, r *http.Request, requestID string) {
	start := time.Now()
	body, err := h.readLimitedBody(w, r)
	if err != nil {
		status := http.StatusBadRequest
		if isRequestTooLarge(err) {
			status = http.StatusRequestEntityTooLarge
		}
		http.Error(w, err.Error(), status)
		return
	}
	defer r.Body.Close()
	rawBody, rawModel, rawStream := parseRawRequestBody(body)
	round, err := h.startRound()
	if err != nil {
		http.Error(w, "start interaction archive failed", http.StatusInternalServerError)
		return
	}
	if round != nil {
		// panic 或遗漏路径也释放 active;WriteMetadata/Abort 幂等。
		defer round.Abort()
	}
	round.SetRequestID(requestID)
	if err := round.WriteRequest(body); err != nil {
		log.Printf("archive raw request: %v", err)
	}
	h.archiveAndLogClientRequest(round, r, len(body))
	providerName, err := h.resolveProviderName(r, rawModel)
	if err != nil {
		h.writeArchivedError(w, round, r, start, "", rawModel, rawStream, http.StatusBadRequest, err.Error())
		return
	}
	provider, ok := h.cfg.Providers[providerName]
	if !ok {
		h.writeArchivedError(w, round, r, start, providerName, rawModel, rawStream, http.StatusBadRequest, fmt.Sprintf("provider %q is not configured", providerName))
		return
	}
	if provider.Protocol == "anthropic" {
		h.writeArchivedError(w, round, r, start, providerName, rawModel, rawStream, http.StatusBadRequest,
			fmt.Sprintf("provider %q uses anthropic protocol; only POST /v1/chat/completions supports OpenAI->Anthropic conversion", providerName))
		return
	}
	h.debugfRound(round, r, "raw proxy client request method=%s path=%s query=%q provider=%s remote=%s body_bytes=%d headers=%s",
		r.Method,
		r.URL.Path,
		r.URL.RawQuery,
		providerName,
		r.RemoteAddr,
		len(body),
		headerSummary(sanitizeHeaders(r.Header)),
	)
	h.archiveAndLogProviderSelection(round, r, providerName, provider, rawModel, rawStream)
	streamRequest := rawStream || acceptsEventStream(r.Header)
	result, err := h.doUpstreamWithFallback(r, round, providerName, provider, body, len(body), streamRequest)
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
		usage, fullPath, streamErr := h.copyAndArchiveRawStream(w, resp, round, providerName, provider, rawModel, rawBody, r.Context(), result.Cancel, r.URL.Path)
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

func (h *Handler) doUpstreamWithFallback(r *http.Request, round *archive.Round, providerName string, provider config.Provider, body []byte, bodyBytes int, stream bool) (upstreamResult, error) {
	return h.doUpstreamWithFallbackPath(r, round, providerName, provider, body, bodyBytes, stream, r.URL.Path, r.URL.RawQuery, r.Method)
}

func (h *Handler) doUpstreamWithFallbackPath(r *http.Request, round *archive.Round, providerName string, provider config.Provider, body []byte, bodyBytes int, stream bool, path, rawQuery, method string) (upstreamResult, error) {
	candidates := h.fallbackCandidates(providerName, provider)
	attempts := make([]fallbackAttemptDebugInfo, 0, len(candidates))
	var lastErr error
	for index, candidateName := range candidates {
		candidate := h.cfg.Providers[candidateName]
		ctx, cancel := h.upstreamContext(r.Context(), stream)
		req, err := h.newUpstreamRequestForPath(ctx, r, candidate, body, path, rawQuery, method, stream)
		if err != nil {
			if cancel != nil {
				cancel()
			}
			lastErr = err
			attempts = append(attempts, fallbackAttemptDebugInfo{
				Provider: candidateName,
				Protocol: candidate.Protocol,
				Error:    err.Error(),
				Fallback: index > 0,
			})
			continue
		}
		h.archiveAndLogUpstreamRequest(round, r, candidateName, candidate, req, bodyBytes)
		h.debugfRound(round, r, "upstream request provider=%s protocol=%s method=%s url=%s body_bytes=%d",
			candidateName,
			candidate.Protocol,
			req.Method,
			req.URL.String(),
			bodyBytes,
		)
		upstreamStart := time.Now()
		resp, err := h.client.Do(req)
		duration := time.Since(upstreamStart)
		h.archiveAndLogUpstreamResponse(round, r, candidateName, candidate, resp, duration, err)
		attempt := fallbackAttemptDebugInfo{
			Provider:   candidateName,
			Protocol:   candidate.Protocol,
			Fallback:   index > 0,
			DurationMS: duration.Milliseconds(),
		}
		if resp != nil {
			attempt.Status = resp.StatusCode
			if resp.StatusCode >= 400 {
				h.metricsRegistry.RecordUpstreamError(candidateName, resp.StatusCode)
			}
		}
		if err != nil {
			if cancel != nil {
				cancel()
			}
			lastErr = err
			attempt.Error = err.Error()
			attempts = append(attempts, attempt)
			h.metricsRegistry.RecordUpstreamAttempt(candidateName, duration, metrics.AttemptHeader)
			h.metricsRegistry.RecordUpstreamError(candidateName, -1)
			if index < len(candidates)-1 {
				h.logUpstreamAlert(round, candidateName, candidate.Protocol, 0, duration, err.Error(), true, candidates[index+1])
				h.debugfRound(round, r, "upstream fallback provider=%s reason=error error=%q next=%s", candidateName, err.Error(), candidates[index+1])
				h.metricsRegistry.RecordFallbackAttempt(candidateName, candidates[index+1], "error")
				continue
			}
			h.archiveAndLogFallbackAttempts(round, attempts)
			return upstreamResult{}, err
		}
		attempts = append(attempts, attempt)
		if shouldFallbackStatus(resp.StatusCode) && index < len(candidates)-1 {
			h.metricsRegistry.RecordUpstreamAttempt(candidateName, duration, metrics.AttemptHeader)
			h.logUpstreamAlert(round, candidateName, candidate.Protocol, resp.StatusCode, duration, "", true, candidates[index+1])
			h.debugfRound(round, r, "upstream fallback provider=%s status=%d next=%s", candidateName, resp.StatusCode, candidates[index+1])
			h.metricsRegistry.RecordFallbackAttempt(candidateName, candidates[index+1], statusBucketForFallback(resp.StatusCode))
			_ = resp.Body.Close()
			if cancel != nil {
				cancel()
			}
			continue
		}
		// 流式:在写出首包 SSE 前探测上游首行;断流/超时则可继续 fallback。
		// attempt duration 对成功流式路径包含 first-event 等待,供 p99 SLO 使用。
		if stream && resp.StatusCode < 400 {
			_, maxLine := h.streamLimits()
			// 即使是最后一个候选也做探测以得到 first-event 延迟;仅在有后续候选时 fallback。
			primed, peekErr := primeStreamBody(resp, h.cfg.StreamIdleTimeout, maxLine)
			duration = time.Since(upstreamStart)
			if peekErr != nil {
				lastErr = peekErr
				attempt.Error = peekErr.Error()
				attempt.DurationMS = duration.Milliseconds()
				attempts[len(attempts)-1] = attempt
				h.metricsRegistry.RecordUpstreamAttempt(candidateName, duration, metrics.AttemptFirstEvent)
				h.metricsRegistry.RecordUpstreamError(candidateName, -1)
				if index < len(candidates)-1 {
					h.logUpstreamAlert(round, candidateName, candidate.Protocol, resp.StatusCode, duration, peekErr.Error(), true, candidates[index+1])
					h.debugfRound(round, r, "upstream fallback provider=%s reason=stream_first_byte error=%q next=%s", candidateName, peekErr.Error(), candidates[index+1])
					h.metricsRegistry.RecordFallbackAttempt(candidateName, candidates[index+1], "stream_first_byte")
					_ = resp.Body.Close()
					if cancel != nil {
						cancel()
					}
					continue
				}
				h.archiveAndLogFallbackAttempts(round, attempts)
				return upstreamResult{}, peekErr
			}
			resp = primed
		}
		// 流式成功: first_event;非流式: header。
		kind := metrics.AttemptHeader
		if stream {
			kind = metrics.AttemptFirstEvent
		}
		h.metricsRegistry.RecordUpstreamAttempt(candidateName, duration, kind)
		h.archiveAndLogFallbackAttempts(round, attempts)
		return upstreamResult{ProviderName: candidateName, Provider: candidate, Response: resp, Duration: duration, Cancel: cancel}, nil
	}
	h.archiveAndLogFallbackAttempts(round, attempts)
	if lastErr != nil {
		return upstreamResult{}, lastErr
	}
	return upstreamResult{}, fmt.Errorf("no fallback providers available")
}

// primeStreamBody 在 timeout 内读取上游流式响应的首行(必须含 \n),成功后把字节回填到 Body。
// 复用 readSSELine 施加单行上限; partial EOF(无换行)视为失败以触发 fallback。
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

func statusBucketForFallback(status int) string {
	switch {
	case status >= 500:
		return "5xx"
	case status == 408:
		return "timeout"
	case status == 429:
		return "rate_limit"
	case status >= 400:
		return fmt.Sprintf("%d", status)
	default:
		return "other"
	}
}

func (h *Handler) fallbackCandidates(providerName string, provider config.Provider) []string {
	candidates := []string{providerName}
	seen := map[string]struct{}{providerName: {}}
	for _, fallbackName := range provider.Fallbacks {
		fallbackName = strings.ToLower(strings.TrimSpace(fallbackName))
		if fallbackName == "" {
			continue
		}
		if _, ok := seen[fallbackName]; ok {
			continue
		}
		fallback, ok := h.cfg.Providers[fallbackName]
		if !ok || fallback.Disabled || fallback.Protocol != provider.Protocol {
			continue
		}
		seen[fallbackName] = struct{}{}
		candidates = append(candidates, fallbackName)
	}
	return candidates
}

func shouldFallbackStatus(status int) bool {
	return status == http.StatusRequestTimeout || status == http.StatusTooManyRequests || status >= 500
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
	copyHeader(req.Header, r.Header)
	removeHopByHop(req.Header)
	req.Header.Del("Accept-Encoding")
	req.Header.Del("Content-Length")
	req.Header.Del("X-AI-Provider")
	// 无条件剥离入站认证头,避免把代理密钥泄露给上游;再按 provider 配置注入。
	req.Header.Del("Authorization")
	req.Header.Del("X-API-Key")
	req.Header.Del("Api-Key")
	if len(body) > 0 && req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if provider.Protocol == "anthropic" {
		if req.Header.Get("Anthropic-Version") == "" {
			req.Header.Set("Anthropic-Version", "2023-06-01")
		}
		if stream {
			req.Header.Set("Accept", "text/event-stream")
		} else if req.Header.Get("Accept") == "" {
			req.Header.Set("Accept", "application/json")
		}
		if provider.APIKey != "" {
			req.Header.Set("X-API-Key", provider.APIKey)
		}
	} else if provider.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+provider.APIKey)
	}
	req.ContentLength = int64(len(body))
	return req, nil
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

// resolveProviderName 仅按 body.model 的 models 规则匹配 provider。
// 已禁用 X-AI-Provider / ?provider= / provider/model 显式选择。
func (h *Handler) resolveProviderName(_ *http.Request, model string) (string, error) {
	model = strings.TrimSpace(model)
	if model != "" {
		if name, ok, err := h.findProviderByModel(model); ok || err != nil {
			return name, err
		}
		if name, ok, err := h.defaultProvider(); ok || err != nil {
			return name, err
		}
		return "", fmt.Errorf("no provider matches model %q; configure provider models patterns or default_provider", model)
	}
	if name, ok, err := h.defaultProvider(); ok || err != nil {
		return name, err
	}
	return "", fmt.Errorf("model is required or set default_provider")
}

func (h *Handler) findProviderByModel(model string) (string, bool, error) {
	model = strings.ToLower(strings.TrimSpace(model))
	if model == "" {
		return "", false, nil
	}
	matches := make([]string, 0, 1)
	for name, provider := range h.cfg.Providers {
		if provider.Disabled {
			continue
		}
		if providerMatchesModel(name, provider, model) {
			matches = append(matches, name)
		}
	}
	if len(matches) == 1 {
		return matches[0], true, nil
	}
	if len(matches) > 1 {
		return "", true, fmt.Errorf("multiple providers match model %q; disambiguate provider models patterns", model)
	}
	return "", false, nil
}

func providerMatchesModel(name string, provider config.Provider, model string) bool {
	patterns := provider.Models
	if len(patterns) == 0 {
		patterns = defaultModelPatterns(name, provider.Protocol)
	}
	for _, pattern := range patterns {
		if matchModelPattern(model, pattern) {
			return true
		}
	}
	return false
}

func defaultModelPatterns(name, protocol string) []string {
	switch strings.ToLower(name) {
	case "deepseek":
		return []string{"deepseek*"}
	case "anthropic", "claude":
		return []string{"claude*"}
	case "openai":
		return []string{"gpt-*", "chatgpt-*", "o*", "text-embedding-*", "dall-e-*"}
	}
	if protocol == "anthropic" {
		return []string{"claude*"}
	}
	return nil
}

func matchModelPattern(model, pattern string) bool {
	pattern = strings.ToLower(strings.TrimSpace(pattern))
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

func (h *Handler) defaultProvider() (string, bool, error) {
	name := strings.ToLower(strings.TrimSpace(h.cfg.DefaultProvider))
	if name == "" {
		return "", false, nil
	}
	provider, ok := h.cfg.Providers[name]
	if !ok {
		return "", false, fmt.Errorf("default_provider %q is not configured", name)
	}
	if provider.Disabled {
		return "", false, fmt.Errorf("default_provider %q is disabled", name)
	}
	return name, true, nil
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

func (h *Handler) recordAndPrint(round *archive.Round, r *http.Request, provider, model string, stream bool, status int, duration time.Duration, usage tokenUsage, errMessage string) {
	h.recordAndPrintFail(round, r, provider, model, stream, status, duration, usage, streamFailFromMessage(errMessage))
}

func (h *Handler) recordAndPrintFail(round *archive.Round, r *http.Request, provider, model string, stream bool, status int, duration time.Duration, usage tokenUsage, fail *streamFail) {
	outcome := outcomeFromStreamFail(fail, status)
	errMessage := ""
	if fail != nil {
		errMessage = fail.Error()
	}
	record := stats.Record{
		Time:                     time.Now(),
		Provider:                 provider,
		Model:                    model,
		InputTokens:              usage.PromptTokens,
		OutputTokens:             usage.CompletionTokens,
		CachedInputTokens:        usage.CachedInputTokens,
		CacheCreationInputTokens: usage.CacheCreationInputTokens,
		CacheHitRate:             usage.CacheHitRate(),
		Duration:                 duration,
		Stream:                   stream,
		Estimated:                usage.Estimated,
		HTTPStatus:               status,
		Outcome:                  outcome,
	}
	if err := h.recorder.Append(record); err != nil {
		log.Printf("append usage record: %v", err)
	}
	route := RouteLabel(r)
	h.metricsRegistry.RecordRequest(provider, model, route, status, duration, outcome)
	// 仅流式 midflight 上游读失败计入 provider 错误率,避免非流式状态码/本地错误重复计数。
	if shouldCountUpstreamError(fail, stream) {
		h.metricsRegistry.RecordUpstreamError(provider, -2) // -2 = stream_midflight
	}
	h.metricsRegistry.RecordTokens(provider, model, usage.PromptTokens, usage.CompletionTokens, usage.CachedInputTokens, usage.CacheCreationInputTokens)
	h.printSummary(round, provider, model, stream, status, duration, usage, errMessage)
}

func (h *Handler) writeArchiveMetadata(round *archive.Round, provider, model string, stream bool, status int, duration time.Duration, usage tokenUsage, responsePath, message, fullResponsePath, outcome string) {
	stableHash, fingerprint, drift, driftCount := h.driftInfo(round)
	fullContent := round == nil || round.FullContent()
	if outcome == "" {
		outcome = outcomeFromStreamFail(streamFailFromMessage(message), status)
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
