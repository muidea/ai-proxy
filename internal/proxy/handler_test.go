package proxy

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"io"
	"log"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ai-proxy/internal/archive"
	"ai-proxy/internal/config"
	"ai-proxy/internal/metrics"
	"ai-proxy/internal/stats"
)

// combineWriter 把所有写入委托到内部的 strings.Builder,
type combineWriter struct{ buf *strings.Builder }

func (c *combineWriter) Write(p []byte) (int, error) { return c.buf.Write(p) }

func TestOpenAICompatibleBufferedUsage(t *testing.T) {
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Fatalf("unexpected authorization: %s", got)
		}
		return jsonResponse(`{"id":"chatcmpl-1","choices":[{"message":{"role":"assistant","content":"hello"}}],"usage":{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10,"prompt_tokens_details":{"cached_tokens":4}}}`), nil
	})

	usageFile := filepath.Join(t.TempDir(), "usage.csv")
	handler := testHandler("https://upstream.test", usageFile, "openai")
	handler.client.Transport = transport
	request := newRequest(http.MethodPost, "/v1/chat/completions", `{"model":"gpt-test","messages":[{"role":"user","content":"hi"}]}`)
	response := newResponseRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	records := readUsageCSV(t, usageFile)
	if got := records[1][3]; got != "7" {
		t.Fatalf("input tokens = %s", got)
	}
	if got := records[1][4]; got != "3" {
		t.Fatalf("output tokens = %s", got)
	}
	if got := records[1][10]; got != "4" {
		t.Fatalf("cached input tokens = %s", got)
	}
	if got := records[1][12]; got != "0.5714" {
		t.Fatalf("cache hit rate = %s", got)
	}
	interactionDir := filepath.Join(filepath.Dir(usageFile), "interactions", "000001")
	assertFileContains(t, filepath.Join(interactionDir, "request.json"), `"model": "gpt-test"`)
	assertFileContains(t, filepath.Join(interactionDir, "request.meta.json"), `"path": "/v1/chat/completions"`)
	assertFileContains(t, filepath.Join(interactionDir, "upstream_request.json"), `"url": "https://upstream.test/v1/chat/completions"`)
	assertFileContains(t, filepath.Join(interactionDir, "upstream_response.json"), `"status": 200`)
	assertFileContains(t, filepath.Join(interactionDir, "response.json"), `"usage"`)
	assertFileContains(t, filepath.Join(interactionDir, "metadata.json"), `"response_path": "response.json"`)
	assertFileContains(t, filepath.Join(interactionDir, "metadata.json"), `"cached_input_tokens": 4`)
	assertFileContains(t, filepath.Join(interactionDir, "metadata.json"), `"cache_hit_rate": 0.5714285714285714`)
}

func TestOpenAICompatibleStreamingUsage(t *testing.T) {
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		body := strings.Join([]string{
			"data: {\"choices\":[{\"delta\":{\"content\":\"he\"}}]}",
			"",
			"data: {\"choices\":[{\"delta\":{\"content\":\"llo\"}}]}",
			"",
			"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":2,\"total_tokens\":7,\"prompt_tokens_details\":{\"cached_tokens\":2}}}",
			"",
			"data: [DONE]",
			"",
		}, "\n")
		return sseResponse(body), nil
	})

	usageFile := filepath.Join(t.TempDir(), "usage.csv")
	handler := testHandler("https://upstream.test", usageFile, "openai")
	handler.client.Transport = transport
	request := newRequest(http.MethodPost, "/v1/chat/completions", `{"model":"gpt-test","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	response := newResponseRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d", response.Code)
	}
	if !strings.Contains(response.Body.String(), "data: [DONE]") {
		t.Fatalf("stream response was not forwarded: %s", response.Body.String())
	}
	records := readUsageCSV(t, usageFile)
	if got := records[1][3]; got != "5" {
		t.Fatalf("input tokens = %s", got)
	}
	if got := records[1][4]; got != "2" {
		t.Fatalf("output tokens = %s", got)
	}
	if got := records[1][10]; got != "2" {
		t.Fatalf("cached input tokens = %s", got)
	}
	if got := records[1][12]; got != "0.4000" {
		t.Fatalf("cache hit rate = %s", got)
	}
	interactionDir := filepath.Join(filepath.Dir(usageFile), "interactions", "000001")
	assertFileContains(t, filepath.Join(interactionDir, "request.json"), `"stream": true`)
	assertFileContains(t, filepath.Join(interactionDir, "response.sse"), "data: [DONE]")
	assertFileContains(t, filepath.Join(interactionDir, "response.json"), `"content": "hello"`)
	assertFileContains(t, filepath.Join(interactionDir, "response.json"), `"total_tokens": 7`)
	assertFileContains(t, filepath.Join(interactionDir, "response.json"), `"cached_tokens": 2`)
	assertFileContains(t, filepath.Join(interactionDir, "metadata.json"), `"response_path": "response.sse"`)
	assertFileContains(t, filepath.Join(interactionDir, "metadata.json"), `"full_response_path": "response.json"`)
}

func TestOpenAIStreamingReadErrorLogsRoundAndMetadata(t *testing.T) {
	var logBuffer strings.Builder
	previousWriter := log.Writer()
	previousFlags := log.Flags()
	previousSlog := slog.Default()
	log.SetOutput(&logBuffer)
	log.SetFlags(0)
	slog.SetDefault(slog.New(slog.NewTextHandler(&combineWriter{&logBuffer}, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer func() {
		log.SetOutput(previousWriter)
		log.SetFlags(previousFlags)
		slog.SetDefault(previousSlog)
	}()

	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body: &failingReadCloser{
				chunks: [][]byte{[]byte("data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n")},
				err:    errors.New("context deadline exceeded"),
			},
		}, nil
	})

	usageFile := filepath.Join(t.TempDir(), "usage.csv")
	handler := testHandler("https://upstream.test", usageFile, "openai")
	handler.client.Transport = transport
	request := newRequest(http.MethodPost, "/v1/chat/completions", `{"model":"gpt-test","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	response := newResponseRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d", response.Code)
	}
	logs := logBuffer.String()
	if !strings.Contains(logs, "level=WARN") || !strings.Contains(logs, "STREAM") {
		t.Fatalf("expected stream warning with WARN level and STREAM marker, got: %s", logs)
	}
	if !strings.Contains(logs, "read upstream stream: context deadline exceeded") {
		t.Fatalf("expected upstream read error, got: %s", logs)
	}
	if !strings.Contains(logs, "level=WARN") || !strings.Contains(logs, "round=1") {
		t.Fatalf("expected final warning with round=1, got: %s", logs)
	}
	interactionDir := filepath.Join(filepath.Dir(usageFile), "interactions", "000001")
	assertFileContains(t, filepath.Join(interactionDir, "metadata.json"), `"error": "read upstream stream: context deadline exceeded"`)
}

func TestOpenAIStreamingClientCancelIsIdentified(t *testing.T) {
	var logBuffer strings.Builder
	previousWriter := log.Writer()
	previousFlags := log.Flags()
	previousSlog := slog.Default()
	log.SetOutput(&logBuffer)
	log.SetFlags(0)
	slog.SetDefault(slog.New(slog.NewTextHandler(&combineWriter{&logBuffer}, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer func() {
		log.SetOutput(previousWriter)
		log.SetFlags(previousFlags)
		slog.SetDefault(previousSlog)
	}()

	var cancel context.CancelFunc
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body: &failingReadCloser{
				chunks:    [][]byte{[]byte("data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n")},
				beforeErr: cancel,
				err:       context.Canceled,
			},
		}, nil
	})

	usageFile := filepath.Join(t.TempDir(), "usage.csv")
	handler := testHandler("https://upstream.test", usageFile, "openai")
	handler.client.Transport = transport
	request := newRequest(http.MethodPost, "/v1/chat/completions", `{"model":"gpt-test","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	ctx, cancelFunc := context.WithCancel(request.Context())
	cancel = cancelFunc
	request = request.WithContext(ctx)
	response := newResponseRecorder()

	handler.ServeHTTP(response, request)

	logs := logBuffer.String()
	if !strings.Contains(logs, "client canceled downstream request") {
		t.Fatalf("expected client cancel log, got: %s", logs)
	}
	interactionDir := filepath.Join(filepath.Dir(usageFile), "interactions", "000001")
	assertFileContains(t, filepath.Join(interactionDir, "metadata.json"), `"error": "read upstream stream: client canceled downstream request"`)
}

func TestAnthropicBufferedConversion(t *testing.T) {
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/v1/messages" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		if got := r.Header.Get("X-API-Key"); got != "anthropic-key" {
			t.Fatalf("unexpected api key: %s", got)
		}
		return jsonResponse(`{"id":"msg_1","model":"claude-test","content":[{"type":"text","text":"hello"}],"stop_reason":"end_turn","usage":{"input_tokens":11,"output_tokens":4,"cache_read_input_tokens":5,"cache_creation_input_tokens":2}}`), nil
	})

	usageFile := filepath.Join(t.TempDir(), "usage.csv")
	tempDir := t.TempDir()
	interactionRecorder, err := archive.NewRecorder(filepath.Join(tempDir, "interactions"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{
		ListenAddr:     ":0",
		UsageFile:      usageFile,
		InteractionDir: filepath.Join(tempDir, "interactions"),
		Providers: map[string]config.Provider{
			"anthropic": {Name: "anthropic", Protocol: "anthropic", BaseURL: "https://upstream.test", APIKey: "anthropic-key"},
		},
	}
	handler := NewHandler(cfg, stats.NewCSVRecorder(usageFile), interactionRecorder, metrics.NewRegistry())
	handler.client.Transport = transport
	request := newRequest(http.MethodPost, "/v1/chat/completions", `{"model":"claude-test","messages":[{"role":"system","content":"brief"},{"role":"user","content":"hi"}]}`)
	response := newResponseRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	var payload map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if got := payload["object"]; got != "chat.completion" {
		t.Fatalf("object = %v", got)
	}
	records := readUsageCSV(t, usageFile)
	if got := records[1][3]; got != "11" {
		t.Fatalf("input tokens = %s", got)
	}
	if got := records[1][4]; got != "4" {
		t.Fatalf("output tokens = %s", got)
	}
	if got := records[1][10]; got != "5" {
		t.Fatalf("cached input tokens = %s", got)
	}
	if got := records[1][11]; got != "2" {
		t.Fatalf("cache creation input tokens = %s", got)
	}
	if got := records[1][12]; got != "0.4545" {
		t.Fatalf("cache hit rate = %s", got)
	}
}

func TestRawProxyAvoidsDuplicateV1AndArchives(t *testing.T) {
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/v1/messages" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		if got := r.URL.RawQuery; got != "beta=true" {
			t.Fatalf("unexpected query: %s", got)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Fatalf("unexpected authorization: %s", got)
		}
		return testResponse(http.StatusNotFound, "application/json", `{"error":"not found"}`), nil
	})

	usageFile := filepath.Join(t.TempDir(), "usage.csv")
	handler := testHandler("https://upstream.test/v1", usageFile, "openai")
	handler.client.Transport = transport
	request := newRequest(http.MethodPost, "/v1/messages?beta=true", `{"model":"claude-test","messages":[{"role":"user","content":"hi"}]}`)
	request.Header.Set("Anthropic-Version", "2023-06-01")
	request.Header.Set("Authorization", "Bearer client-key")
	response := newResponseRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusNotFound {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	interactionDir := filepath.Join(filepath.Dir(usageFile), "interactions", "000001")
	assertFileContains(t, filepath.Join(interactionDir, "request.json"), `"model": "claude-test"`)
	assertFileContains(t, filepath.Join(interactionDir, "request.meta.json"), `"Authorization":`)
	assertFileContains(t, filepath.Join(interactionDir, "request.meta.json"), `<redacted>`)
	assertFileContains(t, filepath.Join(interactionDir, "upstream_request.json"), `"url": "https://upstream.test/v1/messages?beta=true"`)
	assertFileContains(t, filepath.Join(interactionDir, "upstream_response.json"), `"status": 404`)
	assertFileContains(t, filepath.Join(interactionDir, "response.json"), `"error"`)
	records := readUsageCSV(t, usageFile)
	if got := records[1][9]; got != "404" {
		t.Fatalf("http status = %s", got)
	}
}

func TestAnthropicRawRequestInfersAnthropicProvider(t *testing.T) {
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() != "https://anthropic.test/v1/messages?beta=true" {
			t.Fatalf("unexpected url: %s", r.URL.String())
		}
		if got := r.Header.Get("X-API-Key"); got != "anthropic-key" {
			t.Fatalf("unexpected x-api-key: %s", got)
		}
		if got := r.Header.Get("Authorization"); got != "" {
			t.Fatalf("authorization should not be forwarded to anthropic provider: %s", got)
		}
		return jsonResponse(`{"ok":true}`), nil
	})

	usageFile := filepath.Join(t.TempDir(), "usage.csv")
	interactionRecorder, err := archive.NewRecorder(filepath.Join(filepath.Dir(usageFile), "interactions"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{
		ListenAddr:     ":0",
		UsageFile:      usageFile,
		InteractionDir: filepath.Join(filepath.Dir(usageFile), "interactions"),
		Providers: map[string]config.Provider{
			"openai":    {Name: "openai", Protocol: "openai", BaseURL: "https://openai.test", APIKey: "openai-key"},
			"anthropic": {Name: "anthropic", Protocol: "anthropic", BaseURL: "https://anthropic.test", APIKey: "anthropic-key"},
		},
	}
	handler := NewHandler(cfg, stats.NewCSVRecorder(usageFile), interactionRecorder, metrics.NewRegistry())
	handler.client.Transport = transport
	request := newRequest(http.MethodPost, "/v1/messages?beta=true", `{"model":"claude-test","messages":[{"role":"user","content":"hi"}]}`)
	request.Header.Set("Anthropic-Version", "2023-06-01")
	request.Header.Set("Authorization", "Bearer client-key")
	response := newResponseRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	interactionDir := filepath.Join(filepath.Dir(usageFile), "interactions", "000001")
	assertFileContains(t, filepath.Join(interactionDir, "upstream_request.json"), `"provider": "anthropic"`)
	assertFileContains(t, filepath.Join(interactionDir, "upstream_request.json"), `"url": "https://anthropic.test/v1/messages?beta=true"`)
	records := readUsageCSV(t, usageFile)
	if got := records[1][1]; got != "anthropic" {
		t.Fatalf("provider = %s", got)
	}
}

func TestAmbiguousOpenAIProvidersRequireExplicitProvider(t *testing.T) {
	usageFile := filepath.Join(t.TempDir(), "usage.csv")
	interactionRecorder, err := archive.NewRecorder(filepath.Join(filepath.Dir(usageFile), "interactions"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{
		ListenAddr:     ":0",
		UsageFile:      usageFile,
		InteractionDir: filepath.Join(filepath.Dir(usageFile), "interactions"),
		Providers: map[string]config.Provider{
			"openai":   {Name: "openai", Protocol: "openai", BaseURL: "https://openai.test", APIKey: "openai-key"},
			"deepseek": {Name: "deepseek", Protocol: "openai", BaseURL: "https://deepseek.test", APIKey: "deepseek-key"},
		},
	}
	handler := NewHandler(cfg, stats.NewCSVRecorder(usageFile), interactionRecorder, metrics.NewRegistry())
	handler.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		t.Fatalf("request should not reach upstream")
		return nil, nil
	})
	request := newRequest(http.MethodPost, "/v1/chat/completions", `{"model":"unknown-model","messages":[{"role":"user","content":"hi"}]}`)
	response := newResponseRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "multiple providers configured") {
		t.Fatalf("unexpected body: %s", response.Body.String())
	}
	records := readUsageCSV(t, usageFile)
	if got := records[1][9]; got != "400" {
		t.Fatalf("http status = %s", got)
	}
}

func TestDefaultProviderHandlesUnknownOpenAIModel(t *testing.T) {
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "openai.test" {
			t.Fatalf("unexpected host: %s", r.URL.Host)
		}
		return jsonResponse(`{"choices":[{"message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`), nil
	})

	usageFile := filepath.Join(t.TempDir(), "usage.csv")
	interactionRecorder, err := archive.NewRecorder(filepath.Join(filepath.Dir(usageFile), "interactions"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{
		ListenAddr:      ":0",
		UsageFile:       usageFile,
		InteractionDir:  filepath.Join(filepath.Dir(usageFile), "interactions"),
		DefaultProvider: "openai",
		Providers: map[string]config.Provider{
			"openai":   {Name: "openai", Protocol: "openai", BaseURL: "https://openai.test", APIKey: "openai-key"},
			"deepseek": {Name: "deepseek", Protocol: "openai", BaseURL: "https://deepseek.test", APIKey: "deepseek-key"},
		},
	}
	handler := NewHandler(cfg, stats.NewCSVRecorder(usageFile), interactionRecorder, metrics.NewRegistry())
	handler.client.Transport = transport
	request := newRequest(http.MethodPost, "/v1/chat/completions", `{"model":"healthcheck","messages":[{"role":"user","content":"hi"}]}`)
	response := newResponseRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	interactionDir := filepath.Join(filepath.Dir(usageFile), "interactions", "000001")
	assertFileContains(t, filepath.Join(interactionDir, "upstream_request.json"), `"provider": "openai"`)
	records := readUsageCSV(t, usageFile)
	if got := records[1][1]; got != "openai" {
		t.Fatalf("provider = %s", got)
	}
}

func TestDefaultProviderHandlesOpenAIModelsEndpoint(t *testing.T) {
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() != "https://openai.test/v1/models" {
			t.Fatalf("unexpected url: %s", r.URL.String())
		}
		if got := r.Header.Get("Authorization"); got != "Bearer openai-key" {
			t.Fatalf("unexpected authorization: %s", got)
		}
		return jsonResponse(`{"object":"list","data":[]}`), nil
	})

	usageFile := filepath.Join(t.TempDir(), "usage.csv")
	interactionRecorder, err := archive.NewRecorder(filepath.Join(filepath.Dir(usageFile), "interactions"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{
		ListenAddr:      ":0",
		UsageFile:       usageFile,
		InteractionDir:  filepath.Join(filepath.Dir(usageFile), "interactions"),
		DefaultProvider: "openai",
		Providers: map[string]config.Provider{
			"openai":   {Name: "openai", Protocol: "openai", BaseURL: "https://openai.test", APIKey: "openai-key"},
			"deepseek": {Name: "deepseek", Protocol: "openai", BaseURL: "https://deepseek.test", APIKey: "deepseek-key"},
		},
	}
	handler := NewHandler(cfg, stats.NewCSVRecorder(usageFile), interactionRecorder, metrics.NewRegistry())
	handler.client.Transport = transport
	request := newRequest(http.MethodGet, "/v1/models", "")
	response := newResponseRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	interactionDir := filepath.Join(filepath.Dir(usageFile), "interactions", "000001")
	assertFileContains(t, filepath.Join(interactionDir, "upstream_request.json"), `"provider": "openai"`)
	assertFileContains(t, filepath.Join(interactionDir, "response.json"), `"object":"list"`)
}

func TestOpenAICompatibleProviderSelectionByBuiltInModelFamily(t *testing.T) {
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "deepseek.test" {
			t.Fatalf("unexpected host: %s", r.URL.Host)
		}
		return jsonResponse(`{"choices":[{"message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`), nil
	})

	usageFile := filepath.Join(t.TempDir(), "usage.csv")
	interactionRecorder, err := archive.NewRecorder(filepath.Join(filepath.Dir(usageFile), "interactions"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{
		ListenAddr:     ":0",
		UsageFile:      usageFile,
		InteractionDir: filepath.Join(filepath.Dir(usageFile), "interactions"),
		Providers: map[string]config.Provider{
			"openai":   {Name: "openai", Protocol: "openai", BaseURL: "https://openai.test", APIKey: "openai-key"},
			"deepseek": {Name: "deepseek", Protocol: "openai", BaseURL: "https://deepseek.test", APIKey: "deepseek-key"},
		},
	}
	handler := NewHandler(cfg, stats.NewCSVRecorder(usageFile), interactionRecorder, metrics.NewRegistry())
	handler.client.Transport = transport
	request := newRequest(http.MethodPost, "/v1/chat/completions", `{"model":"deepseek-chat","messages":[{"role":"user","content":"hi"}]}`)
	response := newResponseRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	interactionDir := filepath.Join(filepath.Dir(usageFile), "interactions", "000001")
	assertFileContains(t, filepath.Join(interactionDir, "upstream_request.json"), `"provider": "deepseek"`)
}

func TestOpenAICompatibleProviderSelectionByConfiguredModelPattern(t *testing.T) {
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "custom-openai.test" {
			t.Fatalf("unexpected host: %s", r.URL.Host)
		}
		return jsonResponse(`{"choices":[{"message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`), nil
	})

	usageFile := filepath.Join(t.TempDir(), "usage.csv")
	interactionRecorder, err := archive.NewRecorder(filepath.Join(filepath.Dir(usageFile), "interactions"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{
		ListenAddr:     ":0",
		UsageFile:      usageFile,
		InteractionDir: filepath.Join(filepath.Dir(usageFile), "interactions"),
		Providers: map[string]config.Provider{
			"openai":        {Name: "openai", Protocol: "openai", BaseURL: "https://openai.test", APIKey: "openai-key"},
			"custom-openai": {Name: "custom-openai", Protocol: "openai", BaseURL: "https://custom-openai.test", APIKey: "custom-key", Models: []string{"kimi-*"}},
		},
	}
	handler := NewHandler(cfg, stats.NewCSVRecorder(usageFile), interactionRecorder, metrics.NewRegistry())
	handler.client.Transport = transport
	request := newRequest(http.MethodPost, "/v1/chat/completions", `{"model":"kimi-k2","messages":[{"role":"user","content":"hi"}]}`)
	response := newResponseRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	interactionDir := filepath.Join(filepath.Dir(usageFile), "interactions", "000001")
	assertFileContains(t, filepath.Join(interactionDir, "upstream_request.json"), `"provider": "custom-openai"`)
}

func TestOpenAICompatibleFallsBackOnRetryableStatus(t *testing.T) {
	var upstreamHosts []string
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		upstreamHosts = append(upstreamHosts, r.URL.Host)
		switch r.URL.Host {
		case "primary.test":
			return testResponse(http.StatusTooManyRequests, "application/json", `{"error":"rate limited"}`), nil
		case "backup.test":
			if got := r.Header.Get("Authorization"); got != "Bearer backup-key" {
				t.Fatalf("unexpected authorization: %s", got)
			}
			return jsonResponse(`{"choices":[{"message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":2,"completion_tokens":1,"total_tokens":3}}`), nil
		default:
			t.Fatalf("unexpected host: %s", r.URL.Host)
			return nil, nil
		}
	})

	usageFile := filepath.Join(t.TempDir(), "usage.csv")
	interactionRecorder, err := archive.NewRecorder(filepath.Join(filepath.Dir(usageFile), "interactions"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{
		ListenAddr:     ":0",
		UsageFile:      usageFile,
		InteractionDir: filepath.Join(filepath.Dir(usageFile), "interactions"),
		Providers: map[string]config.Provider{
			"primary": {Name: "primary", Protocol: "openai", BaseURL: "https://primary.test", APIKey: "primary-key", Models: []string{"gpt-*"}, Fallbacks: []string{"backup"}},
			"backup":  {Name: "backup", Protocol: "openai", BaseURL: "https://backup.test", APIKey: "backup-key", Models: []string{"gpt-*"}},
		},
	}
	handler := NewHandler(cfg, stats.NewCSVRecorder(usageFile), interactionRecorder, metrics.NewRegistry())
	handler.client.Transport = transport
	request := newRequest(http.MethodPost, "/v1/chat/completions", `{"model":"primary/gpt-test","messages":[{"role":"user","content":"hi"}]}`)
	response := newResponseRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if got := strings.Join(upstreamHosts, ","); got != "primary.test,backup.test" {
		t.Fatalf("upstream hosts = %s", got)
	}
	records := readUsageCSV(t, usageFile)
	if got := records[1][1]; got != "backup" {
		t.Fatalf("provider = %s", got)
	}
	interactionDir := filepath.Join(filepath.Dir(usageFile), "interactions", "000001")
	assertFileContains(t, filepath.Join(interactionDir, "fallback_attempts.json"), `"provider": "primary"`)
	assertFileContains(t, filepath.Join(interactionDir, "fallback_attempts.json"), `"status": 429`)
	assertFileContains(t, filepath.Join(interactionDir, "fallback_attempts.json"), `"provider": "backup"`)
	assertFileContains(t, filepath.Join(interactionDir, "metadata.json"), `"provider": "backup"`)
}

func TestOpenAICompatibleDoesNotFallbackOnClientError(t *testing.T) {
	var upstreamHosts []string
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		upstreamHosts = append(upstreamHosts, r.URL.Host)
		return testResponse(http.StatusBadRequest, "application/json", `{"error":"bad request"}`), nil
	})

	usageFile := filepath.Join(t.TempDir(), "usage.csv")
	interactionRecorder, err := archive.NewRecorder(filepath.Join(filepath.Dir(usageFile), "interactions"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{
		ListenAddr:     ":0",
		UsageFile:      usageFile,
		InteractionDir: filepath.Join(filepath.Dir(usageFile), "interactions"),
		Providers: map[string]config.Provider{
			"primary": {Name: "primary", Protocol: "openai", BaseURL: "https://primary.test", APIKey: "primary-key", Models: []string{"gpt-*"}, Fallbacks: []string{"backup"}},
			"backup":  {Name: "backup", Protocol: "openai", BaseURL: "https://backup.test", APIKey: "backup-key", Models: []string{"gpt-*"}},
		},
	}
	handler := NewHandler(cfg, stats.NewCSVRecorder(usageFile), interactionRecorder, metrics.NewRegistry())
	handler.client.Transport = transport
	request := newRequest(http.MethodPost, "/v1/chat/completions", `{"model":"primary/gpt-test","messages":[{"role":"user","content":"hi"}]}`)
	response := newResponseRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if got := strings.Join(upstreamHosts, ","); got != "primary.test" {
		t.Fatalf("upstream hosts = %s", got)
	}
}

func TestProviderExceptionLogsAreProminent(t *testing.T) {
	var logBuffer strings.Builder
	previousWriter := log.Writer()
	previousFlags := log.Flags()
	previousSlog := slog.Default()
	log.SetOutput(&logBuffer)
	log.SetFlags(0)
	slog.SetDefault(slog.New(slog.NewTextHandler(&combineWriter{&logBuffer}, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer func() {
		log.SetOutput(previousWriter)
		log.SetFlags(previousFlags)
		slog.SetDefault(previousSlog)
	}()

	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return testResponse(http.StatusBadGateway, "application/json", `{"error":"bad gateway"}`), nil
	})

	usageFile := filepath.Join(t.TempDir(), "usage.csv")
	handler := testHandler("https://upstream.test", usageFile, "openai")
	handler.client.Transport = transport
	request := newRequest(http.MethodPost, "/v1/chat/completions", `{"model":"gpt-test","messages":[{"role":"user","content":"hi"}]}`)
	response := newResponseRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusBadGateway {
		t.Fatalf("status = %d", response.Code)
	}
	logs := logBuffer.String()
	if !strings.Contains(logs, "level=ERROR") || !strings.Contains(logs, "upstream alert") {
		t.Fatalf("expected upstream error log, got: %s", logs)
	}
	if !strings.Contains(logs, "provider=openai") {
		t.Fatalf("expected provider in log, got: %s", logs)
	}
	if !strings.Contains(logs, "level=ERROR") || !strings.Contains(logs, "label=error") {
		t.Fatalf("expected summary error log, got: %s", logs)
	}
	if !strings.Contains(logs, "round=1") {
		t.Fatalf("expected round id in logs, got: %s", logs)
	}
}

func TestFallbackLogsNextProvider(t *testing.T) {
	var logBuffer strings.Builder
	previousWriter := log.Writer()
	previousFlags := log.Flags()
	previousSlog := slog.Default()
	log.SetOutput(&logBuffer)
	log.SetFlags(0)
	slog.SetDefault(slog.New(slog.NewTextHandler(&combineWriter{&logBuffer}, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer func() {
		log.SetOutput(previousWriter)
		log.SetFlags(previousFlags)
		slog.SetDefault(previousSlog)
	}()

	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Host {
		case "primary.test":
			return testResponse(http.StatusTooManyRequests, "application/json", `{"error":"rate limited"}`), nil
		case "backup.test":
			return jsonResponse(`{"choices":[{"message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`), nil
		default:
			t.Fatalf("unexpected host: %s", r.URL.Host)
			return nil, nil
		}
	})

	usageFile := filepath.Join(t.TempDir(), "usage.csv")
	interactionRecorder, err := archive.NewRecorder(filepath.Join(filepath.Dir(usageFile), "interactions"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{
		ListenAddr:     ":0",
		UsageFile:      usageFile,
		InteractionDir: filepath.Join(filepath.Dir(usageFile), "interactions"),
		DebugLog:       true,
		Providers: map[string]config.Provider{
			"primary": {Name: "primary", Protocol: "openai", BaseURL: "https://primary.test", APIKey: "primary-key", Models: []string{"gpt-*"}, Fallbacks: []string{"backup"}},
			"backup":  {Name: "backup", Protocol: "openai", BaseURL: "https://backup.test", APIKey: "backup-key", Models: []string{"gpt-*"}},
		},
	}
	handler := NewHandler(cfg, stats.NewCSVRecorder(usageFile), interactionRecorder, metrics.NewRegistry())
	handler.client.Transport = transport
	request := newRequest(http.MethodPost, "/v1/chat/completions", `{"model":"primary/gpt-test","messages":[{"role":"user","content":"hi"}]}`)
	response := newResponseRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d", response.Code)
	}
	logs := logBuffer.String()
	if !strings.Contains(logs, "level=WARN") || !strings.Contains(logs, "upstream alert") {
		t.Fatalf("expected upstream warn log, got: %s", logs)
	}
	if !strings.Contains(logs, "fallback=true") || !strings.Contains(logs, "next_provider=backup") {
		t.Fatalf("expected next provider hint, got: %s", logs)
	}
}

func TestRawProxyUsesBodyModelToResolveProvider(t *testing.T) {
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "deepseek.test" {
			t.Fatalf("unexpected host: %s", r.URL.Host)
		}
		return jsonResponse(`{"ok":true}`), nil
	})

	usageFile := filepath.Join(t.TempDir(), "usage.csv")
	interactionRecorder, err := archive.NewRecorder(filepath.Join(filepath.Dir(usageFile), "interactions"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{
		ListenAddr:     ":0",
		UsageFile:      usageFile,
		InteractionDir: filepath.Join(filepath.Dir(usageFile), "interactions"),
		Providers: map[string]config.Provider{
			"openai":   {Name: "openai", Protocol: "openai", BaseURL: "https://openai.test", APIKey: "openai-key"},
			"deepseek": {Name: "deepseek", Protocol: "openai", BaseURL: "https://deepseek.test", APIKey: "deepseek-key"},
		},
	}
	handler := NewHandler(cfg, stats.NewCSVRecorder(usageFile), interactionRecorder, metrics.NewRegistry())
	handler.client.Transport = transport
	request := newRequest(http.MethodPost, "/v1/responses", `{"model":"deepseek-chat","input":"hi"}`)
	response := newResponseRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	interactionDir := filepath.Join(filepath.Dir(usageFile), "interactions", "000001")
	assertFileContains(t, filepath.Join(interactionDir, "upstream_request.json"), `"provider": "deepseek"`)
	assertFileContains(t, filepath.Join(interactionDir, "response.json"), `"ok":true`)
	records := readUsageCSV(t, usageFile)
	if got := records[1][1]; got != "deepseek" {
		t.Fatalf("provider = %s", got)
	}
}

func TestRawMessagesUsesBodyModelBeforeEndpointProtocol(t *testing.T) {
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() != "https://openai.test/v1/messages" {
			t.Fatalf("unexpected url: %s", r.URL.String())
		}
		if got := r.Header.Get("Authorization"); got != "Bearer openai-key" {
			t.Fatalf("unexpected authorization: %s", got)
		}
		if got := r.Header.Get("X-API-Key"); got != "" {
			t.Fatalf("x-api-key should not be forwarded to openai provider: %s", got)
		}
		return jsonResponse(`{"ok":true}`), nil
	})

	usageFile := filepath.Join(t.TempDir(), "usage.csv")
	interactionRecorder, err := archive.NewRecorder(filepath.Join(filepath.Dir(usageFile), "interactions"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{
		ListenAddr:     ":0",
		UsageFile:      usageFile,
		InteractionDir: filepath.Join(filepath.Dir(usageFile), "interactions"),
		Providers: map[string]config.Provider{
			"openai":    {Name: "openai", Protocol: "openai", BaseURL: "https://openai.test", APIKey: "openai-key"},
			"anthropic": {Name: "anthropic", Protocol: "anthropic", BaseURL: "https://anthropic.test", APIKey: "anthropic-key"},
		},
	}
	handler := NewHandler(cfg, stats.NewCSVRecorder(usageFile), interactionRecorder, metrics.NewRegistry())
	handler.client.Transport = transport
	request := newRequest(http.MethodPost, "/v1/messages", `{"model":"gpt-5.4","input":"hi"}`)
	response := newResponseRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	interactionDir := filepath.Join(filepath.Dir(usageFile), "interactions", "000001")
	assertFileContains(t, filepath.Join(interactionDir, "upstream_request.json"), `"provider": "openai"`)
	records := readUsageCSV(t, usageFile)
	if got := records[1][1]; got != "openai" {
		t.Fatalf("provider = %s", got)
	}
}

func TestOpenAIResponsesEndpointUsesOpenAIProtocolWithoutModel(t *testing.T) {
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() != "https://openai.test/v1/responses" {
			t.Fatalf("unexpected url: %s", r.URL.String())
		}
		if got := r.Header.Get("Authorization"); got != "Bearer openai-key" {
			t.Fatalf("unexpected authorization: %s", got)
		}
		return jsonResponse(`{"id":"resp_1","usage":{"input_tokens":9,"output_tokens":2,"total_tokens":11,"input_tokens_details":{"cached_tokens":3}}}`), nil
	})

	usageFile := filepath.Join(t.TempDir(), "usage.csv")
	interactionRecorder, err := archive.NewRecorder(filepath.Join(filepath.Dir(usageFile), "interactions"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{
		ListenAddr:     ":0",
		UsageFile:      usageFile,
		InteractionDir: filepath.Join(filepath.Dir(usageFile), "interactions"),
		Providers: map[string]config.Provider{
			"openai":    {Name: "openai", Protocol: "openai", BaseURL: "https://openai.test", APIKey: "openai-key"},
			"anthropic": {Name: "anthropic", Protocol: "anthropic", BaseURL: "https://anthropic.test", APIKey: "anthropic-key"},
		},
	}
	handler := NewHandler(cfg, stats.NewCSVRecorder(usageFile), interactionRecorder, metrics.NewRegistry())
	handler.client.Transport = transport
	request := newRequest(http.MethodPost, "/v1/responses", `{"input":"hi"}`)
	response := newResponseRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	records := readUsageCSV(t, usageFile)
	if got := records[1][1]; got != "openai" {
		t.Fatalf("provider = %s", got)
	}
	if got := records[1][10]; got != "3" {
		t.Fatalf("cached input tokens = %s", got)
	}
}

func TestOpenAIResponsesEndpointScopesModelMatchingToOpenAIProtocol(t *testing.T) {
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() != "https://deepseek.test/v1/responses" {
			t.Fatalf("unexpected url: %s", r.URL.String())
		}
		if got := r.Header.Get("Authorization"); got != "Bearer deepseek-key" {
			t.Fatalf("unexpected authorization: %s", got)
		}
		if got := r.Header.Get("X-API-Key"); got != "" {
			t.Fatalf("x-api-key should not be forwarded to openai provider: %s", got)
		}
		return jsonResponse(`{"id":"resp_1","usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`), nil
	})

	usageFile := filepath.Join(t.TempDir(), "usage.csv")
	interactionRecorder, err := archive.NewRecorder(filepath.Join(filepath.Dir(usageFile), "interactions"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{
		ListenAddr:     ":0",
		UsageFile:      usageFile,
		InteractionDir: filepath.Join(filepath.Dir(usageFile), "interactions"),
		Providers: map[string]config.Provider{
			"deepseek":  {Name: "deepseek", Protocol: "openai", BaseURL: "https://deepseek.test", APIKey: "deepseek-key", Models: []string{"deepseek*"}},
			"anthropic": {Name: "anthropic", Protocol: "anthropic", BaseURL: "https://anthropic.test", APIKey: "anthropic-key", Models: []string{"deepseek*"}},
		},
	}
	handler := NewHandler(cfg, stats.NewCSVRecorder(usageFile), interactionRecorder, metrics.NewRegistry())
	handler.client.Transport = transport
	request := newRequest(http.MethodPost, "/v1/responses", `{"model":"deepseek-v4-flash","input":"hi"}`)
	response := newResponseRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	records := readUsageCSV(t, usageFile)
	if got := records[1][1]; got != "deepseek" {
		t.Fatalf("provider = %s", got)
	}
}

func TestRawOpenAIStreamArchivesFullResponse(t *testing.T) {
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		body := strings.Join([]string{
			"data: {\"id\":\"chatcmpl-raw\",\"model\":\"deepseek-chat\",\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}",
			"",
			"data: {\"choices\":[{\"delta\":{\"content\":\"raw\"}}]}",
			"",
			"data: {\"choices\":[{\"delta\":{\"content\":\" stream\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":2,\"total_tokens\":5}}",
			"",
			"data: [DONE]",
			"",
		}, "\n")
		return sseResponse(body), nil
	})

	usageFile := filepath.Join(t.TempDir(), "usage.csv")
	interactionRecorder, err := archive.NewRecorder(filepath.Join(filepath.Dir(usageFile), "interactions"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{
		ListenAddr:     ":0",
		UsageFile:      usageFile,
		InteractionDir: filepath.Join(filepath.Dir(usageFile), "interactions"),
		Providers: map[string]config.Provider{
			"deepseek": {Name: "deepseek", Protocol: "openai", BaseURL: "https://deepseek.test", APIKey: "deepseek-key"},
		},
	}
	handler := NewHandler(cfg, stats.NewCSVRecorder(usageFile), interactionRecorder, metrics.NewRegistry())
	handler.client.Transport = transport
	request := newRequest(http.MethodPost, "/v1/responses", `{"model":"deepseek-chat","stream":true,"input":"hi"}`)
	response := newResponseRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	interactionDir := filepath.Join(filepath.Dir(usageFile), "interactions", "000001")
	assertFileContains(t, filepath.Join(interactionDir, "response.sse"), "data: [DONE]")
	assertFileContains(t, filepath.Join(interactionDir, "response.json"), `"content": "raw stream"`)
	assertFileContains(t, filepath.Join(interactionDir, "metadata.json"), `"full_response_path": "response.json"`)
	records := readUsageCSV(t, usageFile)
	if got := records[1][3]; got != "3" {
		t.Fatalf("input tokens = %s", got)
	}
	if got := records[1][4]; got != "2" {
		t.Fatalf("output tokens = %s", got)
	}
}

func TestAnthropicRawStreamRecordsCacheUsage(t *testing.T) {
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		body := strings.Join([]string{
			`data: {"type":"message_start","message":{"id":"msg_1","model":"claude-test","usage":{"input_tokens":20,"cache_read_input_tokens":8,"cache_creation_input_tokens":3}}}`,
			"",
			`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"hello"}}`,
			"",
			`data: {"type":"message_delta","usage":{"output_tokens":4},"delta":{"stop_reason":"end_turn"}}`,
			"",
			`data: {"type":"message_stop"}`,
			"",
		}, "\n")
		return sseResponse(body), nil
	})

	usageFile := filepath.Join(t.TempDir(), "usage.csv")
	interactionRecorder, err := archive.NewRecorder(filepath.Join(filepath.Dir(usageFile), "interactions"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{
		ListenAddr:     ":0",
		UsageFile:      usageFile,
		InteractionDir: filepath.Join(filepath.Dir(usageFile), "interactions"),
		Providers: map[string]config.Provider{
			"anthropic": {Name: "anthropic", Protocol: "anthropic", BaseURL: "https://anthropic.test", APIKey: "anthropic-key"},
		},
	}
	handler := NewHandler(cfg, stats.NewCSVRecorder(usageFile), interactionRecorder, metrics.NewRegistry())
	handler.client.Transport = transport
	request := newRequest(http.MethodPost, "/v1/messages", `{"model":"claude-test","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	request.Header.Set("Anthropic-Version", "2023-06-01")
	response := newResponseRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	records := readUsageCSV(t, usageFile)
	if got := records[1][10]; got != "8" {
		t.Fatalf("cached input tokens = %s", got)
	}
	if got := records[1][11]; got != "3" {
		t.Fatalf("cache creation input tokens = %s", got)
	}
	if got := records[1][12]; got != "0.4000" {
		t.Fatalf("cache hit rate = %s", got)
	}
	interactionDir := filepath.Join(filepath.Dir(usageFile), "interactions", "000001")
	assertFileContains(t, filepath.Join(interactionDir, "response.json"), `"cache_read_input_tokens": 8`)
	assertFileContains(t, filepath.Join(interactionDir, "metadata.json"), `"cache_hit_rate": 0.4`)
}

func TestDisabledProviderIsSkippedForModelSelection(t *testing.T) {
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "openai.test" {
			t.Fatalf("unexpected host: %s", r.URL.Host)
		}
		return jsonResponse(`{"choices":[{"message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`), nil
	})

	usageFile := filepath.Join(t.TempDir(), "usage.csv")
	interactionRecorder, err := archive.NewRecorder(filepath.Join(filepath.Dir(usageFile), "interactions"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{
		ListenAddr:     ":0",
		UsageFile:      usageFile,
		InteractionDir: filepath.Join(filepath.Dir(usageFile), "interactions"),
		Providers: map[string]config.Provider{
			"openai":   {Name: "openai", Protocol: "openai", BaseURL: "https://openai.test", APIKey: "openai-key"},
			"deepseek": {Name: "deepseek", Protocol: "openai", BaseURL: "https://deepseek.test", APIKey: "deepseek-key", Disabled: true},
		},
	}
	handler := NewHandler(cfg, stats.NewCSVRecorder(usageFile), interactionRecorder, metrics.NewRegistry())
	handler.client.Transport = transport
	request := newRequest(http.MethodPost, "/v1/chat/completions", `{"model":"deepseek-chat","messages":[{"role":"user","content":"hi"}]}`)
	response := newResponseRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	interactionDir := filepath.Join(filepath.Dir(usageFile), "interactions", "000001")
	assertFileContains(t, filepath.Join(interactionDir, "upstream_request.json"), `"provider": "openai"`)
}

func TestDisabledProviderCannotBeExplicitlySelected(t *testing.T) {
	usageFile := filepath.Join(t.TempDir(), "usage.csv")
	interactionRecorder, err := archive.NewRecorder(filepath.Join(filepath.Dir(usageFile), "interactions"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{
		ListenAddr:     ":0",
		UsageFile:      usageFile,
		InteractionDir: filepath.Join(filepath.Dir(usageFile), "interactions"),
		Providers: map[string]config.Provider{
			"deepseek": {Name: "deepseek", Protocol: "openai", BaseURL: "https://deepseek.test", APIKey: "deepseek-key", Disabled: true},
		},
	}
	handler := NewHandler(cfg, stats.NewCSVRecorder(usageFile), interactionRecorder, metrics.NewRegistry())
	request := newRequest(http.MethodPost, "/v1/chat/completions", `{"model":"deepseek/deepseek-chat","messages":[{"role":"user","content":"hi"}]}`)
	response := newResponseRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "provider \"deepseek\" is disabled") {
		t.Fatalf("unexpected body: %s", response.Body.String())
	}
}

func TestBuildUpstreamURLAvoidsDuplicateV1(t *testing.T) {
	incoming, err := http.NewRequest(http.MethodPost, "http://proxy.local/v1/messages?beta=true&provider=openai", nil)
	if err != nil {
		t.Fatal(err)
	}
	got, err := buildUpstreamURL("https://onlycode.shop/v1", incoming.URL)
	if err != nil {
		t.Fatal(err)
	}
	if want := "https://onlycode.shop/v1/messages?beta=true"; got != want {
		t.Fatalf("url = %s, want %s", got, want)
	}
}

func testHandler(baseURL, usageFile, provider string) *Handler {
	interactionRecorder, err := archive.NewRecorder(filepath.Join(filepath.Dir(usageFile), "interactions"))
	if err != nil {
		panic(err)
	}
	cfg := config.Config{
		ListenAddr:     ":0",
		UsageFile:      usageFile,
		InteractionDir: filepath.Join(filepath.Dir(usageFile), "interactions"),
		DebugLog:       true,
		Providers: map[string]config.Provider{
			provider: {Name: provider, Protocol: "openai", BaseURL: baseURL, APIKey: "test-key"},
		},
	}
	return NewHandler(cfg, stats.NewCSVRecorder(usageFile), interactionRecorder, metrics.NewRegistry())
}

func readUsageCSV(t *testing.T, path string) [][]string {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	records, err := csv.NewReader(file).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) < 2 {
		t.Fatalf("expected header and one data row, got %d", len(records))
	}
	return records
}

func assertFileContains(t *testing.T, path, want string) {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), want) {
		t.Fatalf("%s does not contain %q: %s", path, want, body)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

type failingReadCloser struct {
	chunks    [][]byte
	beforeErr func()
	err       error
}

func (r *failingReadCloser) Read(p []byte) (int, error) {
	if len(r.chunks) > 0 {
		chunk := r.chunks[0]
		r.chunks = r.chunks[1:]
		return copy(p, chunk), nil
	}
	if r.beforeErr != nil {
		r.beforeErr()
		r.beforeErr = nil
	}
	return 0, r.err
}

func (r *failingReadCloser) Close() error {
	return nil
}

func jsonResponse(body string) *http.Response {
	return testResponse(http.StatusOK, "application/json", body)
}

func sseResponse(body string) *http.Response {
	return testResponse(http.StatusOK, "text/event-stream", body)
}

func testResponse(status int, contentType, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{contentType}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func newRequest(method, target, body string) *http.Request {
	return httptest.NewRequest(method, target, strings.NewReader(body))
}

func newResponseRecorder() *httptest.ResponseRecorder {
	return httptest.NewRecorder()
}

func TestRequestIDPassesThroughClientHeader(t *testing.T) {
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return jsonResponse(`{"choices":[{"message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`), nil
	})
	usageFile := filepath.Join(t.TempDir(), "usage.csv")
	handler := testHandler("https://upstream.test", usageFile, "openai")
	handler.client.Transport = transport
	request := newRequest(http.MethodPost, "/v1/chat/completions", `{"model":"gpt-test","messages":[{"role":"user","content":"hi"}]}`)
	request.Header.Set(RequestIDHeader, "client-supplied-123")
	response := newResponseRecorder()

	handler.ServeHTTP(response, request)

	if got := response.Header().Get(RequestIDHeader); got != "client-supplied-123" {
		t.Fatalf("X-Request-ID = %q, want client-supplied-123", got)
	}
	interactionDir := filepath.Join(filepath.Dir(usageFile), "interactions", "000001")
	assertFileContains(t, filepath.Join(interactionDir, "metadata.json"), `"request_id": "client-supplied-123"`)
}

func TestRequestIDGeneratedWhenAbsent(t *testing.T) {
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return jsonResponse(`{"choices":[{"message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`), nil
	})
	usageFile := filepath.Join(t.TempDir(), "usage.csv")
	handler := testHandler("https://upstream.test", usageFile, "openai")
	handler.client.Transport = transport
	request := newRequest(http.MethodPost, "/v1/chat/completions", `{"model":"gpt-test","messages":[{"role":"user","content":"hi"}]}`)
	response := newResponseRecorder()

	handler.ServeHTTP(response, request)

	generated := response.Header().Get(RequestIDHeader)
	if len(generated) != 32 {
		t.Fatalf("generated request id length = %d (%q), want 32 hex chars", len(generated), generated)
	}
	interactionDir := filepath.Join(filepath.Dir(usageFile), "interactions", "000001")
	assertFileContains(t, filepath.Join(interactionDir, "metadata.json"), `"request_id": "`+generated+`"`)
}

func TestRequestIDHealthzEchoesAndStoresNothing(t *testing.T) {
	usageFile := filepath.Join(t.TempDir(), "usage.csv")
	handler := testHandler("https://upstream.test", usageFile, "openai")
	request := newRequest(http.MethodGet, "/healthz", "")
	request.Header.Set(RequestIDHeader, "health-42")
	response := newResponseRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d", response.Code)
	}
	if got := response.Header().Get(RequestIDHeader); got != "health-42" {
		t.Fatalf("X-Request-ID = %q, want health-42", got)
	}
}

func TestRequestIDRawProxyAttachesToRound(t *testing.T) {
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return jsonResponse(`{"ok":true}`), nil
	})
	usageFile := filepath.Join(t.TempDir(), "usage.csv")
	handler := testHandler("https://upstream.test/v1", usageFile, "openai")
	handler.client.Transport = transport
	request := newRequest(http.MethodPost, "/v1/responses?beta=true", `{"model":"gpt-test","input":"hi"}`)
	request.Header.Set(RequestIDHeader, "raw-req-7")
	response := newResponseRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d", response.Code)
	}
	interactionDir := filepath.Join(filepath.Dir(usageFile), "interactions", "000001")
	assertFileContains(t, filepath.Join(interactionDir, "metadata.json"), `"request_id": "raw-req-7"`)
}

func TestStablePrefixFingerprintAndDriftRecordedInMetadata(t *testing.T) {
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return jsonResponse(`{"choices":[{"message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`), nil
	})
	usageFile := filepath.Join(t.TempDir(), "usage.csv")
	handler := testHandler("https://upstream.test", usageFile, "openai")
	handler.client.Transport = transport

	// 3 次不同内容的请求,触发漂移事件(阈值 3)
	bodies := []string{
		`{"model":"gpt-test","messages":[{"role":"user","content":"A"}]}`,
		`{"model":"gpt-test","messages":[{"role":"user","content":"B"}]}`,
		`{"model":"gpt-test","messages":[{"role":"user","content":"C"}]}`,
	}
	for _, body := range bodies {
		req := newRequest(http.MethodPost, "/v1/chat/completions", body)
		handler.ServeHTTP(newResponseRecorder(), req)
	}
	interactionDir := filepath.Join(filepath.Dir(usageFile), "interactions", "000003")
	assertFileContains(t, filepath.Join(interactionDir, "metadata.json"), `"stable_prefix_hash"`)
	assertFileContains(t, filepath.Join(interactionDir, "metadata.json"), `"request_fingerprint"`)
	assertFileContains(t, filepath.Join(interactionDir, "metadata.json"), `"stable_prefix_drift": true`)
}

func TestMetricsEndpointRecordedThroughHandler(t *testing.T) {
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return jsonResponse(`{"choices":[{"message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":100,"completion_tokens":50,"total_tokens":150,"prompt_tokens_details":{"cached_tokens":30}}}`), nil
	})
	usageFile := filepath.Join(t.TempDir(), "usage.csv")
	handler := testHandler("https://upstream.test", usageFile, "openai")
	handler.client.Transport = transport
	for i := 0; i < 2; i++ {
		request := newRequest(http.MethodPost, "/v1/chat/completions", `{"model":"gpt-test","messages":[{"role":"user","content":"hi"}]}`)
		handler.ServeHTTP(newResponseRecorder(), request)
	}

	rec := handler.metricsRegistry
	payload, err := rec.StatsJSON()
	if err != nil {
		t.Fatal(err)
	}
	var snap metrics.StatsJSON
	if err := json.Unmarshal(payload, &snap); err != nil {
		t.Fatal(err)
	}
	if snap.Requests.Total != 2 {
		t.Fatalf("metrics requests.total = %d, want 2", snap.Requests.Total)
	}
	if snap.Requests.ByProvider["openai"] != 2 {
		t.Fatalf("metrics openai count = %d, want 2", snap.Requests.ByProvider["openai"])
	}
	if snap.Cache.ByProvider["openai"].Hit != 2 {
		t.Fatalf("cache hits = %d, want 2", snap.Cache.ByProvider["openai"].Hit)
	}
	if snap.Cache.ByProvider["openai"].HitRate != 1.0 {
		t.Fatalf("cache hit rate = %v, want 1.0", snap.Cache.ByProvider["openai"].HitRate)
	}

	statsReq := httptest.NewRequest(http.MethodGet, "/stats", nil)
	statsReq.RemoteAddr = "127.0.0.1:51234"
	statsHandler := metrics.Handler(rec, metrics.HandlerOptions{AllowRemote: false})
	statsRec := httptest.NewRecorder()
	statsHandler.ServeHTTP(statsRec, statsReq)
	if statsRec.Code != http.StatusOK {
		t.Fatalf("stats status = %d, want 200", statsRec.Code)
	}

	metricsReq := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	metricsReq.RemoteAddr = "127.0.0.1:51234"
	metricsHandler := metrics.Handler(rec, metrics.HandlerOptions{AllowRemote: false})
	metricsRec := httptest.NewRecorder()
	metricsHandler.ServeHTTP(metricsRec, metricsReq)
	if metricsRec.Code != http.StatusOK {
		t.Fatalf("metrics status = %d, want 200", metricsRec.Code)
	}
	body := metricsRec.Body.String()
	if !strings.Contains(body, `ai_proxy_requests_total{provider="openai",model="gpt-test",route="chat_completions",status="2xx"} 2`) {
		t.Fatalf("expected chat_completions 2xx counter, got:\n%s", body)
	}
	if !strings.Contains(body, `ai_proxy_cache_hit_rate{provider="openai",model="gpt-test"}`) {
		t.Fatalf("expected cache_hit_rate metric, got:\n%s", body)
	}
}
