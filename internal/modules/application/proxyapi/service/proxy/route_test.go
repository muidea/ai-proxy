package proxy

import (
	"net/http"
	"strings"
	"testing"

	"ai-proxy/internal/pkg/aiproxyconfig"
)

func testRouteConfig() config.Config {
	return config.Config{
		Providers: map[string]config.Provider{
			"openai-full": {
				Name:     "openai-full",
				Protocol: "openai",
				BaseURL:  "https://openai.test/v1",
				APIKey:   "k",
				Models:   []string{"gpt-*", "text-embedding-*"},
				EndpointCapabilities: []string{
					config.EndpointCapabilityChatCompletions,
					config.EndpointCapabilityResponses,
					config.EndpointCapabilityCompletions,
					config.EndpointCapabilityEmbeddings,
				},
			},
			"openai-chat-only": {
				Name:                 "openai-chat-only",
				Protocol:             "openai",
				BaseURL:              "https://openai-chat.test/v1",
				APIKey:               "k",
				Models:               []string{"chat-only-*"},
				EndpointCapabilities: []string{config.EndpointCapabilityChatCompletions},
			},
			"anthropic": {
				Name:                 "anthropic",
				Protocol:             "anthropic",
				BaseURL:              "https://anthropic.test",
				APIKey:               "k",
				Models:               []string{"claude-*"},
				EndpointCapabilities: []string{config.EndpointCapabilityMessages},
			},
		},
		ModelCatalog: map[string]config.ModelInfo{
			"gpt-test": {
				ID: "gpt-test", ContextWindowTokens: 128000, MaxOutputTokens: 4096,
				Operations: []string{config.ModelOperationChatCompletions}, RouteOwner: "openai-full",
			},
			"text-embedding-test": {
				ID: "text-embedding-test", ContextWindowTokens: 8192, MaxOutputTokens: 8191,
				Operations: []string{config.ModelOperationEmbeddings}, RouteOwner: "openai-full",
			},
			"chat-only-model": {
				ID: "chat-only-model", ContextWindowTokens: 32000, MaxOutputTokens: 4096,
				Operations: []string{config.ModelOperationChatCompletions}, RouteOwner: "openai-chat-only",
			},
			"claude-test": {
				ID: "claude-test", ContextWindowTokens: 200000, MaxOutputTokens: 8192,
				Operations: []string{config.ModelOperationChatCompletions}, RouteOwner: "anthropic",
			},
		},
	}
}

func TestResolveTransportPlanMatrix(t *testing.T) {
	cfg := testRouteConfig()
	cases := []struct {
		name             string
		path             string
		model            string
		wantMode         string
		wantUpstreamPath string
		wantOwner        string
		wantCode         string
	}{
		{
			name: "openai chat native",
			path: "/v1/chat/completions", model: "gpt-test",
			wantMode: TransportModeNative, wantUpstreamPath: "/v1/chat/completions", wantOwner: "openai-full",
		},
		{
			name: "openai chat to anthropic conversion",
			path: "/v1/chat/completions", model: "claude-test",
			wantMode: TransportModeOpenAIToAnthropic, wantUpstreamPath: "/v1/messages", wantOwner: "anthropic",
		},
		{
			name: "anthropic messages native",
			path: "/v1/messages", model: "claude-test",
			wantMode: TransportModeNative, wantUpstreamPath: "/v1/messages", wantOwner: "anthropic",
		},
		{
			name: "anthropic messages to openai conversion",
			path: "/v1/messages", model: "gpt-test",
			wantMode: TransportModeAnthropicToOpenAI, wantUpstreamPath: "/v1/chat/completions", wantOwner: "openai-full",
		},
		{
			name: "responses native",
			path: "/v1/responses", model: "gpt-test",
			wantMode: TransportModeNative, wantUpstreamPath: "/v1/responses", wantOwner: "openai-full",
		},
		{
			name: "completions native",
			path: "/v1/completions", model: "gpt-test",
			wantMode: TransportModeNative, wantUpstreamPath: "/v1/completions", wantOwner: "openai-full",
		},
		{
			name: "embeddings native",
			path: "/v1/embeddings", model: "text-embedding-test",
			wantMode: TransportModeNative, wantUpstreamPath: "/v1/embeddings", wantOwner: "openai-full",
		},
		{
			name: "responses not available on chat-only endpoint capability",
			path: "/v1/responses", model: "chat-only-model",
			// model 有 chat_completions,但 RouteOwner 无 responses direct capability
			wantCode: ErrorCodeEndpointUnsupported,
		},
		{
			name: "embeddings not available on chat-only model operations",
			path: "/v1/embeddings", model: "chat-only-model",
			// operation 校验先于 endpoint: catalog 未声明 embeddings → operation_unsupported
			wantCode: ErrorCodeOperationUnsupported,
		},
		{
			name: "embeddings not available via anthropic conversion",
			path: "/v1/embeddings", model: "claude-test",
			wantCode: ErrorCodeOperationUnsupported,
		},
		{
			name: "responses not available via anthropic",
			path: "/v1/responses", model: "claude-test",
			wantCode: ErrorCodeEndpointUnsupported,
		},
		{
			name: "model required",
			path: "/v1/chat/completions", model: "",
			wantCode: ErrorCodeModelRequired,
		},
		{
			name: "model not found",
			path: "/v1/chat/completions", model: "missing-model",
			wantCode: ErrorCodeModelNotFound,
		},
		{
			name: "operation unsupported for embeddings-only model on chat",
			path: "/v1/chat/completions", model: "text-embedding-test",
			wantCode: ErrorCodeOperationUnsupported,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plan, apiErr := ResolveTransportPlan(cfg, http.MethodPost, tc.path, tc.model)
			if tc.wantCode != "" {
				if apiErr == nil {
					t.Fatalf("expected error %s, got plan %#v", tc.wantCode, plan)
				}
				if apiErr.Code != tc.wantCode {
					t.Fatalf("code = %q want %q msg=%s", apiErr.Code, tc.wantCode, apiErr.Message)
				}
				return
			}
			if apiErr != nil {
				t.Fatalf("unexpected error: %#v", apiErr)
			}
			if plan.Mode != tc.wantMode {
				t.Fatalf("mode = %q want %q", plan.Mode, tc.wantMode)
			}
			if plan.UpstreamEndpoint != tc.wantUpstreamPath {
				t.Fatalf("upstream = %q want %q", plan.UpstreamEndpoint, tc.wantUpstreamPath)
			}
			if plan.RouteOwner != tc.wantOwner {
				t.Fatalf("owner = %q want %q", plan.RouteOwner, tc.wantOwner)
			}
			if plan.ClientEndpoint != strings.TrimRight(tc.path, "/") {
				t.Fatalf("client endpoint = %q", plan.ClientEndpoint)
			}
			if plan.ModelID != tc.model {
				t.Fatalf("model = %q", plan.ModelID)
			}
		})
	}
}

func TestValidateConversionRequestRejectsFeatures(t *testing.T) {
	plan := TransportPlan{
		ModelID:          "claude-test",
		Operation:        config.ModelOperationChatCompletions,
		ClientProtocol:   ClientProtocolOpenAI,
		ClientEndpoint:   "/v1/chat/completions",
		RouteOwner:       "anthropic",
		UpstreamProtocol: "anthropic",
		UpstreamEndpoint: "/v1/messages",
		Mode:             TransportModeOpenAIToAnthropic,
	}

	cases := []struct {
		name    string
		body    map[string]any
		feature string
	}{
		{
			name: "tools",
			body: map[string]any{
				"model": "claude-test",
				"messages": []any{
					map[string]any{"role": "user", "content": "hi"},
				},
				"tools": []any{map[string]any{"type": "function"}},
			},
			feature: "tools",
		},
		{
			name: "response_format",
			body: map[string]any{
				"model":           "claude-test",
				"messages":        []any{map[string]any{"role": "user", "content": "hi"}},
				"response_format": map[string]any{"type": "json_object"},
			},
			feature: "response_format",
		},
		{
			name: "image content",
			body: map[string]any{
				"model": "claude-test",
				"messages": []any{
					map[string]any{
						"role": "user",
						"content": []any{
							map[string]any{"type": "image_url", "image_url": map[string]any{"url": "https://x"}},
						},
					},
				},
			},
			feature: "image_url",
		},
		{
			name: "tool role",
			body: map[string]any{
				"model": "claude-test",
				"messages": []any{
					map[string]any{"role": "tool", "content": "result"},
				},
			},
			feature: "tool role",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			apiErr := ValidateConversionRequest(plan, tc.body)
			if apiErr == nil {
				t.Fatal("expected conversion_unsupported")
			}
			if apiErr.Code != ErrorCodeConversionUnsupported {
				t.Fatalf("code = %q", apiErr.Code)
			}
			if apiErr.Feature == "" && !strings.Contains(apiErr.Message, tc.feature) {
				t.Fatalf("feature/message missing %q: feature=%q msg=%s", tc.feature, apiErr.Feature, apiErr.Message)
			}
			if apiErr.ClientEndpoint != plan.ClientEndpoint {
				t.Fatalf("client_endpoint = %q", apiErr.ClientEndpoint)
			}
		})
	}

	// native plan 不做 conversion preflight
	native := plan
	native.Mode = TransportModeNative
	if err := ValidateConversionRequest(native, map[string]any{"tools": []any{}}); err != nil {
		t.Fatalf("native plan should not reject tools in ValidateConversionRequest: %#v", err)
	}
}

func TestClientProtocolForPath(t *testing.T) {
	if got := ClientProtocolForPath("/v1/messages"); got != ClientProtocolAnthropic {
		t.Fatalf("messages protocol = %q", got)
	}
	if got := ClientProtocolForPath("/v1/chat/completions"); got != ClientProtocolOpenAI {
		t.Fatalf("chat protocol = %q", got)
	}
	if got := ClientProtocolForPath("/v1/embeddings"); got != ClientProtocolOpenAI {
		t.Fatalf("embeddings protocol = %q", got)
	}
}

func TestTransportPlanForCanonical(t *testing.T) {
	provider := config.Provider{
		Name:                 "anthropic",
		Protocol:             "anthropic",
		EndpointCapabilities: []string{config.EndpointCapabilityMessages},
	}
	plan, ok := TransportPlanForCanonical("anthropic", provider, "claude-x", config.ModelOperationChatCompletions)
	if !ok {
		t.Fatal("expected canonical plan")
	}
	if plan.Mode != TransportModeOpenAIToAnthropic || plan.UpstreamEndpoint != "/v1/messages" {
		t.Fatalf("plan = %#v", plan)
	}

	openai := config.Provider{
		Name:                 "openai",
		Protocol:             "openai",
		EndpointCapabilities: []string{config.EndpointCapabilityChatCompletions},
	}
	plan, ok = TransportPlanForCanonical("openai", openai, "gpt-x", config.ModelOperationEmbeddings)
	if ok {
		t.Fatalf("embeddings should not be ready without embeddings capability: %#v", plan)
	}
}

func TestConvertUpstreamErrorForClient(t *testing.T) {
	plan := TransportPlan{
		ModelID:          "gpt-test",
		ClientProtocol:   ClientProtocolAnthropic,
		ClientEndpoint:   "/v1/messages",
		UpstreamProtocol: "openai",
		Mode:             TransportModeAnthropicToOpenAI,
	}
	body, ct := convertUpstreamErrorForClient(plan, 429, []byte(`{"error":{"message":"rate limited","type":"rate_limit"}}`), "openai-full")
	if ct != "application/json" {
		t.Fatalf("content-type = %q", ct)
	}
	if !strings.Contains(string(body), `"type":"error"`) {
		t.Fatalf("anthropic envelope missing: %s", body)
	}
	if !strings.Contains(string(body), "rate limited") {
		t.Fatalf("message missing: %s", body)
	}
	if strings.Contains(string(body), "sk-") {
		t.Fatal("must not leak secrets")
	}

	plan.ClientProtocol = ClientProtocolOpenAI
	body, _ = convertUpstreamErrorForClient(plan, 500, []byte(`{"type":"error","error":{"message":"boom","type":"api_error"}}`), "anthropic")
	if !strings.Contains(string(body), `"type":"upstream_error"`) {
		t.Fatalf("openai envelope missing: %s", body)
	}
}
