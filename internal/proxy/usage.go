package proxy

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"ai-proxy/internal/config"
)

type tokenUsage struct {
	PromptTokens             int  `json:"prompt_tokens"`
	CompletionTokens         int  `json:"completion_tokens"`
	TotalTokens              int  `json:"total_tokens"`
	CachedInputTokens        int  `json:"-"`
	CacheCreationInputTokens int  `json:"-"`
	Estimated                bool `json:"-"`
	Known                    bool `json:"-"`
}

func (u tokenUsage) CacheHitRate() float64 {
	if u.PromptTokens <= 0 || u.CachedInputTokens <= 0 {
		return 0
	}
	return float64(u.CachedInputTokens) / float64(u.PromptTokens)
}

func usageFromRaw(raw json.RawMessage) (tokenUsage, bool) {
	if len(raw) == 0 || string(raw) == "null" {
		return tokenUsage{}, false
	}
	var usage tokenUsage
	if err := json.Unmarshal(raw, &usage); err != nil {
		return tokenUsage{}, false
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err == nil {
		applyUsageDetails(&usage, payload)
	}
	usage.Known = true
	return usage, true
}

func usageFromMap(value any) (tokenUsage, bool) {
	if value == nil {
		return tokenUsage{}, false
	}
	bytes, err := json.Marshal(value)
	if err != nil {
		return tokenUsage{}, false
	}
	return usageFromRaw(bytes)
}

func usageFromRawResponse(provider config.Provider, responseBody []byte, requestBody map[string]any) tokenUsage {
	if len(responseBody) == 0 {
		return tokenUsage{}
	}
	var payload map[string]any
	if err := json.Unmarshal(responseBody, &payload); err != nil {
		return tokenUsage{}
	}
	if provider.Protocol == "anthropic" {
		if usageValue, ok := payload["usage"].(map[string]any); ok {
			usage := tokenUsage{Known: true}
			usage.PromptTokens, _ = numberAsInt(usageValue["input_tokens"])
			usage.CompletionTokens, _ = numberAsInt(usageValue["output_tokens"])
			applyUsageDetails(&usage, usageValue)
			return usage
		}
	}
	if usage, ok := usageFromMap(payload["usage"]); ok {
		return usage
	}
	if provider.Protocol == "openai" {
		completionTokens := estimateCompletionTokensFromResponse(responseBody)
		if completionTokens > 0 || len(requestBody) > 0 {
			return tokenUsage{
				PromptTokens:     estimatePromptTokens(requestBody),
				CompletionTokens: completionTokens,
				Estimated:        true,
				Known:            true,
			}
		}
	}
	return tokenUsage{}
}

func applyUsageDetails(usage *tokenUsage, payload map[string]any) {
	if usage == nil || payload == nil {
		return
	}
	if usage.PromptTokens == 0 {
		usage.PromptTokens, _ = numberAsInt(payload["input_tokens"])
	}
	if usage.CompletionTokens == 0 {
		usage.CompletionTokens, _ = numberAsInt(payload["output_tokens"])
	}
	if value, ok := numberAsInt(payload["cache_read_input_tokens"]); ok {
		usage.CachedInputTokens = value
	}
	if value, ok := numberAsInt(payload["cache_creation_input_tokens"]); ok {
		usage.CacheCreationInputTokens = value
	}
	if details, ok := payload["prompt_tokens_details"].(map[string]any); ok {
		if value, ok := numberAsInt(details["cached_tokens"]); ok {
			usage.CachedInputTokens = value
		}
	}
	if details, ok := payload["input_tokens_details"].(map[string]any); ok {
		if value, ok := numberAsInt(details["cached_tokens"]); ok {
			usage.CachedInputTokens = value
		}
		if value, ok := numberAsInt(details["cache_read_tokens"]); ok {
			usage.CachedInputTokens = value
		}
		if value, ok := numberAsInt(details["cache_creation_tokens"]); ok {
			usage.CacheCreationInputTokens = value
		}
	}
}

func estimatePromptTokens(body map[string]any) int {
	var builder strings.Builder
	if messages, ok := body["messages"].([]any); ok {
		for _, message := range messages {
			builder.WriteString(flattenValue(message))
			builder.WriteByte('\n')
		}
	}
	if builder.Len() == 0 {
		builder.WriteString(flattenValue(body))
	}
	return estimateTokens(builder.String())
}

func estimateCompletionTokensFromResponse(body []byte) int {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return estimateTokens(string(body))
	}
	var builder strings.Builder
	choices, _ := payload["choices"].([]any)
	for _, item := range choices {
		choice, _ := item.(map[string]any)
		if message, ok := choice["message"].(map[string]any); ok {
			builder.WriteString(flattenValue(message["content"]))
		}
		if text, ok := choice["text"].(string); ok {
			builder.WriteString(text)
		}
	}
	if builder.Len() == 0 {
		return 0
	}
	return estimateTokens(builder.String())
}

func estimateTokens(text string) int {
	runes := utf8.RuneCountInString(text)
	if runes == 0 {
		return 0
	}
	tokens := (runes + 3) / 4
	if tokens < 1 {
		return 1
	}
	return tokens
}

func flattenValue(value any) string {
	switch v := value.(type) {
	case nil:
		return ""
	case string:
		return v
	case []any:
		parts := make([]string, 0, len(v))
		for _, item := range v {
			parts = append(parts, flattenValue(item))
		}
		return strings.Join(parts, " ")
	case map[string]any:
		parts := make([]string, 0, len(v))
		for key, item := range v {
			if key == "role" {
				continue
			}
			parts = append(parts, flattenValue(item))
		}
		return strings.Join(parts, " ")
	default:
		return fmt.Sprint(v)
	}
}
