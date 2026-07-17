package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"ai-proxy/internal/archive"
	"ai-proxy/internal/config"
	"ai-proxy/internal/metrics"
	"ai-proxy/internal/usage"

	"github.com/anthropics/anthropic-sdk-go"
	anthropicoption "github.com/anthropics/anthropic-sdk-go/option"
	"github.com/openai/openai-go"
	openaioption "github.com/openai/openai-go/option"
)

// SDK 验收：官方 OpenAI / Anthropic Go SDK 对真实 ai-proxy HTTP server 的主路径与 typed error 可解析性。
// 上游用 mock Transport，不产生外部费用。

func startProxySDKServer(t *testing.T, cfg config.Config, upstream http.RoundTripper) *httptest.Server {
	t.Helper()
	interactionDir := filepath.Join(t.TempDir(), "interactions")
	cfg.InteractionDir = interactionDir
	cfg.ListenAddr = "127.0.0.1:0"
	cfg.DebugLog = false
	rec, err := archive.NewRecorder(interactionDir)
	if err != nil {
		t.Fatal(err)
	}
	cfg = mustHandlerConfig(cfg)
	if cfg.ClientAPIKeys == nil {
		cfg.ClientAPIKeys = map[string]config.ClientAPIKey{}
	}
	// SDK 测试默认 key,与 WithAPIKey("inbound-not-used") / anthropic WithAPIKey("inbound") 对齐。
	cfg.ClientAPIKeys["sdk"] = config.ClientAPIKey{ID: "sdk", APIKey: "inbound-not-used", Enabled: true}
	cfg.ClientAPIKeys["anthropic-sdk"] = config.ClientAPIKey{ID: "anthropic-sdk", APIKey: "inbound", Enabled: true}
	h := NewHandler(cfg, usage.NewMemoryStore(), rec, metrics.NewRegistry())
	if upstream != nil {
		h.client.Transport = upstream
	}
	return httptest.NewServer(h)
}

func TestOpenAISDKModelsChatEmbeddingsAndTypedError(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "")
	upstreamHits := 0
	upstream := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		upstreamHits++
		switch {
		case strings.HasSuffix(r.URL.Path, "/chat/completions"):
			return jsonResponse(`{"id":"chatcmpl-1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"pong"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}}`), nil
		case strings.HasSuffix(r.URL.Path, "/embeddings"):
			return jsonResponse(`{"object":"list","data":[{"object":"embedding","index":0,"embedding":[0.1,0.2,0.3]}],"model":"text-embedding-3-large","usage":{"prompt_tokens":2,"total_tokens":2}}`), nil
		default:
			t.Fatalf("unexpected upstream path %s", r.URL.Path)
			return nil, nil
		}
	})
	srv := startProxySDKServer(t, config.Config{
		Providers: map[string]config.Provider{
			"openai": {
				Name: "openai", Protocol: "openai", BaseURL: "https://upstream.test/v1", APIKey: "upstream-key",
				Models: []string{"gpt-test", "text-embedding-3-large"},
				EndpointCapabilities: []string{
					config.EndpointCapabilityChatCompletions,
					config.EndpointCapabilityEmbeddings,
				},
			},
		},
		ModelCatalog: map[string]config.ModelInfo{
			"gpt-test": {
				ID: "gpt-test", ContextWindowTokens: 128000, MaxOutputTokens: 4096,
				Operations: []string{config.ModelOperationChatCompletions}, RouteOwner: "openai",
			},
			"text-embedding-3-large": {
				ID: "text-embedding-3-large", ContextWindowTokens: 8192, MaxOutputTokens: 8191,
				Operations: []string{config.ModelOperationEmbeddings}, RouteOwner: "openai",
			},
		},
	}, upstream)
	defer srv.Close()

	client := openai.NewClient(
		openaioption.WithBaseURL(srv.URL+"/v1/"),
		openaioption.WithAPIKey("inbound-not-used"),
	)
	ctx := context.Background()

	// models
	page, err := client.Models.List(ctx)
	if err != nil {
		t.Fatalf("Models.List: %v", err)
	}
	if len(page.Data) < 2 {
		t.Fatalf("models count = %d", len(page.Data))
	}

	// chat
	chat, err := client.Chat.Completions.New(ctx, openai.ChatCompletionNewParams{
		Model: "gpt-test",
		Messages: []openai.ChatCompletionMessageParamUnion{
			openai.UserMessage("Reply with exactly: pong"),
		},
		MaxTokens: openai.Int(16),
	})
	if err != nil {
		t.Fatalf("Chat.Completions.New: %v", err)
	}
	if len(chat.Choices) == 0 || chat.Choices[0].Message.Content == "" {
		t.Fatalf("empty chat response: %#v", chat)
	}

	// embeddings
	emb, err := client.Embeddings.New(ctx, openai.EmbeddingNewParams{
		Model: "text-embedding-3-large",
		Input: openai.EmbeddingNewParamsInputUnion{OfString: openai.String("hello")},
	})
	if err != nil {
		t.Fatalf("Embeddings.New: %v", err)
	}
	if len(emb.Data) == 0 || len(emb.Data[0].Embedding) == 0 {
		t.Fatalf("empty embedding: %#v", emb)
	}

	// typed local error: operation_unsupported — SDK must parse without panic
	beforeHits := upstreamHits
	_, err = client.Embeddings.New(ctx, openai.EmbeddingNewParams{
		Model: "gpt-test",
		Input: openai.EmbeddingNewParamsInputUnion{OfString: openai.String("nope")},
	})
	if err == nil {
		t.Fatal("expected operation_unsupported error from OpenAI SDK")
	}
	if upstreamHits != beforeHits {
		t.Fatalf("upstream should not be called for local typed error, hits delta=%d", upstreamHits-beforeHits)
	}
	// OpenAI SDK wraps API errors; ensure body code is discoverable.
	if !strings.Contains(err.Error(), "operation_unsupported") && !strings.Contains(err.Error(), "does not support") {
		t.Fatalf("SDK error should surface typed contract: %v", err)
	}
}

func TestAnthropicSDKMessagesNativeAndConversionAndTypedError(t *testing.T) {
	// 避免环境中的 ANTHROPIC_AUTH_TOKEN/API_KEY 与 WithAPIKey 叠加导致双 Header 冲突 401。
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
	t.Setenv("ANTHROPIC_API_KEY", "")
	// Case A: anthropic native RouteOwner
	nativeUpstream := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if !strings.HasSuffix(r.URL.Path, "/messages") {
			t.Fatalf("native upstream path = %s", r.URL.Path)
		}
		return jsonResponse(`{"id":"msg_1","type":"message","role":"assistant","model":"claude-test","content":[{"type":"text","text":"pong"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":1}}`), nil
	})
	nativeSrv := startProxySDKServer(t, config.Config{
		Providers: map[string]config.Provider{
			"anthropic": {
				Name: "anthropic", Protocol: "anthropic", BaseURL: "https://anthropic.upstream", APIKey: "anthropic-key",
				Models:               []string{"claude-test"},
				EndpointCapabilities: []string{config.EndpointCapabilityMessages},
			},
		},
		ModelCatalog: map[string]config.ModelInfo{
			"claude-test": {
				ID: "claude-test", ContextWindowTokens: 200000, MaxOutputTokens: 8192,
				Operations: []string{config.ModelOperationChatCompletions}, RouteOwner: "anthropic",
			},
		},
	}, nativeUpstream)
	defer nativeSrv.Close()

	nativeClient := anthropic.NewClient(
		anthropicoption.WithBaseURL(nativeSrv.URL+"/"),
		anthropicoption.WithAPIKey("inbound"),
	)
	msg, err := nativeClient.Messages.New(context.Background(), anthropic.MessageNewParams{
		Model:     "claude-test",
		MaxTokens: 32,
		Messages: []anthropic.MessageParam{
			anthropic.NewUserMessage(anthropic.NewTextBlock("Reply with exactly: pong")),
		},
	})
	if err != nil {
		t.Fatalf("native Messages.New: %v", err)
	}
	if len(msg.Content) == 0 {
		t.Fatalf("empty native message: %#v", msg)
	}

	// Case B: openai conversion RouteOwner (Anthropic client -> OpenAI upstream)
	convHits := 0
	convUpstream := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		convHits++
		if !strings.HasSuffix(r.URL.Path, "/chat/completions") {
			t.Fatalf("conversion upstream path = %s", r.URL.Path)
		}
		// ensure anthropic headers not leaked
		if r.Header.Get("Anthropic-Version") != "" || r.Header.Get("X-API-Key") != "" {
			t.Fatalf("leaked anthropic headers: %v", r.Header)
		}
		return jsonResponse(`{"id":"chatcmpl-2","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"pong"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}}`), nil
	})
	convSrv := startProxySDKServer(t, config.Config{
		Providers: map[string]config.Provider{
			"openai": {
				Name: "openai", Protocol: "openai", BaseURL: "https://openai.upstream/v1", APIKey: "openai-key",
				Models:               []string{"gpt-test"},
				EndpointCapabilities: []string{config.EndpointCapabilityChatCompletions},
			},
		},
		ModelCatalog: map[string]config.ModelInfo{
			"gpt-test": {
				ID: "gpt-test", ContextWindowTokens: 128000, MaxOutputTokens: 4096,
				Operations: []string{config.ModelOperationChatCompletions}, RouteOwner: "openai",
			},
		},
	}, convUpstream)
	defer convSrv.Close()

	convClient := anthropic.NewClient(
		anthropicoption.WithBaseURL(convSrv.URL+"/"),
		anthropicoption.WithAPIKey("inbound"),
	)
	msg2, err := convClient.Messages.New(context.Background(), anthropic.MessageNewParams{
		Model:     "gpt-test",
		MaxTokens: 32,
		Messages: []anthropic.MessageParam{
			anthropic.NewUserMessage(anthropic.NewTextBlock("Reply with exactly: pong")),
		},
	})
	if err != nil {
		t.Fatalf("conversion Messages.New: %v", err)
	}
	if convHits != 1 {
		t.Fatalf("conversion upstream hits=%d", convHits)
	}
	if len(msg2.Content) == 0 {
		t.Fatalf("empty conversion message: %#v", msg2)
	}

	// typed error via Anthropic SDK (tools -> conversion_unsupported)
	before := convHits
	_, err = convClient.Messages.New(context.Background(), anthropic.MessageNewParams{
		Model:     "gpt-test",
		MaxTokens: 16,
		Messages: []anthropic.MessageParam{
			anthropic.NewUserMessage(anthropic.NewTextBlock("hi")),
		},
		Tools: []anthropic.ToolUnionParam{{
			OfTool: &anthropic.ToolParam{
				Name: "x",
				InputSchema: anthropic.ToolInputSchemaParam{
					Properties: map[string]any{},
				},
			},
		}},
	})
	if err == nil {
		t.Fatal("expected conversion_unsupported via Anthropic SDK")
	}
	if convHits != before {
		t.Fatalf("tools preflight must not hit upstream, hits delta=%d", convHits-before)
	}
	if !strings.Contains(err.Error(), "conversion_unsupported") && !strings.Contains(err.Error(), "tools") {
		t.Fatalf("SDK error should surface conversion contract: %v", err)
	}

	// conversion-mode upstream error envelope must remain Anthropic-parseable
	errUpstream := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusTooManyRequests,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"rate limited","type":"rate_limit_error"}}`)),
		}, nil
	})
	errSrv := startProxySDKServer(t, config.Config{
		Providers: map[string]config.Provider{
			"openai": {
				Name: "openai", Protocol: "openai", BaseURL: "https://openai.upstream/v1", APIKey: "openai-key",
				Models:               []string{"gpt-test"},
				EndpointCapabilities: []string{config.EndpointCapabilityChatCompletions},
			},
		},
		ModelCatalog: map[string]config.ModelInfo{
			"gpt-test": {
				ID: "gpt-test", ContextWindowTokens: 128000, MaxOutputTokens: 4096,
				Operations: []string{config.ModelOperationChatCompletions}, RouteOwner: "openai",
			},
		},
	}, errUpstream)
	defer errSrv.Close()
	errClient := anthropic.NewClient(
		anthropicoption.WithBaseURL(errSrv.URL+"/"),
		anthropicoption.WithAPIKey("inbound"),
	)
	_, err = errClient.Messages.New(context.Background(), anthropic.MessageNewParams{
		Model:     "gpt-test",
		MaxTokens: 16,
		Messages: []anthropic.MessageParam{
			anthropic.NewUserMessage(anthropic.NewTextBlock("hi")),
		},
	})
	if err == nil {
		t.Fatal("expected upstream 429 surfaced through Anthropic SDK")
	}
	// Ensure response was Anthropic error shape (SDK parsed it as API error, not transport decode failure)
	raw, _ := json.Marshal(map[string]string{"err": err.Error()})
	if strings.Contains(string(raw), "json:") && strings.Contains(err.Error(), "cannot unmarshal") {
		t.Fatalf("Anthropic SDK failed to parse conversion upstream error envelope: %v", err)
	}
}
