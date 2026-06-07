package proxy

import (
	"encoding/json"
	"strings"
	"time"
)

type openAIStreamAccumulator struct {
	ID           string
	Object       string
	Model        string
	Created      int64
	Role         string
	Content      strings.Builder
	FinishReason string
	Usage        tokenUsage
}

func newOpenAIStreamAccumulator(fallbackModel string) *openAIStreamAccumulator {
	return &openAIStreamAccumulator{
		ID:      "chatcmpl-stream",
		Object:  "chat.completion",
		Model:   fallbackModel,
		Created: time.Now().Unix(),
		Role:    "assistant",
	}
}

func (a *openAIStreamAccumulator) TrackSSELine(line []byte) {
	trimmed := strings.TrimSpace(string(line))
	if !strings.HasPrefix(trimmed, "data:") {
		return
	}
	payload := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
	a.TrackDataPayload(payload)
}

func (a *openAIStreamAccumulator) TrackDataPayload(payload string) {
	if payload == "" || payload == "[DONE]" {
		return
	}
	var event map[string]any
	if err := json.Unmarshal([]byte(payload), &event); err != nil {
		return
	}
	if id, ok := event["id"].(string); ok && id != "" {
		a.ID = id
	}
	if object, ok := event["object"].(string); ok && object != "" {
		a.Object = object
	}
	if model, ok := event["model"].(string); ok && model != "" {
		a.Model = model
	}
	if created, ok := numberAsInt(event["created"]); ok && created > 0 {
		a.Created = int64(created)
	}
	if parsed, ok := usageFromMap(event["usage"]); ok {
		a.Usage = parsed
	}
	choices, _ := event["choices"].([]any)
	for _, item := range choices {
		choice, _ := item.(map[string]any)
		if reason, ok := choice["finish_reason"].(string); ok && reason != "" {
			a.FinishReason = reason
		}
		if delta, ok := choice["delta"].(map[string]any); ok {
			if role, ok := delta["role"].(string); ok && role != "" {
				a.Role = role
			}
			a.Content.WriteString(flattenValue(delta["content"]))
		}
		if message, ok := choice["message"].(map[string]any); ok {
			if role, ok := message["role"].(string); ok && role != "" {
				a.Role = role
			}
			a.Content.WriteString(flattenValue(message["content"]))
		}
		if text, ok := choice["text"].(string); ok {
			a.Content.WriteString(text)
		}
	}
}

func (a *openAIStreamAccumulator) FinalizeUsage(requestBody map[string]any) tokenUsage {
	usage := a.Usage
	if !usage.Known {
		usage = tokenUsage{
			PromptTokens:     estimatePromptTokens(requestBody),
			CompletionTokens: estimateTokens(a.Content.String()),
			Estimated:        true,
			Known:            true,
		}
		a.Usage = usage
	}
	return usage
}

func (a *openAIStreamAccumulator) ResponseJSON() ([]byte, error) {
	return streamCompletionJSON(a.ID, a.Model, a.Created, firstNonEmpty(a.Role, "assistant"), a.Content.String(), a.FinishReason, a.Usage)
}

func streamCompletionJSON(id, model string, created int64, role, content, finishReason string, usage tokenUsage) ([]byte, error) {
	if finishReason == "" {
		finishReason = "stop"
	}
	if created == 0 {
		created = time.Now().Unix()
	}
	response := map[string]any{
		"id":      firstNonEmpty(id, "chatcmpl-stream"),
		"object":  "chat.completion",
		"created": created,
		"model":   model,
		"choices": []any{
			map[string]any{
				"index": 0,
				"message": map[string]any{
					"role":    firstNonEmpty(role, "assistant"),
					"content": content,
				},
				"finish_reason": finishReason,
			},
		},
		"usage": openAIUsagePayload(usage),
	}
	return json.MarshalIndent(response, "", "  ")
}

func openAIUsagePayload(usage tokenUsage) map[string]any {
	payload := map[string]any{
		"prompt_tokens":     usage.PromptTokens,
		"completion_tokens": usage.CompletionTokens,
		"total_tokens":      usage.PromptTokens + usage.CompletionTokens,
	}
	if usage.CachedInputTokens > 0 {
		payload["prompt_tokens_details"] = map[string]any{
			"cached_tokens": usage.CachedInputTokens,
		}
	}
	if usage.CacheCreationInputTokens > 0 {
		payload["cache_creation_input_tokens"] = usage.CacheCreationInputTokens
	}
	return payload
}

type anthropicRawStreamAccumulator struct {
	ID                       string
	Model                    string
	Content                  strings.Builder
	StopReason               string
	InputTokens              int
	OutputTokens             int
	CachedInputTokens        int
	CacheCreationInputTokens int
}

func newAnthropicRawStreamAccumulator(fallbackModel string) *anthropicRawStreamAccumulator {
	return &anthropicRawStreamAccumulator{
		ID:    "msg_stream",
		Model: fallbackModel,
	}
}

func (a *anthropicRawStreamAccumulator) TrackSSELine(line []byte) {
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
	eventType, _ := event["type"].(string)
	switch eventType {
	case "message_start":
		if message, ok := event["message"].(map[string]any); ok {
			if id, ok := message["id"].(string); ok && id != "" {
				a.ID = id
			}
			if model, ok := message["model"].(string); ok && model != "" {
				a.Model = model
			}
			if usage, ok := anthropicUsage(message["usage"]); ok {
				a.InputTokens = usage.PromptTokens
				a.CachedInputTokens = usage.CachedInputTokens
				a.CacheCreationInputTokens = usage.CacheCreationInputTokens
			}
		}
	case "content_block_delta":
		if delta, ok := event["delta"].(map[string]any); ok {
			if text, ok := delta["text"].(string); ok {
				a.Content.WriteString(text)
			}
		}
	case "message_delta":
		if usage, ok := anthropicUsage(event["usage"]); ok {
			a.OutputTokens = usage.CompletionTokens
			if usage.CachedInputTokens > 0 {
				a.CachedInputTokens = usage.CachedInputTokens
			}
			if usage.CacheCreationInputTokens > 0 {
				a.CacheCreationInputTokens = usage.CacheCreationInputTokens
			}
		}
		if delta, ok := event["delta"].(map[string]any); ok {
			if reason, ok := delta["stop_reason"].(string); ok && reason != "" {
				a.StopReason = reason
			}
		}
	}
}

func (a *anthropicRawStreamAccumulator) FinalizeUsage(requestBody map[string]any) tokenUsage {
	usage := tokenUsage{
		PromptTokens:             a.InputTokens,
		CompletionTokens:         a.OutputTokens,
		CachedInputTokens:        a.CachedInputTokens,
		CacheCreationInputTokens: a.CacheCreationInputTokens,
		Known:                    a.InputTokens > 0 || a.OutputTokens > 0,
	}
	if usage.PromptTokens == 0 {
		usage.PromptTokens = estimatePromptTokens(requestBody)
		usage.Estimated = true
	}
	if usage.CompletionTokens == 0 {
		usage.CompletionTokens = estimateTokens(a.Content.String())
		usage.Estimated = true
	}
	usage.Known = true
	return usage
}

func (a *anthropicRawStreamAccumulator) ResponseJSON(usage tokenUsage) ([]byte, error) {
	response := map[string]any{
		"id":            a.ID,
		"type":          "message",
		"role":          "assistant",
		"model":         a.Model,
		"content":       []any{map[string]any{"type": "text", "text": a.Content.String()}},
		"stop_reason":   firstNonEmpty(a.StopReason, "end_turn"),
		"stop_sequence": nil,
		"usage":         anthropicUsagePayload(usage),
	}
	return json.MarshalIndent(response, "", "  ")
}

func anthropicUsagePayload(usage tokenUsage) map[string]any {
	payload := map[string]any{
		"input_tokens":  usage.PromptTokens,
		"output_tokens": usage.CompletionTokens,
	}
	if usage.CachedInputTokens > 0 {
		payload["cache_read_input_tokens"] = usage.CachedInputTokens
	}
	if usage.CacheCreationInputTokens > 0 {
		payload["cache_creation_input_tokens"] = usage.CacheCreationInputTokens
	}
	return payload
}
