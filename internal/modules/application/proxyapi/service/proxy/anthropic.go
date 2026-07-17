package proxy

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"ai-proxy/internal/pkg/aiproxyarchive"
	"ai-proxy/internal/pkg/aiproxyconfig"
)

type anthropicMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type anthropicRequest struct {
	Model       string             `json:"model"`
	Messages    []anthropicMessage `json:"messages"`
	MaxTokens   int                `json:"max_tokens"`
	System      string             `json:"system,omitempty"`
	Stream      bool               `json:"stream,omitempty"`
	Temperature any                `json:"temperature,omitempty"`
	TopP        any                `json:"top_p,omitempty"`
	Stop        any                `json:"stop_sequences,omitempty"`
}

// handleAnthropicMessages 处理客户端 POST /v1/messages。
// 通过 TransportPlan 决定 native 或 anthropic_to_openai 转换。
func (h *Handler) handleAnthropicMessages(w http.ResponseWriter, r *http.Request, requestID string) {
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
	h.archiveAndLogTransportPlan(round, r, plan, provider, stream)

	switch plan.Mode {
	case TransportModeNative:
		h.forwardAnthropicNative(w, r, round, start, plan, provider, bodyBytes, body, model, stream)
	case TransportModeAnthropicToOpenAI:
		if apiErr := ValidateConversionRequest(plan, body); apiErr != nil {
			h.writeArchivedAPIError(w, round, r, start, plan.RouteOwner, model, stream, statusForAPIError(apiErr), *apiErr)
			return
		}
		h.convertAnthropicMessagesToOpenAI(w, r, round, start, plan, provider, body, model, stream)
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
	}
}

func (h *Handler) forwardAnthropicNative(w http.ResponseWriter, r *http.Request, round *archive.Round, start time.Time, plan TransportPlan, provider config.Provider, bodyBytes []byte, body map[string]any, model string, stream bool) {
	providerName := plan.RouteOwner
	streamRequest := stream || acceptsEventStream(r.Header)
	result, err := h.doUpstreamPath(r, round, providerName, provider, bodyBytes, len(bodyBytes), streamRequest, plan.UpstreamEndpoint, r.URL.RawQuery, r.Method)
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

	responsePath := responseFileName(resp.Header.Get("Content-Type"), strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "event-stream"))
	if strings.HasSuffix(responsePath, ".sse") {
		copyResponseHeader(w.Header(), resp.Header)
		w.WriteHeader(resp.StatusCode)
		usage, fullPath, streamErr := h.copyAndArchiveRawStream(w, resp, round, providerName, provider, model, body, r.Context(), result.Cancel, plan.UpstreamEndpoint)
		duration := time.Since(start)
		errMsg := ""
		if streamErr != nil {
			errMsg = streamErr.Error()
		}
		h.recordAndPrintFail(round, r, providerName, model, true, resp.StatusCode, duration, usage, streamErr)
		h.writeArchiveMetadata(round, providerName, model, true, resp.StatusCode, duration, usage, responsePath, errMsg, fullPath, outcomeFromStreamFail(streamErr, resp.StatusCode))
		return
	}
	responseBody, err := h.readLimitedUpstream(resp.Body)
	if err != nil {
		h.writeArchivedError(w, round, r, start, providerName, model, stream, http.StatusBadGateway, err.Error())
		return
	}
	responseBody, responseHeader, err := h.decodedResponseBodyAndHeader(responseBody, resp.Header)
	if err != nil {
		h.writeArchivedError(w, round, r, start, providerName, model, stream, http.StatusBadGateway, err.Error())
		return
	}
	copyResponseHeader(w.Header(), responseHeader)
	w.WriteHeader(resp.StatusCode)
	if len(responseBody) > 0 {
		_, _ = w.Write(responseBody)
	}
	if err := round.WriteResponse(responsePath, responseBody); err != nil {
		log.Printf("archive anthropic response: %v", err)
	}
	usage := tokenUsage{}
	if resp.StatusCode < 400 {
		usage = usageFromRawResponse(provider, responseBody, body)
	}
	duration := time.Since(start)
	h.recordAndPrint(round, r, providerName, model, stream, resp.StatusCode, duration, usage, "")
	h.writeArchiveMetadata(round, providerName, model, stream, resp.StatusCode, duration, usage, responsePath, "", "", "")
}

func (h *Handler) convertAnthropicMessagesToOpenAI(w http.ResponseWriter, r *http.Request, round *archive.Round, start time.Time, plan TransportPlan, provider config.Provider, body map[string]any, model string, stream bool) {
	providerName := plan.RouteOwner
	openAIBody, err := buildOpenAIChatFromAnthropic(body, model, stream)
	if err != nil {
		// 转换构造失败视为 conversion_unsupported(preflight 后仍可能因消息结构失败)。
		h.writeArchivedAPIError(w, round, r, start, providerName, model, stream, http.StatusBadRequest, conversionAPIError(plan, err))
		return
	}
	result, err := h.doUpstreamPath(r, round, providerName, provider, openAIBody, len(openAIBody), stream, plan.UpstreamEndpoint, "", http.MethodPost)
	if err != nil {
		h.writeArchivedError(w, round, r, start, providerName, model, stream, http.StatusBadGateway, err.Error())
		return
	}
	resp := result.Response
	providerName = result.ProviderName
	if result.Cancel != nil {
		defer result.Cancel()
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		h.writeConversionUpstreamError(w, r, resp, round, start, plan, providerName, model, stream)
		return
	}
	if stream {
		h.handleOpenAIToAnthropicStream(w, r, resp, round, start, providerName, model, body, r.Context(), result.Cancel)
		return
	}
	h.handleOpenAIToAnthropicBuffered(w, r, resp, round, start, providerName, model, body)
}

// handleAnthropicChatCompletions: OpenAI 客户端 /v1/chat/completions → Anthropic 上游 /v1/messages。
func (h *Handler) handleAnthropicChatCompletions(w http.ResponseWriter, r *http.Request, round *archive.Round, start time.Time, plan TransportPlan, provider config.Provider, _ []byte, body map[string]any, model string, stream bool) {
	providerName := plan.RouteOwner
	payload, err := buildAnthropicRequest(body, model, stream)
	if err != nil {
		h.writeArchivedAPIError(w, round, r, start, providerName, model, stream, http.StatusBadRequest, conversionAPIError(plan, err))
		return
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		h.writeArchivedError(w, round, r, start, providerName, model, stream, http.StatusBadRequest, err.Error())
		return
	}

	result, err := h.doUpstreamPath(r, round, providerName, provider, encoded, len(encoded), stream, plan.UpstreamEndpoint, "", http.MethodPost)
	if err != nil {
		h.writeArchivedError(w, round, r, start, providerName, model, stream, http.StatusBadGateway, err.Error())
		return
	}
	resp := result.Response
	providerName = result.ProviderName
	if result.Cancel != nil {
		defer result.Cancel()
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		h.writeConversionUpstreamError(w, r, resp, round, start, plan, providerName, model, stream)
		return
	}
	if stream {
		h.handleAnthropicStream(w, r, resp, round, start, providerName, model, body, r.Context(), result.Cancel)
		return
	}
	h.handleAnthropicBuffered(w, r, resp, round, start, providerName, model, body)
}

// ValidateConversionRequest 在访问上游前检查 payload 是否处于转换最低保证范围。
// 失败返回 conversion_unsupported,不得创建上游请求。
func ValidateConversionRequest(plan TransportPlan, body map[string]any) *APIError {
	if !plan.IsConversion() {
		return nil
	}
	direction := plan.Mode
	if err := rejectUnsupportedConversionFeatures(body, direction); err != nil {
		apiErr := conversionAPIError(plan, err)
		return &apiErr
	}
	// 深度扫描 messages / system 中的非 text content 与 tool 角色。
	if err := rejectUnsupportedConversionPayload(body, direction); err != nil {
		apiErr := conversionAPIError(plan, err)
		return &apiErr
	}
	return nil
}

func conversionAPIError(plan TransportPlan, err error) APIError {
	feature := conversionFeatureFromError(err)
	msg := err.Error()
	if feature != "" {
		msg = fmt.Sprintf("conversion does not support %q; simplify request or use a native endpoint contract", feature)
	}
	return APIError{
		Code:             ErrorCodeConversionUnsupported,
		Message:          msg,
		Model:            plan.ModelID,
		Operation:        plan.Operation,
		ClientEndpoint:   plan.ClientEndpoint,
		ClientProtocol:   plan.ClientProtocol,
		UpstreamProtocol: plan.UpstreamProtocol,
		Feature:          feature,
	}
}

func conversionFeatureFromError(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	// 从已知错误文案提取 feature 名。
	for _, key := range []string{
		"tools", "tool_choice", "functions", "function_call", "response_format",
		"stop_sequences", "stop",
		"tool_calls", "tool role", "tool_use", "tool_result",
		"image", "image_url", "input_image", "audio", "input_audio", "document", "file",
	} {
		if strings.Contains(msg, key) || strings.Contains(msg, `"`+key+`"`) {
			return key
		}
	}
	if strings.Contains(msg, "role ") {
		return "unsupported_role"
	}
	if strings.Contains(msg, "content type") {
		return "unsupported_content"
	}
	return "unsupported_feature"
}

// rejectUnsupportedConversionPayload 扫描 messages/system 等结构,拒绝非文本与 tool 角色。
func rejectUnsupportedConversionPayload(body map[string]any, direction string) error {
	if messages, ok := body["messages"].([]any); ok {
		for _, item := range messages {
			message, ok := item.(map[string]any)
			if !ok {
				continue
			}
			role, _ := message["role"].(string)
			if role == "tool" {
				return fmt.Errorf("%s conversion does not support tool role messages; use a native provider", direction)
			}
			if hasNonEmptyConversionFeature(message["tool_calls"]) {
				return fmt.Errorf("%s conversion does not support tool_calls; use a native provider", direction)
			}
			if _, err := extractTextContent(message["content"], direction); err != nil {
				return err
			}
		}
	}
	if systemRaw, ok := body["system"]; ok && systemRaw != nil {
		if _, err := extractTextContent(systemRaw, direction); err != nil {
			return err
		}
	}
	return nil
}

// writeConversionUpstreamError 保留上游 HTTP status,输出客户端协议兼容的安全错误 envelope。
func (h *Handler) writeConversionUpstreamError(w http.ResponseWriter, r *http.Request, resp *http.Response, round *archive.Round, start time.Time, plan TransportPlan, providerName, model string, stream bool) {
	responseBody, err := h.readLimitedUpstream(resp.Body)
	if err != nil {
		h.writeArchivedError(w, round, r, start, providerName, model, stream, http.StatusBadGateway, err.Error())
		return
	}
	responseBody, responseHeader, err := h.decodedResponseBodyAndHeader(responseBody, resp.Header)
	if err != nil {
		h.writeArchivedError(w, round, r, start, providerName, model, stream, http.StatusBadGateway, err.Error())
		return
	}

	// 按客户端协议转换错误 envelope;无法安全解析时输出 ai-proxy typed error。
	outBody, contentType := convertUpstreamErrorForClient(plan, resp.StatusCode, responseBody, providerName)
	header := responseHeader.Clone()
	header.Del("Content-Length")
	header.Del("Content-Encoding")
	if contentType != "" {
		header.Set("Content-Type", contentType)
	}
	copyResponseHeader(w.Header(), header)
	w.WriteHeader(resp.StatusCode)
	if len(outBody) > 0 {
		_, _ = w.Write(outBody)
	}
	responsePath := responseFileName(contentType, false)
	if err := round.WriteResponse(responsePath, outBody); err != nil {
		log.Printf("archive converted upstream error: %v", err)
	}
	duration := time.Since(start)
	h.recordAndPrint(round, r, providerName, model, stream, resp.StatusCode, duration, tokenUsage{}, "")
	h.writeArchiveMetadata(round, providerName, model, stream, resp.StatusCode, duration, tokenUsage{}, responsePath, "", "", "")
}

// convertUpstreamErrorForClient 将上游错误体转换为客户端协议 envelope,并脱敏。
func convertUpstreamErrorForClient(plan TransportPlan, status int, upstreamBody []byte, routeOwner string) ([]byte, string) {
	safeMsg := summarizeUpstreamError(upstreamBody, status, routeOwner)
	switch plan.ClientProtocol {
	case ClientProtocolOpenAI:
		// Anthropic upstream → OpenAI client
		payload := map[string]any{
			"error": map[string]any{
				"message": safeMsg,
				"type":    "upstream_error",
				"code":    "upstream_error",
				"param":   nil,
			},
		}
		b, _ := json.Marshal(payload)
		return b, "application/json"
	case ClientProtocolAnthropic:
		// OpenAI upstream → Anthropic client
		payload := map[string]any{
			"type": "error",
			"error": map[string]any{
				"type":    "api_error",
				"message": safeMsg,
			},
		}
		b, _ := json.Marshal(payload)
		return b, "application/json"
	default:
		payload := APIErrorResponse{Error: APIError{
			Code:             "upstream_error",
			Message:          safeMsg,
			Model:            plan.ModelID,
			Operation:        plan.Operation,
			ClientEndpoint:   plan.ClientEndpoint,
			ClientProtocol:   plan.ClientProtocol,
			UpstreamProtocol: plan.UpstreamProtocol,
		}}
		b, _ := json.Marshal(payload)
		return b, "application/json"
	}
}

func summarizeUpstreamError(body []byte, status int, routeOwner string) string {
	// 不回传完整上游 body;仅提取安全 message 摘要。
	msg := ""
	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err == nil {
		if errObj, ok := parsed["error"].(map[string]any); ok {
			if m, ok := errObj["message"].(string); ok {
				msg = m
			} else if m, ok := errObj["type"].(string); ok {
				msg = m
			}
		} else if m, ok := parsed["message"].(string); ok {
			msg = m
		}
	}
	msg = redactSecrets(msg)
	if len(msg) > 240 {
		msg = msg[:240] + "..."
	}
	if msg == "" {
		msg = fmt.Sprintf("upstream returned HTTP %d", status)
	}
	if routeOwner != "" {
		return fmt.Sprintf("%s (route_owner=%s)", msg, routeOwner)
	}
	return msg
}

func redactSecrets(s string) string {
	// 粗粒度脱敏:去掉看起来像 Bearer/sk-/key 的片段。
	lower := strings.ToLower(s)
	if strings.Contains(lower, "bearer ") || strings.Contains(lower, "api_key") || strings.Contains(lower, "apikey") || strings.Contains(s, "sk-") {
		return "upstream error (details redacted)"
	}
	return s
}

func buildAnthropicRequest(body map[string]any, model string, stream bool) (anthropicRequest, error) {
	if err := rejectUnsupportedConversionFeatures(body, "openai->anthropic"); err != nil {
		return anthropicRequest{}, err
	}
	req := anthropicRequest{
		Model:     model,
		MaxTokens: 1024,
		Stream:    stream,
	}
	if maxTokens, ok := numberAsInt(body["max_tokens"]); ok && maxTokens > 0 {
		req.MaxTokens = maxTokens
	}
	req.Temperature = body["temperature"]
	req.TopP = body["top_p"]
	if stop, ok := body["stop"]; ok {
		normalizedStop, err := normalizeStopSequences(stop)
		if err != nil {
			return req, err
		}
		req.Stop = normalizedStop
	}

	messages, ok := body["messages"].([]any)
	if !ok || len(messages) == 0 {
		return req, fmt.Errorf("messages must be a non-empty array")
	}
	var system []string
	for _, item := range messages {
		message, ok := item.(map[string]any)
		if !ok {
			return req, fmt.Errorf("each message must be an object")
		}
		role, _ := message["role"].(string)
		if hasNonEmptyConversionFeature(message["tool_calls"]) {
			return req, fmt.Errorf("protocol conversion does not support tool_calls; use a native openai provider")
		}
		if role == "tool" {
			return req, fmt.Errorf("protocol conversion does not support tool role messages; use a native openai provider")
		}
		content, err := extractTextContent(message["content"], "openai->anthropic")
		if err != nil {
			return req, err
		}
		switch role {
		case "system":
			if content != "" {
				system = append(system, content)
			}
		case "assistant", "user":
			req.Messages = append(req.Messages, anthropicMessage{Role: role, Content: content})
		default:
			return req, fmt.Errorf("protocol conversion does not support role %q; use a native provider", role)
		}
	}
	req.System = strings.Join(system, "\n\n")
	if len(req.Messages) == 0 {
		return req, fmt.Errorf("messages must include at least one user or assistant message")
	}
	return req, nil
}

func buildOpenAIChatFromAnthropic(body map[string]any, model string, stream bool) ([]byte, error) {
	if err := rejectUnsupportedConversionFeatures(body, "anthropic->openai"); err != nil {
		return nil, err
	}
	openAI := map[string]any{
		"model":  model,
		"stream": stream,
	}
	if maxTokens, ok := numberAsInt(body["max_tokens"]); ok && maxTokens > 0 {
		openAI["max_tokens"] = maxTokens
	}
	if temp, ok := body["temperature"]; ok {
		openAI["temperature"] = temp
	}
	if topP, ok := body["top_p"]; ok {
		openAI["top_p"] = topP
	}
	if stop, ok := body["stop_sequences"]; ok {
		openAI["stop"] = stop
	}

	messages := make([]any, 0)
	if systemRaw, ok := body["system"]; ok && systemRaw != nil {
		system, err := extractTextContent(systemRaw, "anthropic->openai")
		if err != nil {
			return nil, err
		}
		if system != "" {
			messages = append(messages, map[string]any{"role": "system", "content": system})
		}
	}
	rawMessages, ok := body["messages"].([]any)
	if !ok || len(rawMessages) == 0 {
		return nil, fmt.Errorf("messages must be a non-empty array")
	}
	for _, item := range rawMessages {
		message, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("each message must be an object")
		}
		role, _ := message["role"].(string)
		switch role {
		case "user", "assistant":
		default:
			return nil, fmt.Errorf("protocol conversion does not support role %q; use a native provider", role)
		}
		content, err := extractTextContent(message["content"], "anthropic->openai")
		if err != nil {
			return nil, err
		}
		messages = append(messages, map[string]any{"role": role, "content": content})
	}
	openAI["messages"] = messages
	return json.Marshal(openAI)
}

// hasNonEmptyConversionFeature 判断上游可选字段是否包含真实不支持内容。
// null / 缺失 / 空数组 / 空对象视为无 feature,允许基础文本转换继续。

// normalizeStopSequences 将 OpenAI stop(string|[]string) 规范为 Anthropic stop_sequences 数组。
// nil/空字符串/空数组返回 nil(省略字段);其它类型在访问上游前拒绝。
func normalizeStopSequences(stop any) (any, error) {
	switch v := stop.(type) {
	case nil:
		return nil, nil
	case string:
		s := strings.TrimSpace(v)
		if s == "" {
			return nil, nil
		}
		return []string{s}, nil
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			s, ok := item.(string)
			if !ok {
				return nil, fmt.Errorf("openai->anthropic conversion requires stop to contain only strings")
			}
			s = strings.TrimSpace(s)
			if s != "" {
				out = append(out, s)
			}
		}
		if len(out) == 0 {
			return nil, nil
		}
		return out, nil
	case []string:
		out := make([]string, 0, len(v))
		for _, s := range v {
			s = strings.TrimSpace(s)
			if s != "" {
				out = append(out, s)
			}
		}
		if len(out) == 0 {
			return nil, nil
		}
		return out, nil
	default:
		return nil, fmt.Errorf("openai->anthropic conversion requires stop to be a string or string array")
	}
}

// conversionStreamTextDelta 仅接受字符串 content delta。
// 数组/对象等多模态形态在转换流中必须终止,不得 flatten 成伪文本。
func conversionStreamTextDelta(value any) (string, error) {
	switch v := value.(type) {
	case nil:
		return "", nil
	case string:
		return v, nil
	default:
		return "", fmt.Errorf("protocol conversion does not support non-text stream content; use a native provider")
	}
}

func hasNonEmptyConversionFeature(value any) bool {
	switch v := value.(type) {
	case nil:
		return false
	case string:
		return strings.TrimSpace(v) != ""
	case []any:
		return len(v) > 0
	case map[string]any:
		return len(v) > 0
	default:
		// 其它 JSON 类型(数字/bool 等)视为存在 feature。
		return true
	}
}

// rejectUnsupportedConversionFeatures 拒绝协议转换无法保真的能力,避免静默破坏语义。
func rejectUnsupportedConversionFeatures(body map[string]any, direction string) error {
	unsupportedKeys := []string{"tools", "tool_choice", "functions", "function_call", "response_format"}
	for _, key := range unsupportedKeys {
		if _, ok := body[key]; ok {
			return fmt.Errorf("%s conversion does not support %q; use a native provider", direction, key)
		}
	}
	return nil
}

// extractTextContent 仅接受纯文本或文本 content blocks;图片/工具等明确拒绝。
func extractTextContent(value any, direction string) (string, error) {
	switch v := value.(type) {
	case nil:
		return "", nil
	case string:
		return v, nil
	case []any:
		parts := make([]string, 0, len(v))
		for _, item := range v {
			switch part := item.(type) {
			case string:
				parts = append(parts, part)
			case map[string]any:
				typ, _ := part["type"].(string)
				switch typ {
				case "", "text":
					text, _ := part["text"].(string)
					if text == "" {
						// OpenAI 某些块用 content 字段
						text, _ = part["content"].(string)
					}
					parts = append(parts, text)
				case "image", "image_url", "input_image", "tool_use", "tool_result", "input_audio", "audio", "document", "file":
					return "", fmt.Errorf("%s conversion does not support content type %q; use a native provider", direction, typ)
				default:
					return "", fmt.Errorf("%s conversion does not support content type %q; use a native provider", direction, typ)
				}
			default:
				return "", fmt.Errorf("%s conversion requires string or text content blocks", direction)
			}
		}
		return strings.Join(parts, ""), nil
	case map[string]any:
		// Anthropic system 可为 [{type:text,text:...}]
		typ, _ := v["type"].(string)
		if typ == "" || typ == "text" {
			text, _ := v["text"].(string)
			return text, nil
		}
		return "", fmt.Errorf("%s conversion does not support content type %q; use a native provider", direction, typ)
	default:
		return "", fmt.Errorf("%s conversion requires string or text content blocks", direction)
	}
}

func (h *Handler) handleAnthropicBuffered(w http.ResponseWriter, r *http.Request, resp *http.Response, round *archive.Round, start time.Time, providerName, model string, requestBody map[string]any) {
	body, err := h.readLimitedUpstream(resp.Body)
	if err != nil {
		h.writeArchivedError(w, round, r, start, providerName, model, false, http.StatusBadGateway, err.Error())
		return
	}
	openAIBody, usage, err := convertAnthropicResponse(body, model)
	if err != nil {
		h.writeArchivedError(w, round, r, start, providerName, model, false, http.StatusBadGateway, err.Error())
		return
	}
	if !usage.Known {
		usage = tokenUsage{
			PromptTokens:     estimatePromptTokens(requestBody),
			CompletionTokens: estimateCompletionTokensFromResponse(openAIBody),
			Estimated:        true,
			Known:            true,
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(openAIBody)
	if err := round.WriteResponse("response.json", openAIBody); err != nil {
		log.Printf("archive anthropic response: %v", err)
	}
	duration := time.Since(start)
	h.recordAndPrint(round, r, providerName, model, false, http.StatusOK, duration, usage, "")
	h.writeArchiveMetadata(round, providerName, model, false, http.StatusOK, duration, usage, "response.json", "", "", "success")
}

func (h *Handler) handleOpenAIToAnthropicBuffered(w http.ResponseWriter, r *http.Request, resp *http.Response, round *archive.Round, start time.Time, providerName, model string, requestBody map[string]any) {
	body, err := h.readLimitedUpstream(resp.Body)
	if err != nil {
		h.writeArchivedError(w, round, r, start, providerName, model, false, http.StatusBadGateway, err.Error())
		return
	}
	anthropicBody, usage, err := convertOpenAIChatToAnthropicResponse(body, model)
	if err != nil {
		h.writeArchivedError(w, round, r, start, providerName, model, false, http.StatusBadGateway, err.Error())
		return
	}
	if !usage.Known {
		usage = tokenUsage{
			PromptTokens:     estimatePromptTokens(requestBody),
			CompletionTokens: estimateCompletionTokensFromResponse(body),
			Estimated:        true,
			Known:            true,
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(anthropicBody)
	if err := round.WriteResponse("response.json", anthropicBody); err != nil {
		log.Printf("archive openai→anthropic response: %v", err)
	}
	duration := time.Since(start)
	h.recordAndPrint(round, r, providerName, model, false, http.StatusOK, duration, usage, "")
	h.writeArchiveMetadata(round, providerName, model, false, http.StatusOK, duration, usage, "response.json", "", "", "success")
}

func convertAnthropicResponse(body []byte, fallbackModel string) ([]byte, tokenUsage, error) {
	var payload struct {
		ID         string `json:"id"`
		Model      string `json:"model"`
		StopReason string `json:"stop_reason"`
		Content    []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		Usage struct {
			InputTokens              int `json:"input_tokens"`
			OutputTokens             int `json:"output_tokens"`
			CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
			CacheReadInputTokens     int `json:"cache_read_input_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, tokenUsage{}, err
	}
	var text strings.Builder
	for _, part := range payload.Content {
		switch part.Type {
		case "text", "":
			text.WriteString(part.Text)
		default:
			return nil, tokenUsage{}, fmt.Errorf("protocol conversion does not support response content type %q; use a native provider", part.Type)
		}
	}
	model := payload.Model
	if model == "" {
		model = fallbackModel
	}
	usage := tokenUsage{
		PromptTokens:             payload.Usage.InputTokens,
		CompletionTokens:         payload.Usage.OutputTokens,
		CachedInputTokens:        payload.Usage.CacheReadInputTokens,
		CacheCreationInputTokens: payload.Usage.CacheCreationInputTokens,
		Known:                    payload.Usage.InputTokens > 0 || payload.Usage.OutputTokens > 0,
	}
	response := map[string]any{
		"id":      firstNonEmpty(payload.ID, "chatcmpl-anthropic"),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []any{
			map[string]any{
				"index": 0,
				"message": map[string]any{
					"role":    "assistant",
					"content": text.String(),
				},
				"finish_reason": anthropicStopReason(payload.StopReason),
			},
		},
		"usage": openAIUsagePayload(usage),
	}
	encoded, err := json.Marshal(response)
	return encoded, usage, err
}

func convertOpenAIChatToAnthropicResponse(body []byte, fallbackModel string) ([]byte, tokenUsage, error) {
	var payload struct {
		ID      string `json:"id"`
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Content      any `json:"content"`
				ToolCalls    any `json:"tool_calls"`
				FunctionCall any `json:"function_call"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, tokenUsage{}, err
	}
	content := ""
	stopReason := "end_turn"
	if len(payload.Choices) > 0 {
		msg := payload.Choices[0].Message
		// 上游常回传 "tool_calls": [] / "function_call": null;仅非空才视为不支持。
		if hasNonEmptyConversionFeature(msg.ToolCalls) {
			return nil, tokenUsage{}, fmt.Errorf("protocol conversion does not support response tool_calls; use a native provider")
		}
		if hasNonEmptyConversionFeature(msg.FunctionCall) {
			return nil, tokenUsage{}, fmt.Errorf("protocol conversion does not support response function_call; use a native provider")
		}
		text, err := extractTextContent(msg.Content, "openai->anthropic response")
		if err != nil {
			return nil, tokenUsage{}, err
		}
		content = text
		if fr := payload.Choices[0].FinishReason; fr == "tool_calls" || fr == "function_call" {
			return nil, tokenUsage{}, fmt.Errorf("protocol conversion does not support finish_reason %q; use a native provider", fr)
		}
		stopReason = openAIStopReasonToAnthropic(payload.Choices[0].FinishReason)
	}
	model := payload.Model
	if model == "" {
		model = fallbackModel
	}
	usage := tokenUsage{
		PromptTokens:     payload.Usage.PromptTokens,
		CompletionTokens: payload.Usage.CompletionTokens,
		Known:            payload.Usage.PromptTokens > 0 || payload.Usage.CompletionTokens > 0,
	}
	response := map[string]any{
		"id":          firstNonEmpty(payload.ID, "msg_openai"),
		"type":        "message",
		"role":        "assistant",
		"model":       model,
		"content":     []any{map[string]any{"type": "text", "text": content}},
		"stop_reason": stopReason,
		"usage": map[string]any{
			"input_tokens":  usage.PromptTokens,
			"output_tokens": usage.CompletionTokens,
		},
	}
	encoded, err := json.Marshal(response)
	return encoded, usage, err
}

func (h *Handler) handleAnthropicStream(w http.ResponseWriter, r *http.Request, resp *http.Response, round *archive.Round, start time.Time, providerName, model string, requestBody map[string]any, requestContext context.Context, cancel context.CancelFunc) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	archiveWriter, err := round.CreateResponseWriter("response.sse")
	if err != nil {
		log.Printf("archive anthropic stream response: %v", err)
	}
	if archiveWriter != nil {
		defer archiveWriter.Close()
	}

	reader := bufio.NewReader(resp.Body)
	created := time.Now().Unix()
	id := "chatcmpl-anthropic"
	currentModel := model
	usage := tokenUsage{Known: true}
	var content strings.Builder
	finishReason := "stop"
	roleSent := false
	maxStream, maxLine := h.streamLimits()
	idleTimer, stopIdleTimer := h.startStreamIdleTimer(cancel)
	defer stopIdleTimer()
	var streamErr *streamFail
	var upstreamBytes, totalBytes int64
	sawTerminal := false

	for {
		line, err := readSSELine(reader, maxLine)
		if len(line) > 0 {
			resetStreamIdleTimer(idleTimer, h.cfg.StreamIdleTimeout)
			upstreamBytes += int64(len(line))
			if upstreamBytes > maxStream {
				streamErr = h.logStreamIssue(round, providerName, model, "read anthropic stream limit", fmt.Errorf("upstream stream exceeds limit of %d bytes", maxStream), requestContext, nil)
				break
			}
			text := strings.TrimSpace(string(line))
			if strings.HasPrefix(text, "data:") {
				rest, _ := strings.CutPrefix(text, "data:")
				payload := strings.TrimSpace(rest)
				if payload != "" {
					terminal := isTerminalAnthropicPayload(payload)
					if terminal {
						sawTerminal = true
					}
					events, convErr := anthropicStreamEvents(payload, &id, &currentModel, &usage, &content, &finishReason, &roleSent, created)
					if convErr != nil {
						// 在产生点明确 kind:非法 JSON=protocol;不支持内容=conversion
						kind := streamKindConversion
						countUp := false
						if strings.Contains(strings.ToLower(convErr.Error()), "invalid sse json") {
							kind = streamKindProtocol
							countUp = true
						}
						streamErr = newStreamFail(kind, fmt.Sprintf("convert anthropic stream: %v", convErr), convErr, countUp)
						h.logStreamFail(round, providerName, model, streamErr)
						break
					}
					for _, event := range events {
						if totalBytes+int64(len(event)) > maxStream {
							streamErr = h.logStreamIssue(round, providerName, model, "write anthropic stream client limit", fmt.Errorf("downstream stream exceeds limit of %d bytes", maxStream), requestContext, nil)
							break
						}
						if _, writeErr := w.Write(event); writeErr != nil {
							streamErr = h.logStreamIssue(round, providerName, model, "write anthropic stream client", writeErr, requestContext, nil)
							break
						}
						totalBytes += int64(len(event))
						if archiveWriter != nil {
							if _, writeErr := archiveWriter.Write(event); writeErr != nil {
								h.logStreamIssue(round, providerName, model, "write anthropic archive stream", writeErr, nil, nil)
							}
						}
						if flusher != nil {
							flusher.Flush()
						}
					}
					if streamErr != nil {
						break
					}
					// 终止事件已转换写出后立即退出,避免 idle timeout 覆盖成功结果。
					if terminal {
						break
					}
				}
			}
		}
		if err != nil {
			if err != io.EOF {
				streamErr = h.logStreamIssue(round, providerName, model, "read anthropic stream", err, requestContext, idleTimer)
			} else if !sawTerminal {
				streamErr = h.logStreamIssue(round, providerName, model, "read anthropic stream", fmt.Errorf("upstream stream ended without message_stop"), requestContext, idleTimer)
			}
			break
		}
		if streamErr != nil {
			break
		}
	}

	if usage.PromptTokens == 0 {
		usage.PromptTokens = estimatePromptTokens(requestBody)
		usage.Estimated = true
	}
	if usage.CompletionTokens == 0 {
		usage.CompletionTokens = estimateTokens(content.String())
		usage.Estimated = true
	}
	// 仅在无错误时发送正常终止事件;终止事件也计入下游流量上限。
	if streamErr == nil {
		usageChunk := openAIStreamChunk(id, currentModel, created, []any{}, usage)
		usageEvent := fmt.Appendf(nil, "data: %s\n\n", usageChunk)
		doneEvent := []byte("data: [DONE]\n\n")
		for _, event := range [][]byte{usageEvent, doneEvent} {
			if totalBytes+int64(len(event)) > maxStream {
				streamErr = h.logStreamIssue(round, providerName, model, "write anthropic stream client limit", fmt.Errorf("downstream stream exceeds limit of %d bytes", maxStream), requestContext, nil)
				break
			}
			if _, writeErr := w.Write(event); writeErr != nil {
				streamErr = h.logStreamIssue(round, providerName, model, "write anthropic stream client", writeErr, requestContext, nil)
				break
			}
			totalBytes += int64(len(event))
			if archiveWriter != nil {
				if _, writeErr := archiveWriter.Write(event); writeErr != nil {
					h.logStreamIssue(round, providerName, model, "write anthropic archive stream", writeErr, nil, nil)
				}
			}
		}
		if streamErr == nil && flusher != nil {
			flusher.Flush()
		}
	}
	fullPath := ""
	if streamErr == nil {
		if fullResponse, err := streamCompletionJSON(id, currentModel, created, "assistant", content.String(), finishReason, usage); err != nil {
			log.Printf("build anthropic stream full response: %v", err)
		} else if err := round.WriteResponse("response.json", append(fullResponse, '\n')); err != nil {
			log.Printf("archive anthropic stream full response: %v", err)
		} else {
			fullPath = "response.json"
		}
	}
	duration := time.Since(start)
	errMsg := ""
	if streamErr != nil {
		errMsg = streamErr.Error()
	}
	h.recordAndPrintFail(round, r, providerName, model, true, http.StatusOK, duration, usage, streamErr)
	h.writeArchiveMetadata(round, providerName, model, true, http.StatusOK, duration, usage, "response.sse", errMsg, fullPath, outcomeFromStreamFail(streamErr, http.StatusOK))
}

// handleOpenAIToAnthropicStream 将 OpenAI SSE 转成 Anthropic Messages SSE（基础文本）。
func (h *Handler) handleOpenAIToAnthropicStream(w http.ResponseWriter, r *http.Request, resp *http.Response, round *archive.Round, start time.Time, providerName, model string, requestBody map[string]any, requestContext context.Context, cancel context.CancelFunc) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	archiveWriter, err := round.CreateResponseWriter("response.sse")
	if err != nil {
		log.Printf("archive openai→anthropic stream: %v", err)
	}
	if archiveWriter != nil {
		defer archiveWriter.Close()
	}

	reader := bufio.NewReader(resp.Body)
	id := "msg_openai"
	currentModel := model
	usage := tokenUsage{Known: true}
	var content strings.Builder
	stopReason := "end_turn"
	started := false
	maxStream, maxLine := h.streamLimits()
	idleTimer, stopIdleTimer := h.startStreamIdleTimer(cancel)
	defer stopIdleTimer()
	var streamErr *streamFail
	var upstreamBytes, totalBytes int64
	sawTerminal := false

	writeEvent := func(eventType string, payload any) {
		if streamErr != nil {
			return
		}
		data, _ := json.Marshal(payload)
		event := []byte("event: " + eventType + "\ndata: " + string(data) + "\n\n")
		if totalBytes+int64(len(event)) > maxStream {
			streamErr = h.logStreamIssue(round, providerName, model, "write openai→anthropic stream client limit", fmt.Errorf("downstream stream exceeds limit of %d bytes", maxStream), requestContext, nil)
			return
		}
		if _, writeErr := w.Write(event); writeErr != nil {
			streamErr = h.logStreamIssue(round, providerName, model, "write openai→anthropic stream client", writeErr, requestContext, nil)
			return
		}
		totalBytes += int64(len(event))
		if archiveWriter != nil {
			if _, writeErr := archiveWriter.Write(event); writeErr != nil {
				h.logStreamIssue(round, providerName, model, "write openai→anthropic archive stream", writeErr, nil, nil)
			}
		}
		if flusher != nil {
			flusher.Flush()
		}
	}

	for {
		line, err := readSSELine(reader, maxLine)
		if len(line) > 0 {
			resetStreamIdleTimer(idleTimer, h.cfg.StreamIdleTimeout)
			upstreamBytes += int64(len(line))
			if upstreamBytes > maxStream {
				streamErr = h.logStreamIssue(round, providerName, model, "read openai→anthropic stream limit", fmt.Errorf("upstream stream exceeds limit of %d bytes", maxStream), requestContext, nil)
				break
			}
			text := strings.TrimSpace(string(line))
			if !strings.HasPrefix(text, "data:") {
				continue
			}
			rest, _ := strings.CutPrefix(text, "data:")
			payload := strings.TrimSpace(rest)
			if payload == "" {
				continue
			}
			if payload == "[DONE]" {
				sawTerminal = true
				// 终止事件后立即退出,随后补发 Anthropic 终止事件。
				break
			}
			var chunk map[string]any
			if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
				uerr := fmt.Errorf("invalid SSE JSON: %w", err)
				streamErr = newStreamFail(streamKindProtocol, fmt.Sprintf("protocol openai→anthropic stream: %v", uerr), uerr, true)
				h.logStreamFail(round, providerName, model, streamErr)
				break
			}
			if value, ok := chunk["id"].(string); ok && value != "" {
				id = value
			}
			if value, ok := chunk["model"].(string); ok && value != "" {
				currentModel = value
			}
			if rawUsage, ok := chunk["usage"].(map[string]any); ok {
				if n, ok := numberAsInt(rawUsage["prompt_tokens"]); ok {
					usage.PromptTokens = n
					usage.Known = true
				}
				if n, ok := numberAsInt(rawUsage["completion_tokens"]); ok {
					usage.CompletionTokens = n
					usage.Known = true
				}
			}
			if !started {
				started = true
				writeEvent("message_start", map[string]any{
					"type": "message_start",
					"message": map[string]any{
						"id":    id,
						"type":  "message",
						"role":  "assistant",
						"model": currentModel,
						"usage": map[string]any{"input_tokens": usage.PromptTokens, "output_tokens": 0},
					},
				})
				if streamErr != nil {
					break
				}
				writeEvent("content_block_start", map[string]any{
					"type":  "content_block_start",
					"index": 0,
					"content_block": map[string]any{
						"type": "text",
						"text": "",
					},
				})
				if streamErr != nil {
					break
				}
			}
			if choices, ok := chunk["choices"].([]any); ok && len(choices) > 0 {
				choice, _ := choices[0].(map[string]any)
				if delta, ok := choice["delta"].(map[string]any); ok {
					if hasNonEmptyConversionFeature(delta["tool_calls"]) || hasNonEmptyConversionFeature(delta["function_call"]) {
						streamErr = h.logStreamIssue(round, providerName, model, "convert openai→anthropic stream", fmt.Errorf("protocol conversion does not support streaming tool_calls/function_call; use a native provider"), requestContext, nil)
						break
					}
					textDelta, cerr := conversionStreamTextDelta(delta["content"])
					if cerr != nil {
						streamErr = h.logStreamIssue(round, providerName, model, "convert openai→anthropic stream", cerr, requestContext, nil)
						break
					}
					if textDelta != "" {
						content.WriteString(textDelta)
						writeEvent("content_block_delta", map[string]any{
							"type":  "content_block_delta",
							"index": 0,
							"delta": map[string]any{"type": "text_delta", "text": textDelta},
						})
						if streamErr != nil {
							break
						}
					}
				}
				if reason, ok := choice["finish_reason"].(string); ok && reason != "" {
					if reason == "tool_calls" || reason == "function_call" {
						streamErr = h.logStreamIssue(round, providerName, model, "convert openai→anthropic stream", fmt.Errorf("protocol conversion does not support finish_reason %q; use a native provider", reason), requestContext, nil)
						break
					}
					stopReason = openAIStopReasonToAnthropic(reason)
				}
			}
		}
		if err != nil {
			if err != io.EOF {
				streamErr = h.logStreamIssue(round, providerName, model, "read openai→anthropic stream", err, requestContext, idleTimer)
			} else if !sawTerminal {
				streamErr = h.logStreamIssue(round, providerName, model, "read openai→anthropic stream", fmt.Errorf("upstream stream ended without [DONE]"), requestContext, idleTimer)
			}
			break
		}
		if streamErr != nil {
			break
		}
	}

	if !started && streamErr == nil {
		writeEvent("message_start", map[string]any{
			"type": "message_start",
			"message": map[string]any{
				"id":    id,
				"type":  "message",
				"role":  "assistant",
				"model": currentModel,
				"usage": map[string]any{"input_tokens": 0, "output_tokens": 0},
			},
		})
		writeEvent("content_block_start", map[string]any{
			"type":          "content_block_start",
			"index":         0,
			"content_block": map[string]any{"type": "text", "text": ""},
		})
	}
	if usage.PromptTokens == 0 {
		usage.PromptTokens = estimatePromptTokens(requestBody)
		usage.Estimated = true
	}
	if usage.CompletionTokens == 0 {
		usage.CompletionTokens = estimateTokens(content.String())
		usage.Estimated = true
	}
	if streamErr == nil {
		writeEvent("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
		writeEvent("message_delta", map[string]any{
			"type":  "message_delta",
			"delta": map[string]any{"stop_reason": stopReason},
			"usage": map[string]any{"output_tokens": usage.CompletionTokens},
		})
		writeEvent("message_stop", map[string]any{"type": "message_stop"})
	}

	fullPath := ""
	if streamErr == nil {
		full := map[string]any{
			"id":          id,
			"type":        "message",
			"role":        "assistant",
			"model":       currentModel,
			"content":     []any{map[string]any{"type": "text", "text": content.String()}},
			"stop_reason": stopReason,
			"usage": map[string]any{
				"input_tokens":  usage.PromptTokens,
				"output_tokens": usage.CompletionTokens,
			},
		}
		if encoded, err := json.Marshal(full); err != nil {
			log.Printf("build openai→anthropic full response: %v", err)
		} else if err := round.WriteResponse("response.json", append(encoded, '\n')); err != nil {
			log.Printf("archive openai→anthropic full response: %v", err)
		} else {
			fullPath = "response.json"
		}
	}
	duration := time.Since(start)
	errMsg := ""
	if streamErr != nil {
		errMsg = streamErr.Error()
	}
	h.recordAndPrintFail(round, r, providerName, model, true, http.StatusOK, duration, usage, streamErr)
	h.writeArchiveMetadata(round, providerName, model, true, http.StatusOK, duration, usage, "response.sse", errMsg, fullPath, outcomeFromStreamFail(streamErr, http.StatusOK))
}

func anthropicStreamEvents(payload string, id, model *string, usage *tokenUsage, content *strings.Builder, finishReason *string, roleSent *bool, created int64) ([][]byte, error) {
	var event map[string]any
	if err := json.Unmarshal([]byte(payload), &event); err != nil {
		return nil, fmt.Errorf("invalid SSE JSON: %w", err)
	}
	eventType, _ := event["type"].(string)
	switch eventType {
	case "message_start":
		if message, ok := event["message"].(map[string]any); ok {
			if value, ok := message["id"].(string); ok && value != "" {
				*id = value
			}
			if value, ok := message["model"].(string); ok && value != "" {
				*model = value
			}
			if parsed, ok := anthropicUsage(message["usage"]); ok {
				usage.PromptTokens = parsed.PromptTokens
				usage.CachedInputTokens = parsed.CachedInputTokens
				usage.CacheCreationInputTokens = parsed.CacheCreationInputTokens
				usage.Known = true
			}
		}
		if !*roleSent {
			*roleSent = true
			return [][]byte{sseData(openAIStreamChunk(*id, *model, created, []any{
				map[string]any{"index": 0, "delta": map[string]any{"role": "assistant"}, "finish_reason": nil},
			}, tokenUsage{}))}, nil
		}
	case "content_block_start":
		if block, ok := event["content_block"].(map[string]any); ok {
			typ, _ := block["type"].(string)
			if typ != "" && typ != "text" {
				return nil, fmt.Errorf("protocol conversion does not support streaming content type %q; use a native provider", typ)
			}
		}
	case "content_block_delta":
		if delta, ok := event["delta"].(map[string]any); ok {
			dType, _ := delta["type"].(string)
			if dType != "" && dType != "text_delta" && dType != "text" {
				return nil, fmt.Errorf("protocol conversion does not support streaming delta type %q; use a native provider", dType)
			}
			if text, ok := delta["text"].(string); ok && text != "" {
				content.WriteString(text)
				return [][]byte{sseData(openAIStreamChunk(*id, *model, created, []any{
					map[string]any{"index": 0, "delta": map[string]any{"content": text}, "finish_reason": nil},
				}, tokenUsage{}))}, nil
			}
		}
	case "message_delta":
		if parsed, ok := anthropicUsage(event["usage"]); ok {
			usage.CompletionTokens = parsed.CompletionTokens
			if parsed.CachedInputTokens > 0 {
				usage.CachedInputTokens = parsed.CachedInputTokens
			}
			if parsed.CacheCreationInputTokens > 0 {
				usage.CacheCreationInputTokens = parsed.CacheCreationInputTokens
			}
			usage.Known = true
		}
		if delta, ok := event["delta"].(map[string]any); ok {
			if reason, ok := delta["stop_reason"].(string); ok && reason != "" {
				if reason == "tool_use" || reason == "tool_calls" {
					return nil, fmt.Errorf("protocol conversion does not support stop_reason %q; use a native provider", reason)
				}
				*finishReason = anthropicStopReason(reason)
			}
		}
	case "message_stop":
		return [][]byte{sseData(openAIStreamChunk(*id, *model, created, []any{
			map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": *finishReason},
		}, tokenUsage{}))}, nil
	}
	return nil, nil
}

func anthropicUsage(value any) (tokenUsage, bool) {
	raw, ok := value.(map[string]any)
	if !ok {
		return tokenUsage{}, false
	}
	usage := tokenUsage{Known: true}
	usage.PromptTokens, _ = numberAsInt(raw["input_tokens"])
	usage.CompletionTokens, _ = numberAsInt(raw["output_tokens"])
	applyUsageDetails(&usage, raw)
	return usage, true
}

func openAIStreamChunk(id, model string, created int64, choices []any, usage tokenUsage) []byte {
	chunk := map[string]any{
		"id":      id,
		"object":  "chat.completion.chunk",
		"created": created,
		"model":   model,
		"choices": choices,
	}
	if usage.PromptTokens > 0 || usage.CompletionTokens > 0 {
		chunk["usage"] = openAIUsagePayload(usage)
	}
	encoded, _ := json.Marshal(chunk)
	return encoded
}

func sseData(payload []byte) []byte {
	return []byte("data: " + string(payload) + "\n\n")
}

func numberAsInt(value any) (int, bool) {
	switch v := value.(type) {
	case float64:
		return int(v), true
	case int:
		return v, true
	case json.Number:
		i, err := v.Int64()
		return int(i), err == nil
	default:
		return 0, false
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func anthropicStopReason(reason string) string {
	switch reason {
	case "max_tokens":
		return "length"
	case "stop_sequence", "end_turn", "":
		return "stop"
	default:
		return reason
	}
}

func openAIStopReasonToAnthropic(reason string) string {
	switch reason {
	case "length":
		return "max_tokens"
	case "stop", "":
		return "end_turn"
	default:
		return reason
	}
}
