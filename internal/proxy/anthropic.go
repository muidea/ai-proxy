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

	"ai-proxy/internal/archive"
	"ai-proxy/internal/config"
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
// anthropic provider: 直通; openai provider: 转换为 chat/completions 再回写 Anthropic 形状。
func (h *Handler) handleAnthropicMessages(w http.ResponseWriter, r *http.Request, requestID string) {
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
	h.archiveAndLogProviderSelection(round, r, providerName, provider, model, stream)

	if provider.Protocol == "anthropic" {
		h.forwardAnthropicNative(w, r, round, start, providerName, provider, bodyBytes, body, model, stream)
		return
	}
	if provider.Protocol != "openai" {
		h.writeArchivedError(w, round, r, start, providerName, model, stream, http.StatusBadRequest,
			fmt.Sprintf("unsupported provider protocol %q for /v1/messages", provider.Protocol))
		return
	}
	h.convertAnthropicMessagesToOpenAI(w, r, round, start, providerName, provider, body, model, stream)
}

func (h *Handler) forwardAnthropicNative(w http.ResponseWriter, r *http.Request, round *archive.Round, start time.Time, providerName string, provider config.Provider, bodyBytes []byte, body map[string]any, model string, stream bool) {
	streamRequest := stream || acceptsEventStream(r.Header)
	result, err := h.doUpstreamWithFallback(r, round, providerName, provider, bodyBytes, len(bodyBytes), streamRequest)
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
		copyHeader(w.Header(), resp.Header)
		w.WriteHeader(resp.StatusCode)
		usage, fullPath, streamErr := h.copyAndArchiveRawStream(w, resp, round, providerName, provider, model, body, r.Context(), result.Cancel)
		duration := time.Since(start)
		h.recordAndPrint(round, r, providerName, model, true, resp.StatusCode, duration, usage, streamErr)
		h.writeArchiveMetadata(round, providerName, model, true, resp.StatusCode, duration, usage, responsePath, streamErr, fullPath)
		return
	}
	responseBody, err := io.ReadAll(resp.Body)
	readErrMessage := ""
	if err != nil {
		readErrMessage = h.logStreamIssue(round, providerName, model, "read anthropic response", err, nil, nil)
	}
	responseBody, responseHeader := decodedResponseBodyAndHeader(responseBody, resp.Header)
	copyHeader(w.Header(), responseHeader)
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
	h.recordAndPrint(round, r, providerName, model, stream, resp.StatusCode, duration, usage, readErrMessage)
	h.writeArchiveMetadata(round, providerName, model, stream, resp.StatusCode, duration, usage, responsePath, readErrMessage, "")
}

func (h *Handler) convertAnthropicMessagesToOpenAI(w http.ResponseWriter, r *http.Request, round *archive.Round, start time.Time, providerName string, provider config.Provider, body map[string]any, model string, stream bool) {
	openAIBody, err := buildOpenAIChatFromAnthropic(body, model, stream)
	if err != nil {
		h.writeArchivedError(w, round, r, start, providerName, model, stream, http.StatusBadRequest, err.Error())
		return
	}
	result, err := h.doUpstreamWithFallbackPath(r, round, providerName, provider, openAIBody, len(openAIBody), stream, "/v1/chat/completions", "", http.MethodPost)
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
		responseBody, err := io.ReadAll(resp.Body)
		if err != nil {
			h.writeArchivedError(w, round, r, start, providerName, model, stream, http.StatusBadGateway, err.Error())
			return
		}
		copyHeader(w.Header(), resp.Header)
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(responseBody)
		responsePath := responseFileName(resp.Header.Get("Content-Type"), false)
		if err := round.WriteResponse(responsePath, responseBody); err != nil {
			log.Printf("archive converted anthropic error response: %v", err)
		}
		duration := time.Since(start)
		h.recordAndPrint(round, r, providerName, model, stream, resp.StatusCode, duration, tokenUsage{}, "")
		h.writeArchiveMetadata(round, providerName, model, stream, resp.StatusCode, duration, tokenUsage{}, responsePath, "", "")
		return
	}
	if stream {
		h.handleOpenAIToAnthropicStream(w, r, resp, round, start, providerName, model, body, r.Context(), result.Cancel)
		return
	}
	h.handleOpenAIToAnthropicBuffered(w, r, resp, round, start, providerName, model, body)
}

// handleAnthropicChatCompletions: OpenAI 客户端 /v1/chat/completions → Anthropic 上游 /v1/messages。
func (h *Handler) handleAnthropicChatCompletions(w http.ResponseWriter, r *http.Request, round *archive.Round, start time.Time, providerName string, provider config.Provider, _ []byte, body map[string]any, model string, stream bool) {
	payload, err := buildAnthropicRequest(body, model, stream)
	if err != nil {
		h.writeArchivedError(w, round, r, start, providerName, model, stream, http.StatusBadRequest, err.Error())
		return
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		h.writeArchivedError(w, round, r, start, providerName, model, stream, http.StatusBadRequest, err.Error())
		return
	}

	result, err := h.doUpstreamWithFallbackPath(r, round, providerName, provider, encoded, len(encoded), stream, "/v1/messages", "", http.MethodPost)
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
		responseBody, err := io.ReadAll(resp.Body)
		if err != nil {
			h.writeArchivedError(w, round, r, start, providerName, model, stream, http.StatusBadGateway, err.Error())
			return
		}
		copyHeader(w.Header(), resp.Header)
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(responseBody)
		responsePath := responseFileName(resp.Header.Get("Content-Type"), false)
		if err := round.WriteResponse(responsePath, responseBody); err != nil {
			log.Printf("archive anthropic error response: %v", err)
		}
		duration := time.Since(start)
		h.recordAndPrint(round, r, providerName, model, stream, resp.StatusCode, duration, tokenUsage{}, "")
		h.writeArchiveMetadata(round, providerName, model, stream, resp.StatusCode, duration, tokenUsage{}, responsePath, "", "")
		return
	}
	if stream {
		h.handleAnthropicStream(w, r, resp, round, start, providerName, model, body, r.Context(), result.Cancel)
		return
	}
	h.handleAnthropicBuffered(w, r, resp, round, start, providerName, model, body)
}

func buildAnthropicRequest(body map[string]any, model string, stream bool) (anthropicRequest, error) {
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
		req.Stop = stop
	}

	messages, ok := body["messages"].([]any)
	if !ok || len(messages) == 0 {
		return req, fmt.Errorf("messages must be a non-empty array")
	}
	var system []string
	for _, item := range messages {
		message, _ := item.(map[string]any)
		role, _ := message["role"].(string)
		content := flattenValue(message["content"])
		switch role {
		case "system":
			if content != "" {
				system = append(system, content)
			}
		case "assistant", "user":
			req.Messages = append(req.Messages, anthropicMessage{Role: role, Content: content})
		default:
			req.Messages = append(req.Messages, anthropicMessage{Role: "user", Content: content})
		}
	}
	req.System = strings.Join(system, "\n\n")
	if len(req.Messages) == 0 {
		return req, fmt.Errorf("messages must include at least one user or assistant message")
	}
	return req, nil
}

func buildOpenAIChatFromAnthropic(body map[string]any, model string, stream bool) ([]byte, error) {
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
	if system := flattenValue(body["system"]); system != "" {
		messages = append(messages, map[string]any{"role": "system", "content": system})
	}
	rawMessages, ok := body["messages"].([]any)
	if !ok || len(rawMessages) == 0 {
		return nil, fmt.Errorf("messages must be a non-empty array")
	}
	for _, item := range rawMessages {
		message, _ := item.(map[string]any)
		role, _ := message["role"].(string)
		if role == "" {
			role = "user"
		}
		content := flattenValue(message["content"])
		messages = append(messages, map[string]any{"role": role, "content": content})
	}
	openAI["messages"] = messages
	return json.Marshal(openAI)
}

func (h *Handler) handleAnthropicBuffered(w http.ResponseWriter, r *http.Request, resp *http.Response, round *archive.Round, start time.Time, providerName, model string, requestBody map[string]any) {
	body, err := io.ReadAll(resp.Body)
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
	h.writeArchiveMetadata(round, providerName, model, false, http.StatusOK, duration, usage, "response.json", "", "")
}

func (h *Handler) handleOpenAIToAnthropicBuffered(w http.ResponseWriter, r *http.Request, resp *http.Response, round *archive.Round, start time.Time, providerName, model string, requestBody map[string]any) {
	body, err := io.ReadAll(resp.Body)
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
	h.writeArchiveMetadata(round, providerName, model, false, http.StatusOK, duration, usage, "response.json", "", "")
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
		if part.Type == "text" {
			text.WriteString(part.Text)
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
				Content any `json:"content"`
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
		content = flattenValue(payload.Choices[0].Message.Content)
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
	idleTimer, stopIdleTimer := h.startStreamIdleTimer(cancel)
	defer stopIdleTimer()
	streamErr := ""

	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			resetStreamIdleTimer(idleTimer, h.cfg.StreamIdleTimeout)
			text := strings.TrimSpace(string(line))
			if strings.HasPrefix(text, "data:") {
				rest, _ := strings.CutPrefix(text, "data:")
				payload := strings.TrimSpace(rest)
				if payload != "" {
					events := anthropicStreamEvents(payload, &id, &currentModel, &usage, &content, &finishReason, &roleSent, created)
					for _, event := range events {
						if _, writeErr := w.Write(event); writeErr != nil {
							streamErr = h.logStreamIssue(round, providerName, model, "write anthropic stream client", writeErr, requestContext, nil)
							break
						}
						if archiveWriter != nil {
							if _, writeErr := archiveWriter.Write(event); writeErr != nil {
								h.logStreamIssue(round, providerName, model, "write anthropic archive stream", writeErr, nil, nil)
							}
						}
						if flusher != nil {
							flusher.Flush()
						}
					}
					if streamErr != "" {
						break
					}
				}
			}
		}
		if err != nil {
			if err != io.EOF {
				streamErr = h.logStreamIssue(round, providerName, model, "read anthropic stream", err, requestContext, idleTimer)
			}
			break
		}
		if streamErr != "" {
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
	usageChunk := openAIStreamChunk(id, currentModel, created, []any{}, usage)
	usageEvent := fmt.Appendf(nil, "data: %s\n\n", usageChunk)
	doneEvent := []byte("data: [DONE]\n\n")
	_, _ = w.Write(usageEvent)
	_, _ = w.Write(doneEvent)
	if archiveWriter != nil {
		if _, writeErr := archiveWriter.Write(usageEvent); writeErr != nil {
			log.Printf("write anthropic archive usage: %v", writeErr)
		}
		if _, writeErr := archiveWriter.Write(doneEvent); writeErr != nil {
			log.Printf("write anthropic archive done: %v", writeErr)
		}
	}
	if flusher != nil {
		flusher.Flush()
	}
	if fullResponse, err := streamCompletionJSON(id, currentModel, created, "assistant", content.String(), finishReason, usage); err != nil {
		log.Printf("build anthropic stream full response: %v", err)
	} else if err := round.WriteResponse("response.json", append(fullResponse, '\n')); err != nil {
		log.Printf("archive anthropic stream full response: %v", err)
	}
	duration := time.Since(start)
	h.recordAndPrint(round, r, providerName, model, true, http.StatusOK, duration, usage, streamErr)
	h.writeArchiveMetadata(round, providerName, model, true, http.StatusOK, duration, usage, "response.sse", streamErr, "response.json")
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
	idleTimer, stopIdleTimer := h.startStreamIdleTimer(cancel)
	defer stopIdleTimer()
	streamErr := ""

	writeEvent := func(eventType string, payload any) {
		data, _ := json.Marshal(payload)
		event := []byte("event: " + eventType + "\ndata: " + string(data) + "\n\n")
		if _, writeErr := w.Write(event); writeErr != nil {
			streamErr = h.logStreamIssue(round, providerName, model, "write openai→anthropic stream client", writeErr, requestContext, nil)
			return
		}
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
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			resetStreamIdleTimer(idleTimer, h.cfg.StreamIdleTimeout)
			text := strings.TrimSpace(string(line))
			if !strings.HasPrefix(text, "data:") {
				continue
			}
			rest, _ := strings.CutPrefix(text, "data:")
			payload := strings.TrimSpace(rest)
			if payload == "" || payload == "[DONE]" {
				continue
			}
			var chunk map[string]any
			if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
				continue
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
				if streamErr != "" {
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
				if streamErr != "" {
					break
				}
			}
			if choices, ok := chunk["choices"].([]any); ok && len(choices) > 0 {
				choice, _ := choices[0].(map[string]any)
				if delta, ok := choice["delta"].(map[string]any); ok {
					if textDelta := flattenValue(delta["content"]); textDelta != "" {
						content.WriteString(textDelta)
						writeEvent("content_block_delta", map[string]any{
							"type":  "content_block_delta",
							"index": 0,
							"delta": map[string]any{"type": "text_delta", "text": textDelta},
						})
						if streamErr != "" {
							break
						}
					}
				}
				if reason, ok := choice["finish_reason"].(string); ok && reason != "" {
					stopReason = openAIStopReasonToAnthropic(reason)
				}
			}
		}
		if err != nil {
			if err != io.EOF {
				streamErr = h.logStreamIssue(round, providerName, model, "read openai→anthropic stream", err, requestContext, idleTimer)
			}
			break
		}
		if streamErr != "" {
			break
		}
	}

	if !started {
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
	writeEvent("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
	writeEvent("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": stopReason},
		"usage": map[string]any{"output_tokens": usage.CompletionTokens},
	})
	writeEvent("message_stop", map[string]any{"type": "message_stop"})

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
	}
	duration := time.Since(start)
	h.recordAndPrint(round, r, providerName, model, true, http.StatusOK, duration, usage, streamErr)
	h.writeArchiveMetadata(round, providerName, model, true, http.StatusOK, duration, usage, "response.sse", streamErr, "response.json")
}

func anthropicStreamEvents(payload string, id, model *string, usage *tokenUsage, content *strings.Builder, finishReason *string, roleSent *bool, created int64) [][]byte {
	var event map[string]any
	if err := json.Unmarshal([]byte(payload), &event); err != nil {
		return nil
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
			}, tokenUsage{}))}
		}
	case "content_block_delta":
		if delta, ok := event["delta"].(map[string]any); ok {
			if text, ok := delta["text"].(string); ok && text != "" {
				content.WriteString(text)
				return [][]byte{sseData(openAIStreamChunk(*id, *model, created, []any{
					map[string]any{"index": 0, "delta": map[string]any{"content": text}, "finish_reason": nil},
				}, tokenUsage{}))}
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
				*finishReason = anthropicStopReason(reason)
			}
		}
	case "message_stop":
		return [][]byte{sseData(openAIStreamChunk(*id, *model, created, []any{
			map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": *finishReason},
		}, tokenUsage{}))}
	}
	return nil
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
