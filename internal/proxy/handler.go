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
	if !strings.HasPrefix(r.URL.Path, "/v1/") {
		http.NotFound(w, r)
		return
	}
	if r.URL.Path == "/v1/chat/completions" && r.Method == http.MethodPost {
		h.handleChatCompletions(w, r, requestID)
		return
	}
	h.forwardRaw(w, r, requestID)
}

func (h *Handler) handleChatCompletions(w http.ResponseWriter, r *http.Request, requestID string) {
	start := time.Now()
	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "read request body failed", http.StatusBadRequest)
		return
	}
	defer r.Body.Close()

	round, err := h.startRound()
	if err != nil {
		http.Error(w, "start interaction archive failed", http.StatusInternalServerError)
		return
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
	bodyBytes, model = rewriteModelPrefix(bodyBytes, body, model, providerName, h.cfg.Providers)
	h.archiveAndLogProviderSelection(round, r, providerName, provider, model, stream)

	if provider.Protocol == "anthropic" {
		h.handleAnthropicChatCompletions(w, r, round, start, providerName, provider, body, model, stream)
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
	h.writeArchiveMetadata(round, provider, model, stream, status, duration, usage, "response.txt", message, "")
}

func (h *Handler) forwardRaw(w http.ResponseWriter, r *http.Request, requestID string) {
	start := time.Now()
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "read request body failed", http.StatusBadRequest)
		return
	}
	defer r.Body.Close()
	rawBody, rawModel, rawStream := parseRawRequestBody(body)
	round, err := h.startRound()
	if err != nil {
		http.Error(w, "start interaction archive failed", http.StatusInternalServerError)
		return
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
		copyHeader(w.Header(), resp.Header)
		w.WriteHeader(resp.StatusCode)
		usage, fullPath, streamErr := h.copyAndArchiveRawStream(w, resp, round, providerName, provider, rawModel, rawBody, r.Context(), result.Cancel)
		duration := time.Since(start)
		h.recordAndPrint(round, r, providerName, rawModel, true, resp.StatusCode, duration, usage, streamErr)
		h.writeArchiveMetadata(round, providerName, rawModel, true, resp.StatusCode, duration, usage, responsePath, streamErr, fullPath)
		return
	}
	responseBody, err := io.ReadAll(resp.Body)
	readErrMessage := ""
	if err != nil {
		readErrMessage = h.logStreamIssue(round, providerName, rawModel, "read raw response", err, nil, nil)
	}
	responseBody, responseHeader := decodedResponseBodyAndHeader(responseBody, resp.Header)
	copyHeader(w.Header(), responseHeader)
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
	h.recordAndPrint(round, r, providerName, rawModel, rawStream, resp.StatusCode, duration, usage, readErrMessage)
	h.writeArchiveMetadata(round, providerName, rawModel, rawStream, resp.StatusCode, duration, usage, responsePath, readErrMessage, "")
}

func (h *Handler) handleBufferedResponse(w http.ResponseWriter, resp *http.Response, round *archive.Round, start time.Time, providerName, model string, stream bool, requestBody map[string]any, r *http.Request) {
	responseBody, readErr := io.ReadAll(resp.Body)
	readErrMessage := ""
	responseBody, responseHeader := decodedResponseBodyAndHeader(responseBody, resp.Header)
	copyHeader(w.Header(), responseHeader)
	w.WriteHeader(resp.StatusCode)
	if len(responseBody) > 0 {
		_, _ = w.Write(responseBody)
	}
	if readErr != nil {
		readErrMessage = h.logStreamIssue(round, providerName, model, "read upstream response", readErr, nil, nil)
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
	h.recordAndPrint(round, r, providerName, model, stream, resp.StatusCode, duration, usage, readErrMessage)
	h.writeArchiveMetadata(round, providerName, model, stream, resp.StatusCode, duration, usage, responsePath, readErrMessage, "")
}

func (h *Handler) handleStreamResponse(w http.ResponseWriter, resp *http.Response, round *archive.Round, start time.Time, providerName, model string, requestBody map[string]any, requestContext context.Context, cancel context.CancelFunc, r *http.Request) {
	copyHeader(w.Header(), resp.Header)
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
	idleTimer, stopIdleTimer := h.startStreamIdleTimer(cancel)
	defer stopIdleTimer()
	streamErr := ""
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			resetStreamIdleTimer(idleTimer, h.cfg.StreamIdleTimeout)
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
		}
		if err != nil {
			if err != io.EOF {
				streamErr = h.logStreamIssue(round, providerName, model, "read upstream stream", err, requestContext, idleTimer)
			}
			break
		}
	}

	usage := accumulator.FinalizeUsage(requestBody)
	if fullResponse, err := accumulator.ResponseJSON(); err != nil {
		log.Printf("build stream full response: %v", err)
	} else if err := round.WriteResponse("response.json", append(fullResponse, '\n')); err != nil {
		log.Printf("archive stream full response: %v", err)
	}
	duration := time.Since(start)
	h.recordAndPrint(round, r, providerName, model, true, resp.StatusCode, duration, usage, streamErr)
	h.writeArchiveMetadata(round, providerName, model, true, resp.StatusCode, duration, usage, "response.sse", streamErr, "response.json")
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

func (h *Handler) copyAndArchiveRawStream(w http.ResponseWriter, resp *http.Response, round *archive.Round, providerName string, provider config.Provider, model string, requestBody map[string]any, requestContext context.Context, cancel context.CancelFunc) (tokenUsage, string, string) {
	archiveWriter, err := round.CreateResponseWriter("response.sse")
	if err != nil {
		log.Printf("archive raw stream response: %v", err)
	}
	if archiveWriter != nil {
		defer archiveWriter.Close()
	}
	var openAIAccumulator *openAIStreamAccumulator
	var anthropicAccumulator *anthropicRawStreamAccumulator
	if provider.Protocol == "anthropic" {
		anthropicAccumulator = newAnthropicRawStreamAccumulator(model)
	} else {
		openAIAccumulator = newOpenAIStreamAccumulator(model)
	}

	reader := bufio.NewReader(resp.Body)
	idleTimer, stopIdleTimer := h.startStreamIdleTimer(cancel)
	defer stopIdleTimer()
	streamErr := ""
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			resetStreamIdleTimer(idleTimer, h.cfg.StreamIdleTimeout)
			if openAIAccumulator != nil {
				openAIAccumulator.TrackSSELine(line)
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
		}
		if err != nil {
			if err != io.EOF {
				streamErr = h.logStreamIssue(round, providerName, model, "read raw stream", err, requestContext, idleTimer)
			}
			break
		}
	}

	if openAIAccumulator != nil {
		usage := openAIAccumulator.FinalizeUsage(requestBody)
		if fullResponse, err := openAIAccumulator.ResponseJSON(); err != nil {
			log.Printf("build raw stream full response: %v", err)
		} else if err := round.WriteResponse("response.json", append(fullResponse, '\n')); err != nil {
			log.Printf("archive raw stream full response: %v", err)
		}
		return usage, "response.json", streamErr
	}
	usage := anthropicAccumulator.FinalizeUsage(requestBody)
	if fullResponse, err := anthropicAccumulator.ResponseJSON(usage); err != nil {
		log.Printf("build anthropic raw stream full response: %v", err)
	} else if err := round.WriteResponse("response.json", append(fullResponse, '\n')); err != nil {
		log.Printf("archive anthropic raw stream full response: %v", err)
	}
	return usage, "response.json", streamErr
}

type upstreamResult struct {
	ProviderName string
	Provider     config.Provider
	Response     *http.Response
	Duration     time.Duration
	Cancel       context.CancelFunc
}

func (h *Handler) doUpstreamWithFallback(r *http.Request, round *archive.Round, providerName string, provider config.Provider, body []byte, bodyBytes int, stream bool) (upstreamResult, error) {
	candidates := h.fallbackCandidates(providerName, provider)
	attempts := make([]fallbackAttemptDebugInfo, 0, len(candidates))
	var lastErr error
	for index, candidateName := range candidates {
		candidate := h.cfg.Providers[candidateName]
		ctx, cancel := h.upstreamContext(r.Context(), stream)
		req, err := h.newUpstreamRequest(ctx, r, candidate, body)
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
		if !shouldFallbackStatus(resp.StatusCode) || index == len(candidates)-1 {
			h.archiveAndLogFallbackAttempts(round, attempts)
			return upstreamResult{ProviderName: candidateName, Provider: candidate, Response: resp, Duration: duration, Cancel: cancel}, nil
		}
		h.logUpstreamAlert(round, candidateName, candidate.Protocol, resp.StatusCode, duration, "", true, candidates[index+1])
		h.debugfRound(round, r, "upstream fallback provider=%s status=%d next=%s", candidateName, resp.StatusCode, candidates[index+1])
		h.metricsRegistry.RecordFallbackAttempt(candidateName, candidates[index+1], statusBucketForFallback(resp.StatusCode))
		_ = resp.Body.Close()
		if cancel != nil {
			cancel()
		}
	}
	h.archiveAndLogFallbackAttempts(round, attempts)
	if lastErr != nil {
		return upstreamResult{}, lastErr
	}
	return upstreamResult{}, fmt.Errorf("no fallback providers available")
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
	target, err := buildUpstreamURL(provider.BaseURL, r.URL)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, r.Method, target, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	copyHeader(req.Header, r.Header)
	removeHopByHop(req.Header)
	req.Header.Del("Accept-Encoding")
	req.Header.Del("Content-Length")
	req.Header.Del("X-AI-Provider")
	if provider.APIKey != "" {
		if provider.Protocol == "anthropic" {
			req.Header.Set("X-API-Key", provider.APIKey)
			req.Header.Del("Authorization")
		} else {
			req.Header.Set("Authorization", "Bearer "+provider.APIKey)
		}
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
	if basePath == "/v1" && (incomingPath == "/v1" || strings.HasPrefix(incomingPath, "/v1/")) {
		return incomingPath
	}
	return basePath + incomingPath
}

func (h *Handler) resolveProviderName(r *http.Request, model string) (string, error) {
	if provider := strings.TrimSpace(r.Header.Get("X-AI-Provider")); provider != "" {
		return h.validateExplicitProvider(strings.ToLower(provider))
	}
	if provider := strings.TrimSpace(r.URL.Query().Get("provider")); provider != "" {
		return h.validateExplicitProvider(strings.ToLower(provider))
	}
	if idx := strings.IndexRune(model, '/'); idx > 0 {
		prefix := strings.ToLower(model[:idx])
		if _, ok := h.cfg.Providers[prefix]; ok {
			return h.validateExplicitProvider(prefix)
		}
	}
	protocol := inferredProtocol(r)
	if model != "" {
		if protocol != "" {
			if name, ok, err := h.findProviderByModel(protocol, model); ok || err != nil {
				return name, err
			}
		}
		if name, ok, err := h.findProviderByModel("", model); ok || err != nil {
			return name, err
		}
		if protocol != "" {
			return h.providerForProtocolAndModel(protocol, model)
		}
		return h.providerForModel("", model)
	}
	if protocol != "" {
		return h.providerForProtocolAndModel(protocol, model)
	}
	if name, ok, err := h.defaultProviderForProtocol(""); ok || err != nil {
		return name, err
	}
	if h.enabledProviderCount() == 1 {
		for name, provider := range h.cfg.Providers {
			if provider.Disabled {
				continue
			}
			return name, nil
		}
	}
	return "", fmt.Errorf("provider is ambiguous; set X-AI-Provider, ?provider=, or use a model prefix like provider/model")
}

func (h *Handler) validateExplicitProvider(name string) (string, error) {
	provider, ok := h.cfg.Providers[name]
	if !ok {
		return "", fmt.Errorf("provider %q is not configured", name)
	}
	if provider.Disabled {
		return "", fmt.Errorf("provider %q is disabled", name)
	}
	return name, nil
}

func inferredProtocol(r *http.Request) string {
	if r.URL.Path == "/v1/messages" || hasHeaderPrefix(r.Header, "Anthropic-") {
		return "anthropic"
	}
	if r.URL.Path == "/v1/chat/completions" || r.URL.Path == "/v1/completions" || r.URL.Path == "/v1/embeddings" || r.URL.Path == "/v1/responses" || r.URL.Path == "/v1/models" {
		return "openai"
	}
	return ""
}

func hasHeaderPrefix(headers http.Header, prefix string) bool {
	prefix = strings.ToLower(prefix)
	for key := range headers {
		if strings.HasPrefix(strings.ToLower(key), prefix) {
			return true
		}
	}
	return false
}

func (h *Handler) providerForProtocolAndModel(protocol, model string) (string, error) {
	if model != "" {
		if name, ok, err := h.findProviderByModel(protocol, model); ok || err != nil {
			return name, err
		}
	}
	if name, ok, err := h.defaultProviderForProtocol(protocol); ok || err != nil {
		return name, err
	}
	return h.uniqueProviderForProtocol(protocol)
}

func (h *Handler) providerForModel(protocol, model string) (string, error) {
	if name, ok, err := h.findProviderByModel(protocol, model); ok || err != nil {
		return name, err
	}
	if name, ok, err := h.defaultProviderForProtocol(protocol); ok || err != nil {
		return name, err
	}
	return "", fmt.Errorf("provider is ambiguous for model %q; set X-AI-Provider, ?provider=, provider/model, or provider models patterns", model)
}

func (h *Handler) findProviderByModel(protocol, model string) (string, bool, error) {
	model = strings.ToLower(strings.TrimSpace(model))
	if model == "" {
		return "", false, nil
	}
	matches := make([]string, 0, 1)
	for name, provider := range h.cfg.Providers {
		if provider.Disabled {
			continue
		}
		if protocol != "" && provider.Protocol != protocol {
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
		return "", true, fmt.Errorf("multiple providers match model %q; set X-AI-Provider, ?provider=, or use provider/model", model)
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

func (h *Handler) uniqueProviderForProtocol(protocol string) (string, error) {
	matches := make([]string, 0, 1)
	for name, provider := range h.cfg.Providers {
		if provider.Disabled {
			continue
		}
		if provider.Protocol == protocol {
			matches = append(matches, name)
		}
	}
	if len(matches) == 1 {
		return matches[0], nil
	}
	if name, ok, err := h.defaultProviderForProtocol(protocol); ok || err != nil {
		return name, err
	}
	if len(matches) == 0 && h.enabledProviderCount() == 1 {
		for name, provider := range h.cfg.Providers {
			if provider.Disabled {
				continue
			}
			return name, nil
		}
	}
	if len(matches) == 0 {
		return "", fmt.Errorf("no provider configured for protocol %q; set X-AI-Provider or add a provider with protocol: %s", protocol, protocol)
	}
	return "", fmt.Errorf("multiple providers configured for protocol %q; set X-AI-Provider, ?provider=, provider/model, or provider models patterns", protocol)
}

func (h *Handler) defaultProviderForProtocol(protocol string) (string, bool, error) {
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
	if protocol != "" && provider.Protocol != protocol {
		return "", false, fmt.Errorf("default_provider %q uses protocol %q, but request requires protocol %q; set X-AI-Provider, ?provider=, or provider/model", name, provider.Protocol, protocol)
	}
	return name, true, nil
}

func (h *Handler) enabledProviderCount() int {
	count := 0
	for _, provider := range h.cfg.Providers {
		if !provider.Disabled {
			count++
		}
	}
	return count
}

func rewriteModelPrefix(original []byte, body map[string]any, model, providerName string, providers map[string]config.Provider) ([]byte, string) {
	idx := strings.IndexRune(model, '/')
	if idx <= 0 {
		return original, model
	}
	prefix := strings.ToLower(model[:idx])
	if prefix != providerName {
		return original, model
	}
	if _, ok := providers[prefix]; !ok {
		return original, model
	}
	stripped := model[idx+1:]
	body["model"] = stripped
	encoded, err := json.Marshal(body)
	if err != nil {
		return original, model
	}
	return encoded, stripped
}

func copyHeader(dst, src http.Header) {
	for key, values := range src {
		for _, value := range values {
			dst.Add(key, value)
		}
	}
}

func decodedResponseBodyAndHeader(body []byte, header http.Header) ([]byte, http.Header) {
	decodedHeader := header.Clone()
	if !strings.EqualFold(strings.TrimSpace(decodedHeader.Get("Content-Encoding")), "gzip") {
		return body, decodedHeader
	}
	reader, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		return body, decodedHeader
	}
	defer reader.Close()
	decodedBody, err := io.ReadAll(reader)
	if err != nil {
		return body, decodedHeader
	}
	decodedHeader.Del("Content-Encoding")
	decodedHeader.Del("Content-Length")
	return decodedBody, decodedHeader
}

func removeHopByHop(header http.Header) {
	for _, key := range []string{
		"Connection",
		"Keep-Alive",
		"Proxy-Authenticate",
		"Proxy-Authorization",
		"TE",
		"Trailer",
		"Transfer-Encoding",
		"Upgrade",
	} {
		header.Del(key)
	}
}

func (h *Handler) recordAndPrint(round *archive.Round, r *http.Request, provider, model string, stream bool, status int, duration time.Duration, usage tokenUsage, errMessage string) {
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
	}
	if err := h.recorder.Append(record); err != nil {
		log.Printf("append usage record: %v", err)
	}
	route := RouteLabel(r)
	h.metricsRegistry.RecordRequest(provider, model, route, status, duration)
	h.metricsRegistry.RecordTokens(provider, model, usage.PromptTokens, usage.CompletionTokens, usage.CachedInputTokens, usage.CacheCreationInputTokens)
	h.printSummary(round, provider, model, stream, status, duration, usage, errMessage)
}

func (h *Handler) writeArchiveMetadata(round *archive.Round, provider, model string, stream bool, status int, duration time.Duration, usage tokenUsage, responsePath, message, fullResponsePath string) {
	stableHash, fingerprint, drift, driftCount := h.driftInfo(round)
	if err := round.WriteMetadata(archive.Metadata{
		FinishedAt:               time.Now(),
		Provider:                 provider,
		Model:                    model,
		StablePrefixHash:         stableHash,
		RequestFingerprint:       fingerprint,
		StablePrefixDrift:        drift,
		StablePrefixDriftCount:   driftCount,
		Stream:                   stream,
		HTTPStatus:               status,
		DurationMS:               duration.Milliseconds(),
		InputTokens:              usage.PromptTokens,
		OutputTokens:             usage.CompletionTokens,
		TotalTokens:              usage.PromptTokens + usage.CompletionTokens,
		CachedInputTokens:        usage.CachedInputTokens,
		CacheCreationInputTokens: usage.CacheCreationInputTokens,
		CacheHitRate:             usage.CacheHitRate(),
		Estimated:                usage.Estimated,
		RequestPath:              "request.json",
		RequestMetaPath:          "request.meta.json",
		UpstreamRequestPath:      "upstream_request.json",
		UpstreamResponsePath:     "upstream_response.json",
		ResponsePath:             responsePath,
		FullResponsePath:         fullResponsePath,
		Error:                    message,
	}); err != nil {
		log.Printf("archive metadata: %v", err)
	}
}

// driftInfo 从 round 已记录的稳定 prefix 与 fingerprint 提取漂移信息。
// Round 不携带 prefix 时返回零值。
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

func (h *Handler) logStreamIssue(round *archive.Round, provider, model, operation string, err error, requestContext context.Context, idleTimer *streamIdleTimer) string {
	if err == nil {
		return ""
	}
	message := fmt.Sprintf("%s: %v", operation, err)
	level := slog.LevelWarn
	if idleTimer != nil && idleTimer.expired.Load() {
		message = fmt.Sprintf("%s: stream idle timeout exceeded after %s", operation, idleTimer.timeout.Truncate(time.Millisecond))
	} else if requestContext != nil && errors.Is(requestContext.Err(), context.Canceled) {
		message = fmt.Sprintf("%s: client canceled downstream request", operation)
		level = slog.LevelInfo
	} else if requestContext != nil && errors.Is(requestContext.Err(), context.DeadlineExceeded) {
		message = fmt.Sprintf("%s: downstream request deadline exceeded", operation)
	}
	slog.LogAttrs(context.Background(), level, "stream issue",
		slog.String("event", "STREAM"),
		slog.Int("round", roundID(round)),
		slog.String("provider", provider),
		slog.String("model", model),
		slog.String("message", message),
	)
	return message
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
