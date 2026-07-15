package proxy

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

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
	if got := records[1][11]; got != "4" {
		t.Fatalf("cached input tokens = %s", got)
	}
	if got := records[1][13]; got != "0.5714" {
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

func TestOpenAICompatibleGzipResponseUsage(t *testing.T) {
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if got := r.Header.Get("Accept-Encoding"); got != "" {
			t.Fatalf("accept-encoding should not be forwarded upstream: %s", got)
		}
		return gzipJSONResponse(`{"id":"chatcmpl-1","choices":[{"message":{"role":"assistant","content":"hello"}}],"usage":{"prompt_tokens":27575,"completion_tokens":4293,"total_tokens":31868,"prompt_tokens_details":{"cached_tokens":13440},"prompt_cache_hit_tokens":13440,"prompt_cache_miss_tokens":14135}}`)
	})

	usageFile := filepath.Join(t.TempDir(), "usage.csv")
	handler := testHandler("https://upstream.test", usageFile, "openai")
	handler.client.Transport = transport
	request := newRequest(http.MethodPost, "/v1/chat/completions", `{"model":"gpt-test","messages":[{"role":"user","content":"hi"}]}`)
	request.Header.Set("Accept-Encoding", "gzip")
	response := newResponseRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if got := response.Header().Get("Content-Encoding"); got != "" {
		t.Fatalf("content-encoding should be stripped after decode: %s", got)
	}
	if !strings.Contains(response.Body.String(), `"usage"`) {
		t.Fatalf("expected decoded JSON body, got %q", response.Body.String())
	}
	records := readUsageCSV(t, usageFile)
	if got := records[1][3]; got != "27575" {
		t.Fatalf("input tokens = %s", got)
	}
	if got := records[1][4]; got != "4293" {
		t.Fatalf("output tokens = %s", got)
	}
	if got := records[1][8]; got != "false" {
		t.Fatalf("estimated = %s", got)
	}
	if got := records[1][11]; got != "13440" {
		t.Fatalf("cached input tokens = %s", got)
	}
	if got := records[1][13]; got != "0.4874" {
		t.Fatalf("cache hit rate = %s", got)
	}
	interactionDir := filepath.Join(filepath.Dir(usageFile), "interactions", "000001")
	assertFileContains(t, filepath.Join(interactionDir, "response.json"), `"prompt_tokens":27575`)
	assertFileContains(t, filepath.Join(interactionDir, "metadata.json"), `"cached_input_tokens": 13440`)
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
	if got := records[1][11]; got != "2" {
		t.Fatalf("cached input tokens = %s", got)
	}
	if got := records[1][13]; got != "0.4000" {
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
	handler := NewHandler(mustHandlerConfig(cfg), stats.NewCSVRecorder(usageFile), interactionRecorder, metrics.NewRegistry())
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
	if got := records[1][11]; got != "5" {
		t.Fatalf("cached input tokens = %s", got)
	}
	if got := records[1][12]; got != "2" {
		t.Fatalf("cache creation input tokens = %s", got)
	}
	if got := records[1][13]; got != "0.4545" {
		t.Fatalf("cache hit rate = %s", got)
	}
}

func TestAnthropicNativeAvoidsDuplicateV1AndArchives(t *testing.T) {
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/v1/messages" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		if got := r.URL.RawQuery; got != "beta=true" {
			t.Fatalf("unexpected query: %s", got)
		}
		if got := r.Header.Get("X-API-Key"); got != "test-key" {
			t.Fatalf("unexpected x-api-key: %s", got)
		}
		if got := r.Header.Get("Authorization"); got != "" {
			t.Fatalf("authorization should not be forwarded: %s", got)
		}
		return testResponse(http.StatusNotFound, "application/json", `{"error":"not found"}`), nil
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
			"anthropic": {Name: "anthropic", Protocol: "anthropic", BaseURL: "https://upstream.test/v1", APIKey: "test-key"},
		},
	}
	handler := NewHandler(mustHandlerConfig(cfg), stats.NewCSVRecorder(usageFile), interactionRecorder, metrics.NewRegistry())
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
	handler := NewHandler(mustHandlerConfig(cfg), stats.NewCSVRecorder(usageFile), interactionRecorder, metrics.NewRegistry())
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

func TestUnmatchedModelWithoutDefaultProviderReturns400(t *testing.T) {
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
	handler := NewHandler(mustHandlerConfig(cfg), stats.NewCSVRecorder(usageFile), interactionRecorder, metrics.NewRegistry())
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
	if !strings.Contains(response.Body.String(), "model_not_found") {
		t.Fatalf("unexpected body: %s", response.Body.String())
	}
	records := readUsageCSV(t, usageFile)
	if got := records[1][9]; got != "400" {
		t.Fatalf("http status = %s", got)
	}
}

func TestMultipleProvidersMatchingSameModelReturns400(t *testing.T) {
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
			"primary": {Name: "primary", Protocol: "openai", BaseURL: "https://primary.test", APIKey: "k1", Models: []string{"shared-*"}},
			"backup":  {Name: "backup", Protocol: "openai", BaseURL: "https://backup.test", APIKey: "k2", Models: []string{"shared-*"}},
		},
	}
	handler := NewHandler(mustHandlerConfig(cfg), stats.NewCSVRecorder(usageFile), interactionRecorder, metrics.NewRegistry())
	handler.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		t.Fatalf("request should not reach upstream")
		return nil, nil
	})
	request := newRequest(http.MethodPost, "/v1/chat/completions", `{"model":"shared-model","messages":[{"role":"user","content":"hi"}]}`)
	response := newResponseRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	// 启动/合成 catalog 时多匹配 model 不会进入 authority,运行时返回 model_not_found。
	if !strings.Contains(response.Body.String(), "model_not_found") {
		t.Fatalf("unexpected body: %s", response.Body.String())
	}
}

func TestDefaultProviderNoLongerRoutesUnknownModel(t *testing.T) {
	// catalog 权威:未知 model 不再回落到 default_provider。
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
	handler := NewHandler(mustHandlerConfig(cfg), stats.NewCSVRecorder(usageFile), interactionRecorder, metrics.NewRegistry())
	handler.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		t.Fatalf("request should not reach upstream")
		return nil, nil
	})
	request := newRequest(http.MethodPost, "/v1/chat/completions", `{"model":"healthcheck","messages":[{"role":"user","content":"hi"}]}`)
	response := newResponseRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "model_not_found") {
		t.Fatalf("unexpected body: %s", response.Body.String())
	}
}

func TestLocalModelsEndpointReturnsCatalog(t *testing.T) {
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
			"openai":   {Name: "openai", Protocol: "openai", BaseURL: "https://openai.test", APIKey: "openai-key", Models: []string{"gpt-*"}},
			"deepseek": {Name: "deepseek", Protocol: "openai", BaseURL: "https://deepseek.test", APIKey: "deepseek-key", Models: []string{"deepseek*"}},
		},
		ModelCatalog: map[string]config.ModelInfo{
			"gpt-4o": {
				ID:                  "gpt-4o",
				ContextWindowTokens: 128000,
				MaxOutputTokens:     16384,
				Operations:          []string{config.ModelOperationChatCompletions},
			},
			"deepseek-chat": {
				ID:                  "deepseek-chat",
				ContextWindowTokens: 64000,
				MaxOutputTokens:     8192,
				Operations:          []string{config.ModelOperationChatCompletions, config.ModelOperationEmbeddings},
			},
		},
	}
	handler := NewHandler(mustHandlerConfig(cfg), stats.NewCSVRecorder(usageFile), interactionRecorder, metrics.NewRegistry())
	handler.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		t.Fatalf("GET /v1/models should not reach upstream: %s", r.URL.String())
		return nil, nil
	})
	request := newRequest(http.MethodGet, "/v1/models", "")
	response := newResponseRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	var payload map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload["object"] != "list" {
		t.Fatalf("object = %v", payload["object"])
	}
	data, _ := payload["data"].([]any)
	if len(data) != 2 {
		t.Fatalf("data len = %d, body = %s", len(data), response.Body.String())
	}
	if !strings.Contains(response.Body.String(), `"contextWindowTokens":128000`) && !strings.Contains(response.Body.String(), `"contextWindowTokens": 128000`) {
		t.Fatalf("expected contextWindowTokens, got: %s", response.Body.String())
	}
	if !strings.Contains(response.Body.String(), `"maxOutputTokens":16384`) && !strings.Contains(response.Body.String(), `"maxOutputTokens": 16384`) {
		t.Fatalf("expected maxOutputTokens, got: %s", response.Body.String())
	}
	if !strings.Contains(response.Body.String(), `"owned_by":"openai"`) && !strings.Contains(response.Body.String(), `"owned_by": "openai"`) {
		t.Fatalf("expected owned_by openai for gpt-4o, got: %s", response.Body.String())
	}
	if !strings.Contains(response.Body.String(), `"operations"`) {
		t.Fatalf("expected operations field, got: %s", response.Body.String())
	}
	if !strings.Contains(response.Body.String(), `"chat_completions"`) {
		t.Fatalf("expected chat_completions operation, got: %s", response.Body.String())
	}
	if !strings.Contains(response.Body.String(), `"embeddings"`) {
		t.Fatalf("expected embeddings operation on deepseek-chat, got: %s", response.Body.String())
	}
	interactionDir := filepath.Join(filepath.Dir(usageFile), "interactions", "000001")
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
	handler := NewHandler(mustHandlerConfig(cfg), stats.NewCSVRecorder(usageFile), interactionRecorder, metrics.NewRegistry())
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
	handler := NewHandler(mustHandlerConfig(cfg), stats.NewCSVRecorder(usageFile), interactionRecorder, metrics.NewRegistry())
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
	handler := NewHandler(mustHandlerConfig(cfg), stats.NewCSVRecorder(usageFile), interactionRecorder, metrics.NewRegistry())
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

func TestAnthropicMessagesConvertsToOpenAIChat(t *testing.T) {
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() != "https://openai.test/v1/chat/completions" {
			t.Fatalf("unexpected url: %s", r.URL.String())
		}
		if got := r.Header.Get("Authorization"); got != "Bearer openai-key" {
			t.Fatalf("unexpected authorization: %s", got)
		}
		if got := r.Header.Get("X-API-Key"); got != "" {
			t.Fatalf("x-api-key should not be forwarded to openai provider: %s", got)
		}
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		if payload["model"] != "gpt-5.4" {
			t.Fatalf("model = %v", payload["model"])
		}
		return jsonResponse(`{"id":"chatcmpl-1","model":"gpt-5.4","choices":[{"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`), nil
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
	handler := NewHandler(mustHandlerConfig(cfg), stats.NewCSVRecorder(usageFile), interactionRecorder, metrics.NewRegistry())
	handler.client.Transport = transport
	request := newRequest(http.MethodPost, "/v1/messages", `{"model":"gpt-5.4","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`)
	response := newResponseRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), `"type":"message"`) && !strings.Contains(response.Body.String(), `"type": "message"`) {
		t.Fatalf("expected anthropic message response, got: %s", response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "hello") {
		t.Fatalf("expected converted content, got: %s", response.Body.String())
	}
	interactionDir := filepath.Join(filepath.Dir(usageFile), "interactions", "000001")
	assertFileContains(t, filepath.Join(interactionDir, "upstream_request.json"), `"provider": "openai"`)
	assertFileContains(t, filepath.Join(interactionDir, "upstream_request.json"), `/v1/chat/completions`)
	records := readUsageCSV(t, usageFile)
	if got := records[1][1]; got != "openai" {
		t.Fatalf("provider = %s", got)
	}
}

func TestOpenAIResponsesRequiresModel(t *testing.T) {
	// catalog 权威:无 model 时返回 model_required,不再使用 default_provider。
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
			"openai": {Name: "openai", Protocol: "openai", BaseURL: "https://openai.test", APIKey: "openai-key"},
		},
	}
	handler := NewHandler(mustHandlerConfig(cfg), stats.NewCSVRecorder(usageFile), interactionRecorder, metrics.NewRegistry())
	handler.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		t.Fatalf("request should not reach upstream")
		return nil, nil
	})
	request := newRequest(http.MethodPost, "/v1/responses", `{"input":"hi"}`)
	response := newResponseRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "model_required") {
		t.Fatalf("unexpected body: %s", response.Body.String())
	}
}

func TestOpenAIResponsesRejectsAnthropicProvider(t *testing.T) {
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
			"anthropic": {Name: "anthropic", Protocol: "anthropic", BaseURL: "https://anthropic.test", APIKey: "anthropic-key", Models: []string{"claude*"}},
		},
	}
	handler := NewHandler(mustHandlerConfig(cfg), stats.NewCSVRecorder(usageFile), interactionRecorder, metrics.NewRegistry())
	handler.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		t.Fatalf("request should not reach upstream")
		return nil, nil
	})
	request := newRequest(http.MethodPost, "/v1/responses", `{"model":"claude-test","input":"hi"}`)
	response := newResponseRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	// catalog/endpoint authority: anthropic 不具备 /v1/responses → endpoint_unsupported
	if !strings.Contains(response.Body.String(), "endpoint_unsupported") {
		t.Fatalf("unexpected body: %s", response.Body.String())
	}
}

func TestUnsupportedInboundPathReturns404(t *testing.T) {
	usageFile := filepath.Join(t.TempDir(), "usage.csv")
	handler := testHandler("https://upstream.test", usageFile, "openai")
	request := newRequest(http.MethodPost, "/v1/unknown", `{"model":"gpt-test"}`)
	response := newResponseRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusNotFound {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
}

func TestExplicitProviderHeaderIsIgnored(t *testing.T) {
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
	handler := NewHandler(mustHandlerConfig(cfg), stats.NewCSVRecorder(usageFile), interactionRecorder, metrics.NewRegistry())
	handler.client.Transport = transport
	request := newRequest(http.MethodPost, "/v1/chat/completions", `{"model":"deepseek-chat","messages":[{"role":"user","content":"hi"}]}`)
	request.Header.Set("X-AI-Provider", "openai")
	response := newResponseRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	interactionDir := filepath.Join(filepath.Dir(usageFile), "interactions", "000001")
	assertFileContains(t, filepath.Join(interactionDir, "upstream_request.json"), `"provider": "deepseek"`)
}

func TestRawOpenAIStreamArchivesFullResponse(t *testing.T) {
	// 使用真实 Responses API SSE(response.completed 终止),而非 Chat Completions+[DONE]。
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/v1/responses" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		body := strings.Join([]string{
			`data: {"type":"response.created","response":{"id":"resp_1","model":"deepseek-chat"}}`,
			"",
			`data: {"type":"response.output_text.delta","delta":"raw"}`,
			"",
			`data: {"type":"response.output_text.delta","delta":" stream"}`,
			"",
			`data: {"type":"response.completed","response":{"id":"resp_1","model":"deepseek-chat","usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5}}}`,
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
			"deepseek": {Name: "deepseek", Protocol: "openai", BaseURL: "https://deepseek.test", APIKey: "deepseek-key", Models: []string{"deepseek*"}},
		},
	}
	handler := NewHandler(mustHandlerConfig(cfg), stats.NewCSVRecorder(usageFile), interactionRecorder, metrics.NewRegistry())
	handler.client.Transport = transport
	request := newRequest(http.MethodPost, "/v1/responses", `{"model":"deepseek-chat","stream":true,"input":"hi"}`)
	response := newResponseRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "response.completed") {
		t.Fatalf("client body missing response.completed: %s", response.Body.String())
	}
	interactionDir := filepath.Join(filepath.Dir(usageFile), "interactions", "000001")
	assertFileContains(t, filepath.Join(interactionDir, "response.sse"), "response.completed")
	assertFileContains(t, filepath.Join(interactionDir, "metadata.json"), `"outcome": "success"`)
	// 不应误判为截断
	meta, err := os.ReadFile(filepath.Join(interactionDir, "metadata.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(meta), "upstream_truncated") || strings.Contains(string(meta), "without terminal") {
		t.Fatalf("responses stream should not be truncated: %s", meta)
	}
	records := readUsageCSV(t, usageFile)
	if got := records[1][10]; got != "success" { // outcome column
		t.Fatalf("outcome = %s, want success", got)
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
	handler := NewHandler(mustHandlerConfig(cfg), stats.NewCSVRecorder(usageFile), interactionRecorder, metrics.NewRegistry())
	handler.client.Transport = transport
	request := newRequest(http.MethodPost, "/v1/messages", `{"model":"claude-test","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	request.Header.Set("Anthropic-Version", "2023-06-01")
	response := newResponseRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	records := readUsageCSV(t, usageFile)
	if got := records[1][11]; got != "8" {
		t.Fatalf("cached input tokens = %s", got)
	}
	if got := records[1][12]; got != "3" {
		t.Fatalf("cache creation input tokens = %s", got)
	}
	if got := records[1][13]; got != "0.4000" {
		t.Fatalf("cache hit rate = %s", got)
	}
	interactionDir := filepath.Join(filepath.Dir(usageFile), "interactions", "000001")
	assertFileContains(t, filepath.Join(interactionDir, "response.json"), `"cache_read_input_tokens": 8`)
	assertFileContains(t, filepath.Join(interactionDir, "metadata.json"), `"cache_hit_rate": 0.4`)
}

func TestDisabledProviderIsSkippedForModelSelection(t *testing.T) {
	// disabled provider 不参与 catalog 合成;deepseek-chat 无 route → model_not_found。
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
	handler := NewHandler(mustHandlerConfig(cfg), stats.NewCSVRecorder(usageFile), interactionRecorder, metrics.NewRegistry())
	handler.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		t.Fatalf("request should not reach upstream")
		return nil, nil
	})
	request := newRequest(http.MethodPost, "/v1/chat/completions", `{"model":"deepseek-chat","messages":[{"role":"user","content":"hi"}]}`)
	response := newResponseRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "model_not_found") {
		t.Fatalf("unexpected body: %s", response.Body.String())
	}
}

func TestDisabledProviderIsNotMatchedByModel(t *testing.T) {
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
	handler := NewHandler(mustHandlerConfig(cfg), stats.NewCSVRecorder(usageFile), interactionRecorder, metrics.NewRegistry())
	request := newRequest(http.MethodPost, "/v1/chat/completions", `{"model":"deepseek-chat","messages":[{"role":"user","content":"hi"}]}`)
	response := newResponseRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "model_not_found") {
		t.Fatalf("unexpected body: %s", response.Body.String())
	}
}

func TestBuildUpstreamURLAvoidsDuplicateV1(t *testing.T) {
	incoming, err := http.NewRequest(http.MethodPost, "http://proxy.local/v1/messages?beta=true&provider=openai", nil)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		base string
		want string
	}{
		{
			name: "root v1 base",
			base: "https://onlycode.shop/v1",
			want: "https://onlycode.shop/v1/messages?beta=true",
		},
		{
			name: "nested v1 base",
			base: "https://api.krill-ai.com/codex/v1",
			want: "https://api.krill-ai.com/codex/v1/messages?beta=true",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := buildUpstreamURL(tt.base, incoming.URL)
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Fatalf("url = %s, want %s", got, tt.want)
			}
		})
	}
}

func testHandler(baseURL, usageFile, provider string) *Handler {
	cfg := config.Config{
		ListenAddr:     ":0",
		UsageFile:      usageFile,
		InteractionDir: filepath.Join(filepath.Dir(usageFile), "interactions"),
		DebugLog:       true,
		Providers: map[string]config.Provider{
			provider: {Name: provider, Protocol: "openai", BaseURL: baseURL, APIKey: "test-key"},
		},
	}
	return newTestHandler(cfg, usageFile)
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

func gzipJSONResponse(body string) (*http.Response, error) {
	var buffer bytes.Buffer
	writer := gzip.NewWriter(&buffer)
	if _, err := writer.Write([]byte(body)); err != nil {
		return nil, err
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Header: http.Header{
			"Content-Type":     []string{"application/json"},
			"Content-Encoding": []string{"gzip"},
			"Content-Length":   []string{fmt.Sprintf("%d", buffer.Len())},
		},
		Body: io.NopCloser(bytes.NewReader(buffer.Bytes())),
	}, nil
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
	if !strings.Contains(body, `ai_proxy_requests_total{provider="openai",model="gpt-test",route="chat_completions",status="2xx",outcome="success"} 2`) {
		t.Fatalf("expected chat_completions 2xx counter, got:\n%s", body)
	}
	if !strings.Contains(body, `ai_proxy_cache_hit_rate{provider="openai",model="gpt-test"}`) {
		t.Fatalf("expected cache_hit_rate metric, got:\n%s", body)
	}
}

func TestInboundAPIKeyRequired(t *testing.T) {
	usageFile := filepath.Join(t.TempDir(), "usage.csv")
	handler := testHandler("https://upstream.test", usageFile, "openai")
	handler.cfg.InboundAPIKey = "secret"
	handler.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		t.Fatal("upstream should not be called without auth")
		return nil, nil
	})

	req := newRequest(http.MethodPost, "/v1/chat/completions", `{"model":"gpt-test","messages":[{"role":"user","content":"hi"}]}`)
	rec := newResponseRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}

	req = newRequest(http.MethodPost, "/v1/chat/completions", `{"model":"gpt-test","messages":[{"role":"user","content":"hi"}]}`)
	req.Header.Set("Authorization", "Bearer secret")
	handler.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return jsonResponse(`{"choices":[{"message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`), nil
	})
	rec = newResponseRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}

	req = newRequest(http.MethodPost, "/v1/chat/completions", `{"model":"gpt-test","messages":[{"role":"user","content":"hi"}]}`)
	req.Header.Set("X-API-Key", "secret")
	rec = newResponseRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("x-api-key status = %d", rec.Code)
	}
}

func TestRequestBodyLimit(t *testing.T) {
	usageFile := filepath.Join(t.TempDir(), "usage.csv")
	handler := testHandler("https://upstream.test", usageFile, "openai")
	handler.cfg.MaxRequestBodyBytes = 64
	handler.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		t.Fatal("should not reach upstream")
		return nil, nil
	})
	body := `{"model":"gpt-test","messages":[{"role":"user","content":"` + strings.Repeat("x", 200) + `"}]}`
	req := newRequest(http.MethodPost, "/v1/chat/completions", body)
	rec := newResponseRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestProtocolConversionRejectsTools(t *testing.T) {
	usageFile := filepath.Join(t.TempDir(), "usage.csv")
	interactionRecorder, err := archive.NewRecorder(filepath.Join(filepath.Dir(usageFile), "interactions"))
	if err != nil {
		t.Fatal(err)
	}
	handler := NewHandler(mustHandlerConfig(config.Config{
		ListenAddr:     ":0",
		UsageFile:      usageFile,
		InteractionDir: filepath.Join(filepath.Dir(usageFile), "interactions"),
		DebugLog:       true,
		Providers: map[string]config.Provider{
			"anthropic": {Name: "anthropic", Protocol: "anthropic", BaseURL: "https://upstream.test", APIKey: "k", Models: []string{"claude*"}},
		},
	}), stats.NewCSVRecorder(usageFile), interactionRecorder, metrics.NewRegistry())
	handler.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		t.Fatal("upstream should not be called for unsupported conversion")
		return nil, nil
	})
	body := `{"model":"claude-test","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"x"}}]}`
	req := newRequest(http.MethodPost, "/v1/chat/completions", body)
	rec := newResponseRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "tools") {
		t.Fatalf("body should mention tools: %s", rec.Body.String())
	}
}

func TestHealthzBypassesInboundAuth(t *testing.T) {
	usageFile := filepath.Join(t.TempDir(), "usage.csv")
	handler := testHandler("https://upstream.test", usageFile, "openai")
	handler.cfg.InboundAPIKey = "secret"
	req := newRequest(http.MethodGet, "/healthz", "")
	rec := newResponseRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("healthz status = %d", rec.Code)
	}
}

func TestInboundAPIKeyNotForwardedUpstream(t *testing.T) {
	var gotAuth, gotXAPIKey string
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		gotAuth = r.Header.Get("Authorization")
		gotXAPIKey = r.Header.Get("X-API-Key")
		return jsonResponse(`{"choices":[{"message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`), nil
	})
	usageFile := filepath.Join(t.TempDir(), "usage.csv")
	handler := testHandler("https://upstream.test", usageFile, "openai")
	handler.cfg.InboundAPIKey = "inbound-secret"
	// provider API key is test-key from testHandler
	handler.client.Transport = transport
	req := newRequest(http.MethodPost, "/v1/chat/completions", `{"model":"gpt-test","messages":[{"role":"user","content":"hi"}]}`)
	req.Header.Set("Authorization", "Bearer inbound-secret")
	req.Header.Set("X-API-Key", "inbound-secret")
	rec := newResponseRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if gotAuth != "Bearer test-key" {
		t.Fatalf("upstream Authorization = %q, want provider key", gotAuth)
	}
	if gotXAPIKey != "" {
		t.Fatalf("upstream X-API-Key should be empty, got %q", gotXAPIKey)
	}
}

func TestUpstreamResponseOversizeReturns502(t *testing.T) {
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		body := `{"choices":[{"message":{"role":"assistant","content":"` + strings.Repeat("x", 200) + `"}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`
		return jsonResponse(body), nil
	})
	usageFile := filepath.Join(t.TempDir(), "usage.csv")
	handler := testHandler("https://upstream.test", usageFile, "openai")
	handler.cfg.MaxUpstreamResponseBytes = 64
	handler.client.Transport = transport
	req := newRequest(http.MethodPost, "/v1/chat/completions", `{"model":"gpt-test","messages":[{"role":"user","content":"hi"}]}`)
	rec := newResponseRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "exceeds limit") {
		t.Fatalf("body = %s", rec.Body.String())
	}
}

func TestPrimeStreamBodyTimeout(t *testing.T) {
	pr, pw := io.Pipe()
	// never write body
	defer pr.Close()
	defer pw.Close()
	resp := &http.Response{StatusCode: 200, Body: pr, Header: make(http.Header)}
	start := time.Now()
	_, err := primeStreamBody(resp, 50*time.Millisecond, 1024)
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if !strings.Contains(err.Error(), "timeout") {
		t.Fatalf("error = %q", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("timeout took too long: %s", time.Since(start))
	}
}

func TestPrimeStreamBodyFirstLine(t *testing.T) {
	body := io.NopCloser(strings.NewReader("data: {\"x\":1}\n\ndata: [DONE]\n\n"))
	resp := &http.Response{StatusCode: 200, Body: body, Header: make(http.Header)}
	primed, err := primeStreamBody(resp, time.Second, 1024)
	if err != nil {
		t.Fatal(err)
	}
	all, err := io.ReadAll(primed.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(all), "data: {\"x\":1}") {
		t.Fatalf("body = %q", all)
	}
}

func TestPrimeStreamBodyPartialEOF(t *testing.T) {
	// 无换行的 partial 数据应作为首事件失败返回。
	body := io.NopCloser(strings.NewReader("data: partial-without-newline"))
	resp := &http.Response{StatusCode: 200, Body: body, Header: make(http.Header)}
	_, err := primeStreamBody(resp, time.Second, 1024)
	if err == nil {
		t.Fatal("expected partial EOF to fail")
	}
}

func TestPrimeStreamBodyLineLimit(t *testing.T) {
	// 超长无换行行应被单行上限拦截。
	body := io.NopCloser(strings.NewReader(strings.Repeat("x", 100)))
	resp := &http.Response{StatusCode: 200, Body: body, Header: make(http.Header)}
	_, err := primeStreamBody(resp, time.Second, 16)
	if err == nil {
		t.Fatal("expected line limit error")
	}
	if !strings.Contains(err.Error(), "exceeds limit") {
		t.Fatalf("error = %q", err)
	}
}

func TestStreamCleanEOFWithoutDONEIsTruncated(t *testing.T) {
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		body := "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n"
		return sseResponse(body), nil
	})
	usageFile := filepath.Join(t.TempDir(), "usage.csv")
	handler := testHandler("https://upstream.test", usageFile, "openai")
	handler.client.Transport = transport
	req := newRequest(http.MethodPost, "/v1/chat/completions", `{"model":"gpt-test","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	rec := newResponseRecorder()
	handler.ServeHTTP(rec, req)
	meta, err := os.ReadFile(filepath.Join(filepath.Dir(usageFile), "interactions", "000001", "metadata.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(meta), `"outcome": "upstream_truncated"`) {
		t.Fatalf("expected upstream_truncated, got %s", meta)
	}
	records := readUsageCSV(t, usageFile)
	if got := records[1][10]; got != "upstream_truncated" {
		t.Fatalf("csv outcome = %s", got)
	}
}

func TestNonStream5xxDoesNotDoubleCountUpstreamError(t *testing.T) {
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusBadGateway,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"error":"bad"}`)),
		}, nil
	})
	usageFile := filepath.Join(t.TempDir(), "usage.csv")
	handler := testHandler("https://upstream.test", usageFile, "openai")
	handler.client.Transport = transport
	req := newRequest(http.MethodPost, "/v1/chat/completions", `{"model":"gpt-test","messages":[{"role":"user","content":"hi"}]}`)
	handler.ServeHTTP(newResponseRecorder(), req)

	// 应只有一次 upstream error(真实 502),不应再追加 -2
	payload, err := handler.metricsRegistry.StatsJSON()
	if err != nil {
		t.Fatal(err)
	}
	var snap metrics.StatsJSON
	if err := json.Unmarshal(payload, &snap); err != nil {
		t.Fatal(err)
	}
	// ByStatusCode should have 502 once; -2 should be absent or zero for this path
	if snap.Errors.ByStatusCode["502"] != 1 {
		t.Fatalf("502 count = %v, want 1", snap.Errors.ByStatusCode)
	}
	if snap.Errors.ByStatusCode["-2"] != 0 {
		t.Fatalf("unexpected midflight -2 count: %v", snap.Errors.ByStatusCode["-2"])
	}
}

func TestResponsesStreamFailedOutcome(t *testing.T) {
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		body := strings.Join([]string{
			`data: {"type":"response.created","response":{"id":"resp_f","model":"deepseek-chat"}}`,
			"",
			`data: {"type":"response.failed","response":{"id":"resp_f","error":{"message":"provider down","code":"server_error"}}}`,
			"",
		}, "\n")
		return sseResponse(body), nil
	})
	usageFile := filepath.Join(t.TempDir(), "usage.csv")
	handler := testHandler("https://upstream.test", usageFile, "openai")
	// ensure model matches
	handler.cfg.Providers["openai"] = config.Provider{Name: "openai", Protocol: "openai", BaseURL: "https://upstream.test", APIKey: "k", Models: []string{"deepseek*"}}
	handler.cfg.ModelCatalog = nil
	handler.cfg = mustHandlerConfig(handler.cfg)
	handler.client.Transport = transport
	req := newRequest(http.MethodPost, "/v1/responses", `{"model":"deepseek-chat","stream":true,"input":"hi"}`)
	handler.ServeHTTP(newResponseRecorder(), req)

	meta, err := os.ReadFile(filepath.Join(filepath.Dir(usageFile), "interactions", "000001", "metadata.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(meta), `"outcome": "upstream_failed"`) {
		t.Fatalf("expected upstream_failed, got %s", meta)
	}
	records := readUsageCSV(t, usageFile)
	if got := records[1][10]; got != "upstream_failed" {
		t.Fatalf("csv outcome = %s", got)
	}
	// 应计入 upstream error
	payload, err := handler.metricsRegistry.StatsJSON()
	if err != nil {
		t.Fatal(err)
	}
	var snap metrics.StatsJSON
	if err := json.Unmarshal(payload, &snap); err != nil {
		t.Fatal(err)
	}
	if snap.Errors.ByStatusCode["-2"] < 1 {
		t.Fatalf("expected midflight/upstream error count, got %v", snap.Errors.ByStatusCode)
	}
}

func TestResponsesStreamRecordsUsageFromCompleted(t *testing.T) {
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		body := strings.Join([]string{
			`data: {"type":"response.created","response":{"id":"resp_1","model":"deepseek-chat"}}`,
			"",
			`data: {"type":"response.output_text.delta","delta":"hello"}`,
			"",
			`data: {"type":"response.completed","response":{"id":"resp_1","model":"deepseek-chat","usage":{"input_tokens":11,"output_tokens":7,"total_tokens":18}}}`,
			"",
		}, "\n")
		return sseResponse(body), nil
	})
	usageFile := filepath.Join(t.TempDir(), "usage.csv")
	handler := testHandler("https://upstream.test", usageFile, "openai")
	handler.cfg.Providers["openai"] = config.Provider{Name: "openai", Protocol: "openai", BaseURL: "https://upstream.test", APIKey: "k", Models: []string{"deepseek*"}}
	handler.cfg.ModelCatalog = nil
	handler.cfg = mustHandlerConfig(handler.cfg)
	handler.client.Transport = transport
	req := newRequest(http.MethodPost, "/v1/responses", `{"model":"deepseek-chat","stream":true,"input":"hi"}`)
	handler.ServeHTTP(newResponseRecorder(), req)

	records := readUsageCSV(t, usageFile)
	if got := records[1][3]; got != "11" {
		t.Fatalf("input tokens = %s, want 11 from response.completed", got)
	}
	if got := records[1][4]; got != "7" {
		t.Fatalf("output tokens = %s, want 7 from response.completed", got)
	}
	if got := records[1][10]; got != "success" {
		t.Fatalf("outcome = %s", got)
	}
	if got := records[1][8]; got != "false" {
		t.Fatalf("estimated = %s, want false (real usage)", got)
	}
}

func TestBufferedErrorMetadataOutcomeNotSuccess(t *testing.T) {
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusBadGateway,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"error":"nope"}`)),
		}, nil
	})
	usageFile := filepath.Join(t.TempDir(), "usage.csv")
	handler := testHandler("https://upstream.test", usageFile, "openai")
	handler.client.Transport = transport
	req := newRequest(http.MethodPost, "/v1/chat/completions", `{"model":"gpt-test","messages":[{"role":"user","content":"hi"}]}`)
	handler.ServeHTTP(newResponseRecorder(), req)
	meta, err := os.ReadFile(filepath.Join(filepath.Dir(usageFile), "interactions", "000001", "metadata.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(meta), `"outcome": "success"`) {
		t.Fatalf("5xx metadata should not be success: %s", meta)
	}
	if !strings.Contains(string(meta), `"outcome": "error"`) {
		t.Fatalf("expected outcome error, got %s", meta)
	}
}

func TestRemoveHopByHopDynamicConnection(t *testing.T) {
	header := http.Header{}
	header.Set("Connection", "X-Custom-Hop, Keep-Alive")
	header.Set("X-Custom-Hop", "should-go")
	header.Set("Keep-Alive", "timeout=5")
	header.Set("Proxy-Connection", "keep-alive")
	header.Set("Transfer-Encoding", "chunked")
	header.Set("Content-Type", "application/json")
	header.Set("X-Request-Id", "keep-me")
	removeHopByHop(header)
	if header.Get("X-Custom-Hop") != "" {
		t.Fatal("dynamic Connection header should be removed")
	}
	if header.Get("Keep-Alive") != "" || header.Get("Transfer-Encoding") != "" || header.Get("Connection") != "" || header.Get("Proxy-Connection") != "" {
		t.Fatalf("standard hop-by-hop remain: %v", header)
	}
	if header.Get("Content-Type") != "application/json" || header.Get("X-Request-Id") != "keep-me" {
		t.Fatalf("end-to-end headers stripped: %v", header)
	}
}

func TestCopyResponseHeaderStripsHopByHop(t *testing.T) {
	src := http.Header{}
	src.Set("Content-Type", "text/event-stream")
	src.Set("Transfer-Encoding", "chunked")
	src.Set("Connection", "close")
	dst := http.Header{}
	copyResponseHeader(dst, src)
	if dst.Get("Content-Type") != "text/event-stream" {
		t.Fatal("content-type missing")
	}
	if dst.Get("Transfer-Encoding") != "" || dst.Get("Connection") != "" {
		t.Fatalf("hop-by-hop leaked: %v", dst)
	}
}

func TestConversionStreamReturnsAfterDONEWithoutWaitingEOF(t *testing.T) {
	// OpenAI→Anthropic:上游在 [DONE] 后保持连接,处理器应立即结束并补发终止事件。
	pr, pw := io.Pipe()
	go func() {
		defer pw.Close()
		chunks := []string{
			`data: {"id":"chatcmpl-1","choices":[{"delta":{"role":"assistant"}}]}` + "\n\n",
			`data: {"choices":[{"delta":{"content":"hi"}}]}` + "\n\n",
			`data: [DONE]` + "\n\n",
		}
		for _, c := range chunks {
			if _, err := pw.Write([]byte(c)); err != nil {
				return
			}
		}
		// 故意不关闭:若实现继续读会挂起直到测试超时。
		time.Sleep(2 * time.Second)
	}()

	usageFile := filepath.Join(t.TempDir(), "usage.csv")
	interactionRecorder, err := archive.NewRecorder(filepath.Join(filepath.Dir(usageFile), "interactions"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{
		ListenAddr:        ":0",
		UsageFile:         usageFile,
		InteractionDir:    filepath.Join(filepath.Dir(usageFile), "interactions"),
		StreamIdleTimeout: 5 * time.Second,
		Providers: map[string]config.Provider{
			"openai": {Name: "openai", Protocol: "openai", BaseURL: "https://upstream.test", APIKey: "k", Models: []string{"gpt*"}},
		},
	}
	handler := NewHandler(mustHandlerConfig(cfg), stats.NewCSVRecorder(usageFile), interactionRecorder, metrics.NewRegistry())
	handler.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       pr,
		}, nil
	})

	// 走 /v1/messages 命中 openai provider → 转换流
	// 需要 model 匹配:用 gpt-test 但 path /v1/messages
	// resolveProvider 用 body.model
	done := make(chan struct{})
	var rec *httptest.ResponseRecorder
	go func() {
		defer close(done)
		req := newRequest(http.MethodPost, "/v1/messages", `{"model":"gpt-test","stream":true,"messages":[{"role":"user","content":"hi"}],"max_tokens":16}`)
		rec = newResponseRecorder()
		handler.ServeHTTP(rec, req)
	}()

	select {
	case <-done:
		// ok: returned without waiting for pipe close
	case <-time.After(1500 * time.Millisecond):
		t.Fatal("conversion stream did not return after [DONE]; still waiting on upstream")
	}
	if rec == nil || rec.Code != http.StatusOK {
		t.Fatalf("status = %v", rec)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "message_stop") {
		t.Fatalf("expected anthropic message_stop in converted stream, got: %s", body)
	}
}

func TestLogStreamFailEmitsProtocolOutcome(t *testing.T) {
	var buf strings.Builder
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(prev)

	h := testHandler("https://upstream.test", filepath.Join(t.TempDir(), "u.csv"), "openai")
	fail := newStreamFail(streamKindProtocol, "convert anthropic stream: invalid SSE JSON", fmt.Errorf("invalid SSE JSON"), true)
	h.logStreamFail(nil, "anthropic", "claude", fail)
	logs := buf.String()
	if !strings.Contains(logs, "outcome=protocol") && !strings.Contains(logs, `outcome":"protocol"`) && !strings.Contains(logs, "outcome=protocol") {
		// text handler: outcome=protocol
		if !strings.Contains(logs, "protocol") {
			t.Fatalf("expected protocol in logs: %s", logs)
		}
	}
	if strings.Contains(logs, "outcome=conversion") {
		t.Fatalf("protocol should not be reclassified as conversion: %s", logs)
	}
}

func TestAnthropicToOpenAIStreamReturnsAfterMessageStop(t *testing.T) {
	// Anthropic 上游发送 message_stop 后保持连接;转换流应立即补发 [DONE] 并返回。
	pr, pw := io.Pipe()
	go func() {
		defer pw.Close()
		chunks := []string{
			`data: {"type":"message_start","message":{"id":"msg_1","model":"claude-test","usage":{"input_tokens":3,"output_tokens":0}}}` + "\n\n",
			`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"hi"}}` + "\n\n",
			`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}` + "\n\n",
			`data: {"type":"message_stop"}` + "\n\n",
		}
		for _, c := range chunks {
			if _, err := pw.Write([]byte(c)); err != nil {
				return
			}
		}
		time.Sleep(2 * time.Second) // 故意不关 pipe
	}()

	usageFile := filepath.Join(t.TempDir(), "usage.csv")
	interactionRecorder, err := archive.NewRecorder(filepath.Join(filepath.Dir(usageFile), "interactions"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{
		ListenAddr:        ":0",
		UsageFile:         usageFile,
		InteractionDir:    filepath.Join(filepath.Dir(usageFile), "interactions"),
		StreamIdleTimeout: 5 * time.Second,
		Providers: map[string]config.Provider{
			"anthropic": {Name: "anthropic", Protocol: "anthropic", BaseURL: "https://upstream.test", APIKey: "k", Models: []string{"claude*"}},
		},
	}
	handler := NewHandler(mustHandlerConfig(cfg), stats.NewCSVRecorder(usageFile), interactionRecorder, metrics.NewRegistry())
	handler.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/v1/messages" {
			t.Errorf("upstream path = %s", r.URL.Path)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       pr,
		}, nil
	})

	done := make(chan struct{})
	var rec *httptest.ResponseRecorder
	go func() {
		defer close(done)
		// OpenAI 客户端路径 → Anthropic 上游转换流
		req := newRequest(http.MethodPost, "/v1/chat/completions", `{"model":"claude-test","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
		rec = newResponseRecorder()
		handler.ServeHTTP(rec, req)
	}()

	select {
	case <-done:
	case <-time.After(1500 * time.Millisecond):
		t.Fatal("anthropic→openai stream did not return after message_stop")
	}
	if rec == nil || rec.Code != http.StatusOK {
		t.Fatalf("status = %v", rec)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("expected OpenAI [DONE] after conversion, got: %s", body)
	}
}

func TestOperationUnsupportedRejectsBeforeUpstream(t *testing.T) {
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
			"openai": {Name: "openai", Protocol: "openai", BaseURL: "https://openai.test", APIKey: "k", Models: []string{"gpt-chat", "embed-model"}},
		},
		ModelCatalog: map[string]config.ModelInfo{
			"gpt-chat": {
				ID: "gpt-chat", ContextWindowTokens: 128000, MaxOutputTokens: 16384,
				Operations: []string{config.ModelOperationChatCompletions}, RouteOwner: "openai",
			},
			"embed-model": {
				ID: "embed-model", ContextWindowTokens: 8192, MaxOutputTokens: 8191,
				Operations: []string{config.ModelOperationEmbeddings}, RouteOwner: "openai",
			},
		},
	}
	handler := NewHandler(mustHandlerConfig(cfg), stats.NewCSVRecorder(usageFile), interactionRecorder, metrics.NewRegistry())
	upstreamHits := 0
	handler.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		upstreamHits++
		t.Fatalf("upstream should not be called for operation_unsupported")
		return nil, nil
	})

	// chat-only model calling embeddings
	req := newRequest(http.MethodPost, "/v1/embeddings", `{"model":"gpt-chat","input":"hi"}`)
	resp := newResponseRecorder()
	handler.ServeHTTP(resp, req)
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s", resp.Code, resp.Body.String())
	}
	if !strings.Contains(resp.Body.String(), "operation_unsupported") {
		t.Fatalf("body = %s", resp.Body.String())
	}

	// embedding-only model calling chat
	req = newRequest(http.MethodPost, "/v1/chat/completions", `{"model":"embed-model","messages":[{"role":"user","content":"hi"}]}`)
	resp = newResponseRecorder()
	handler.ServeHTTP(resp, req)
	if resp.Code != http.StatusBadRequest || !strings.Contains(resp.Body.String(), "operation_unsupported") {
		t.Fatalf("status=%d body=%s", resp.Code, resp.Body.String())
	}
	if upstreamHits != 0 {
		t.Fatalf("upstream hits = %d", upstreamHits)
	}
}

func TestSupportedOperationForwardsUpstream(t *testing.T) {
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
			"openai": {Name: "openai", Protocol: "openai", BaseURL: "https://openai.test", APIKey: "k", Models: []string{"gpt-chat"}},
		},
		ModelCatalog: map[string]config.ModelInfo{
			"gpt-chat": {
				ID: "gpt-chat", ContextWindowTokens: 128000, MaxOutputTokens: 16384,
				Operations: []string{config.ModelOperationChatCompletions}, RouteOwner: "openai",
			},
		},
	}
	handler := NewHandler(mustHandlerConfig(cfg), stats.NewCSVRecorder(usageFile), interactionRecorder, metrics.NewRegistry())
	handler.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "openai.test" {
			t.Fatalf("host = %s", r.URL.Host)
		}
		return jsonResponse(`{"choices":[{"message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`), nil
	})
	req := newRequest(http.MethodPost, "/v1/chat/completions", `{"model":"gpt-chat","messages":[{"role":"user","content":"hi"}]}`)
	resp := newResponseRecorder()
	handler.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.Code, resp.Body.String())
	}
}

func TestModelsListResponseUsesTypedDTO(t *testing.T) {
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
			"openai": {Name: "openai", Protocol: "openai", BaseURL: "https://openai.test", APIKey: "k", Models: []string{"gpt-4o"}},
		},
		ModelCatalog: map[string]config.ModelInfo{
			"gpt-4o": {
				ID: "gpt-4o", ContextWindowTokens: 128000, MaxOutputTokens: 16384,
				Operations: []string{config.ModelOperationChatCompletions}, RouteOwner: "openai",
			},
		},
	}
	handler := NewHandler(mustHandlerConfig(cfg), stats.NewCSVRecorder(usageFile), interactionRecorder, metrics.NewRegistry())
	req := newRequest(http.MethodGet, "/v1/models", "")
	resp := newResponseRecorder()
	handler.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.Code, resp.Body.String())
	}
	var payload ModelsListResponse
	if err := json.Unmarshal(resp.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Object != "list" || len(payload.Data) != 1 {
		t.Fatalf("payload = %#v", payload)
	}
	if payload.Data[0].OwnedBy != "openai" || len(payload.Data[0].Operations) != 1 || payload.Data[0].Operations[0] != "chat_completions" {
		t.Fatalf("record = %#v", payload.Data[0])
	}
}

func TestModelsGETAndPOSTConsistent(t *testing.T) {
	usageFile := filepath.Join(t.TempDir(), "usage.csv")
	interactionRecorder, err := archive.NewRecorder(filepath.Join(filepath.Dir(usageFile), "interactions"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := mustHandlerConfig(config.Config{
		ListenAddr:     ":0",
		UsageFile:      usageFile,
		InteractionDir: filepath.Join(filepath.Dir(usageFile), "interactions"),
		Providers: map[string]config.Provider{
			"openai": {Name: "openai", Protocol: "openai", BaseURL: "https://openai.test", APIKey: "k", Models: []string{"gpt-4o", "emb"}},
		},
		ModelCatalog: map[string]config.ModelInfo{
			"emb": {
				ID: "emb", ContextWindowTokens: 8192, MaxOutputTokens: 8191,
				Operations: []string{config.ModelOperationEmbeddings}, RouteOwner: "openai",
			},
			"gpt-4o": {
				ID: "gpt-4o", ContextWindowTokens: 128000, MaxOutputTokens: 16384,
				Operations: []string{config.ModelOperationChatCompletions}, RouteOwner: "openai",
			},
		},
	})
	handler := NewHandler(cfg, stats.NewCSVRecorder(usageFile), interactionRecorder, metrics.NewRegistry())
	getRec := newResponseRecorder()
	handler.ServeHTTP(getRec, newRequest(http.MethodGet, "/v1/models", ""))
	postRec := newResponseRecorder()
	handler.ServeHTTP(postRec, newRequest(http.MethodPost, "/v1/models", ""))
	if getRec.Code != 200 || postRec.Code != 200 {
		t.Fatalf("status get=%d post=%d", getRec.Code, postRec.Code)
	}
	var getPayload, postPayload ModelsListResponse
	if err := json.Unmarshal(getRec.Body.Bytes(), &getPayload); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(postRec.Body.Bytes(), &postPayload); err != nil {
		t.Fatal(err)
	}
	if len(getPayload.Data) != 2 || len(postPayload.Data) != 2 {
		t.Fatalf("len get=%d post=%d", len(getPayload.Data), len(postPayload.Data))
	}
	// stable order by case-fold id: emb, gpt-4o
	if getPayload.Data[0].ID != "emb" || getPayload.Data[1].ID != "gpt-4o" {
		t.Fatalf("order = %q %q", getPayload.Data[0].ID, getPayload.Data[1].ID)
	}
	getBody, err := json.Marshal(getPayload)
	if err != nil {
		t.Fatal(err)
	}
	postBody, err := json.Marshal(postPayload)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(getPayload, postPayload) {
		t.Fatalf("GET/POST payload mismatch: get=%s post=%s", getBody, postBody)
	}
	if getPayload.Data[0].OwnedBy != "openai" || getPayload.Data[1].OwnedBy != "openai" {
		t.Fatalf("owned_by = %#v", getPayload.Data)
	}
}

func TestEmbeddingOnlyRejectsMessagesResponsesCompletions(t *testing.T) {
	usageFile := filepath.Join(t.TempDir(), "usage.csv")
	interactionRecorder, err := archive.NewRecorder(filepath.Join(filepath.Dir(usageFile), "interactions"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := mustHandlerConfig(config.Config{
		ListenAddr:     ":0",
		UsageFile:      usageFile,
		InteractionDir: filepath.Join(filepath.Dir(usageFile), "interactions"),
		Providers: map[string]config.Provider{
			"openai": {Name: "openai", Protocol: "openai", BaseURL: "https://openai.test", APIKey: "k", Models: []string{"emb-only"}},
		},
		ModelCatalog: map[string]config.ModelInfo{
			"emb-only": {
				ID: "emb-only", ContextWindowTokens: 8192, MaxOutputTokens: 8191,
				Operations: []string{config.ModelOperationEmbeddings}, RouteOwner: "openai",
			},
		},
	})
	handler := NewHandler(cfg, stats.NewCSVRecorder(usageFile), interactionRecorder, metrics.NewRegistry())
	hits := 0
	handler.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		hits++
		t.Fatalf("upstream should not be called: %s", r.URL.Path)
		return nil, nil
	})
	for _, path := range []string{"/v1/messages", "/v1/responses", "/v1/completions", "/v1/chat/completions"} {
		rec := newResponseRecorder()
		body := `{"model":"emb-only","messages":[{"role":"user","content":"hi"}],"input":"hi","prompt":"hi"}`
		handler.ServeHTTP(rec, newRequest(http.MethodPost, path, body))
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "operation_unsupported") {
			t.Fatalf("path %s status=%d body=%s", path, rec.Code, rec.Body.String())
		}
	}
	if hits != 0 {
		t.Fatalf("upstream hits=%d", hits)
	}
}

func TestAPIErrorDoesNotLeakSecrets(t *testing.T) {
	usageFile := filepath.Join(t.TempDir(), "usage.csv")
	interactionRecorder, err := archive.NewRecorder(filepath.Join(filepath.Dir(usageFile), "interactions"))
	if err != nil {
		t.Fatal(err)
	}
	secret := "sk-super-secret-key-should-not-leak"
	cfg := mustHandlerConfig(config.Config{
		ListenAddr:     ":0",
		UsageFile:      usageFile,
		InteractionDir: filepath.Join(filepath.Dir(usageFile), "interactions"),
		Providers: map[string]config.Provider{
			"openai": {Name: "openai", Protocol: "openai", BaseURL: "https://openai.test", APIKey: secret, Models: []string{"gpt-chat"}},
		},
		ModelCatalog: map[string]config.ModelInfo{
			"gpt-chat": {
				ID: "gpt-chat", ContextWindowTokens: 128000, MaxOutputTokens: 16384,
				Operations: []string{config.ModelOperationChatCompletions}, RouteOwner: "openai",
			},
		},
	})
	handler := NewHandler(cfg, stats.NewCSVRecorder(usageFile), interactionRecorder, metrics.NewRegistry())
	rec := newResponseRecorder()
	handler.ServeHTTP(rec, newRequest(http.MethodPost, "/v1/embeddings", `{"model":"gpt-chat","input":"hi"}`))
	body := rec.Body.String()
	if strings.Contains(body, secret) || strings.Contains(body, "Authorization") {
		t.Fatalf("error leaked secret: %s", body)
	}
	if !strings.Contains(body, "operation_unsupported") {
		t.Fatalf("body=%s", body)
	}
}

func TestNewHandlerRejectsUnresolvedConfig(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic for unresolved config")
		}
	}()
	_ = NewHandler(config.Config{
		Providers: map[string]config.Provider{
			"openai": {Name: "openai", Protocol: "openai", BaseURL: "https://x", APIKey: "k"},
		},
		ModelCatalog: map[string]config.ModelInfo{
			"gpt-x": {ID: "gpt-x", ContextWindowTokens: 100, MaxOutputTokens: 10, Operations: []string{"chat_completions"}},
		},
	}, stats.NewCSVRecorder(filepath.Join(t.TempDir(), "u.csv")), nil, metrics.NewRegistry())
}

func TestNewHandlerAllowsEmptyCatalog(t *testing.T) {
	cfg := config.Config{
		Providers: map[string]config.Provider{
			"openai": {
				Name: "openai", Protocol: "openai", BaseURL: "https://x", APIKey: "k",
				EndpointCapabilities: []string{config.EndpointCapabilityChatCompletions},
			},
		},
		ModelCatalog: map[string]config.ModelInfo{},
	}
	h := NewHandler(cfg, stats.NewCSVRecorder(filepath.Join(t.TempDir(), "u.csv")), nil, metrics.NewRegistry())
	if h == nil {
		t.Fatal("handler nil")
	}
}

func TestRequireResolvedConfigRejectsUnknownProtocol(t *testing.T) {
	err := requireResolvedConfig(config.Config{
		Providers: map[string]config.Provider{
			"weird": {
				Name: "weird", Protocol: "foo", BaseURL: "https://x", APIKey: "k",
				EndpointCapabilities: []string{config.EndpointCapabilityChatCompletions},
			},
		},
		ModelCatalog: map[string]config.ModelInfo{},
	})
	if err == nil || !strings.Contains(err.Error(), "unknown protocol") {
		t.Fatalf("error = %v", err)
	}
}

func TestRequireResolvedConfigRejectsDuplicateCapabilities(t *testing.T) {
	err := requireResolvedConfig(config.Config{
		Providers: map[string]config.Provider{
			"openai": {
				Name: "openai", Protocol: "openai", BaseURL: "https://x", APIKey: "k",
				EndpointCapabilities: []string{
					config.EndpointCapabilityChatCompletions,
					config.EndpointCapabilityChatCompletions,
				},
			},
		},
		ModelCatalog: map[string]config.ModelInfo{},
	})
	if err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("error = %v", err)
	}
}

func TestRequireResolvedConfigRejectsUnsortedOperations(t *testing.T) {
	err := requireResolvedConfig(config.Config{
		Providers: map[string]config.Provider{
			"openai": {
				Name: "openai", Protocol: "openai", BaseURL: "https://x", APIKey: "k",
				Models: []string{"multi"},
				EndpointCapabilities: []string{
					config.EndpointCapabilityChatCompletions,
					config.EndpointCapabilityEmbeddings,
				},
			},
		},
		ModelCatalog: map[string]config.ModelInfo{
			"multi": {
				ID: "multi", ContextWindowTokens: 1000, MaxOutputTokens: 100,
				// embeddings 应在 chat_completions 之后
				Operations: []string{config.ModelOperationEmbeddings, config.ModelOperationChatCompletions},
				RouteOwner: "openai",
			},
		},
	})
	if err == nil || !strings.Contains(err.Error(), "sorted") {
		t.Fatalf("error = %v", err)
	}
}

func TestRequireResolvedConfigAllowsEmptyCatalog(t *testing.T) {
	err := requireResolvedConfig(config.Config{
		Providers: map[string]config.Provider{
			"openai": {
				Name: "openai", Protocol: "openai", BaseURL: "https://x", APIKey: "k",
				EndpointCapabilities: []string{config.EndpointCapabilityChatCompletions},
			},
		},
		ModelCatalog: map[string]config.ModelInfo{},
	})
	if err != nil {
		t.Fatalf("empty catalog should be allowed: %v", err)
	}
}

func TestRetryableUpstreamErrorDoesNotSwitchProvider(t *testing.T) {
	// 5xx 仅打唯一 RouteOwner,不切换其它 provider。
	usageFile := filepath.Join(t.TempDir(), "usage.csv")
	interactionRecorder, err := archive.NewRecorder(filepath.Join(filepath.Dir(usageFile), "interactions"))
	if err != nil {
		t.Fatal(err)
	}
	hosts := []string{}
	cfg := mustHandlerConfig(config.Config{
		ListenAddr:     ":0",
		UsageFile:      usageFile,
		InteractionDir: filepath.Join(filepath.Dir(usageFile), "interactions"),
		Providers: map[string]config.Provider{
			"primary": {Name: "primary", Protocol: "openai", BaseURL: "https://primary.test", APIKey: "k", Models: []string{"gpt-test"}},
			"backup":  {Name: "backup", Protocol: "openai", BaseURL: "https://backup.test", APIKey: "k", Models: []string{"other-*"}},
		},
		ModelCatalog: map[string]config.ModelInfo{
			"gpt-test": {
				ID: "gpt-test", ContextWindowTokens: 128000, MaxOutputTokens: 16384,
				Operations: []string{config.ModelOperationChatCompletions}, RouteOwner: "primary",
			},
		},
	})
	handler := NewHandler(cfg, stats.NewCSVRecorder(usageFile), interactionRecorder, metrics.NewRegistry())
	handler.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		hosts = append(hosts, r.URL.Host)
		return testResponse(http.StatusBadGateway, "application/json", `{"error":"bad gateway"}`), nil
	})
	rec := newResponseRecorder()
	handler.ServeHTTP(rec, newRequest(http.MethodPost, "/v1/chat/completions", `{"model":"gpt-test","messages":[{"role":"user","content":"hi"}]}`))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if len(hosts) != 1 || hosts[0] != "primary.test" {
		t.Fatalf("hosts=%v, want only primary.test once", hosts)
	}
}

func TestNetworkErrorDoesNotSwitchProvider(t *testing.T) {
	usageFile := filepath.Join(t.TempDir(), "usage.csv")
	interactionRecorder, err := archive.NewRecorder(filepath.Join(filepath.Dir(usageFile), "interactions"))
	if err != nil {
		t.Fatal(err)
	}
	hosts := []string{}
	cfg := mustHandlerConfig(config.Config{
		ListenAddr:     ":0",
		UsageFile:      usageFile,
		InteractionDir: filepath.Join(filepath.Dir(usageFile), "interactions"),
		Providers: map[string]config.Provider{
			"primary": {Name: "primary", Protocol: "openai", BaseURL: "https://primary.test", APIKey: "k", Models: []string{"gpt-test"}},
			"backup":  {Name: "backup", Protocol: "openai", BaseURL: "https://backup.test", APIKey: "k", Models: []string{"other-*"}},
		},
		ModelCatalog: map[string]config.ModelInfo{
			"gpt-test": {
				ID: "gpt-test", ContextWindowTokens: 128000, MaxOutputTokens: 16384,
				Operations: []string{config.ModelOperationChatCompletions}, RouteOwner: "primary",
			},
		},
	})
	handler := NewHandler(cfg, stats.NewCSVRecorder(usageFile), interactionRecorder, metrics.NewRegistry())
	handler.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		hosts = append(hosts, r.URL.Host)
		return nil, fmt.Errorf("connection refused")
	})
	rec := newResponseRecorder()
	handler.ServeHTTP(rec, newRequest(http.MethodPost, "/v1/chat/completions", `{"model":"gpt-test","messages":[{"role":"user","content":"hi"}]}`))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if len(hosts) != 1 || hosts[0] != "primary.test" {
		t.Fatalf("hosts=%v, want only primary.test once", hosts)
	}
}

func TestFirstStreamEventFailureDoesNotSwitchProvider(t *testing.T) {
	usageFile := filepath.Join(t.TempDir(), "usage.csv")
	interactionRecorder, err := archive.NewRecorder(filepath.Join(filepath.Dir(usageFile), "interactions"))
	if err != nil {
		t.Fatal(err)
	}
	hosts := []string{}
	cfg := mustHandlerConfig(config.Config{
		ListenAddr:     ":0",
		UsageFile:      usageFile,
		InteractionDir: filepath.Join(filepath.Dir(usageFile), "interactions"),
		Providers: map[string]config.Provider{
			"primary": {Name: "primary", Protocol: "openai", BaseURL: "https://primary.test", APIKey: "k", Models: []string{"gpt-test"}},
			"backup":  {Name: "backup", Protocol: "openai", BaseURL: "https://backup.test", APIKey: "k", Models: []string{"other-*"}},
		},
		ModelCatalog: map[string]config.ModelInfo{
			"gpt-test": {
				ID: "gpt-test", ContextWindowTokens: 128000, MaxOutputTokens: 16384,
				Operations: []string{config.ModelOperationChatCompletions}, RouteOwner: "primary",
			},
		},
	})
	handler := NewHandler(cfg, stats.NewCSVRecorder(usageFile), interactionRecorder, metrics.NewRegistry())
	handler.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		hosts = append(hosts, r.URL.Host)
		return testResponse(http.StatusOK, "text/event-stream", "data: incomplete-first-event"), nil
	})

	rec := newResponseRecorder()
	handler.ServeHTTP(rec, newRequest(http.MethodPost, "/v1/chat/completions", `{"model":"gpt-test","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if len(hosts) != 1 || hosts[0] != "primary.test" {
		t.Fatalf("hosts=%v, want only primary.test once", hosts)
	}
}

func TestRequireResolvedConfigMalformedTable(t *testing.T) {
	baseProvider := config.Provider{
		Name: "openai", Protocol: "openai", BaseURL: "https://x", APIKey: "k",
		Models: []string{"gpt-x", "multi"},
		EndpointCapabilities: []string{
			config.EndpointCapabilityChatCompletions,
			config.EndpointCapabilityEmbeddings,
		},
	}
	validCatalog := map[string]config.ModelInfo{
		"gpt-x": {
			ID: "gpt-x", ContextWindowTokens: 1000, MaxOutputTokens: 100,
			Operations: []string{config.ModelOperationChatCompletions}, RouteOwner: "openai",
		},
	}
	cases := []struct {
		name    string
		cfg     config.Config
		wantSub string
	}{
		{
			name: "unknown protocol",
			cfg: config.Config{
				Providers: map[string]config.Provider{
					"weird": {
						Name: "weird", Protocol: "foo", BaseURL: "https://x", APIKey: "k",
						EndpointCapabilities: []string{config.EndpointCapabilityChatCompletions},
					},
				},
			},
			wantSub: "unknown protocol",
		},
		{
			name: "unknown capability",
			cfg: config.Config{
				Providers: map[string]config.Provider{
					"openai": {
						Name: "openai", Protocol: "openai", BaseURL: "https://x", APIKey: "k",
						EndpointCapabilities: []string{"widgets"},
					},
				},
			},
			wantSub: "unknown value",
		},
		{
			name: "duplicate capability",
			cfg: config.Config{
				Providers: map[string]config.Provider{
					"openai": {
						Name: "openai", Protocol: "openai", BaseURL: "https://x", APIKey: "k",
						EndpointCapabilities: []string{
							config.EndpointCapabilityChatCompletions,
							config.EndpointCapabilityChatCompletions,
						},
					},
				},
			},
			wantSub: "duplicate",
		},
		{
			name: "unsorted capability",
			cfg: config.Config{
				Providers: map[string]config.Provider{
					"openai": {
						Name: "openai", Protocol: "openai", BaseURL: "https://x", APIKey: "k",
						EndpointCapabilities: []string{
							config.EndpointCapabilityEmbeddings,
							config.EndpointCapabilityChatCompletions,
						},
					},
				},
			},
			wantSub: "sorted",
		},
		{
			name: "illegal capacity",
			cfg: config.Config{
				Providers: map[string]config.Provider{"openai": baseProvider},
				ModelCatalog: map[string]config.ModelInfo{
					"gpt-x": {
						ID: "gpt-x", ContextWindowTokens: 100, MaxOutputTokens: 100,
						Operations: []string{config.ModelOperationChatCompletions}, RouteOwner: "openai",
					},
				},
			},
			wantSub: "max_output_tokens",
		},
		{
			name: "case-fold duplicate",
			cfg: config.Config{
				Providers: map[string]config.Provider{
					"openai": {
						Name: "openai", Protocol: "openai", BaseURL: "https://x", APIKey: "k",
						// 同时匹配两种原文 id,确保先通过 RouteOwner 匹配再触发 case-fold 校验。
						Models: []string{"GPT-X", "gpt-x"},
						EndpointCapabilities: []string{
							config.EndpointCapabilityChatCompletions,
							config.EndpointCapabilityEmbeddings,
						},
					},
				},
				ModelCatalog: map[string]config.ModelInfo{
					"GPT-X": {
						ID: "GPT-X", ContextWindowTokens: 1000, MaxOutputTokens: 100,
						Operations: []string{config.ModelOperationChatCompletions}, RouteOwner: "openai",
					},
					"gpt-x": {
						ID: "gpt-x", ContextWindowTokens: 1000, MaxOutputTokens: 100,
						Operations: []string{config.ModelOperationChatCompletions}, RouteOwner: "openai",
					},
				},
			},
			wantSub: "case-fold",
		},
		{
			name: "wrong route owner",
			cfg: config.Config{
				Providers: map[string]config.Provider{
					"openai": baseProvider,
					"other": {
						Name: "other", Protocol: "openai", BaseURL: "https://y", APIKey: "k",
						Models:               []string{"nope"},
						EndpointCapabilities: []string{config.EndpointCapabilityChatCompletions},
					},
				},
				ModelCatalog: map[string]config.ModelInfo{
					"gpt-x": {
						ID: "gpt-x", ContextWindowTokens: 1000, MaxOutputTokens: 100,
						Operations: []string{config.ModelOperationChatCompletions}, RouteOwner: "other",
					},
				},
			},
			wantSub: "does not match model",
		},
		{
			name: "operation readiness",
			cfg: config.Config{
				Providers: map[string]config.Provider{
					"openai": {
						Name: "openai", Protocol: "openai", BaseURL: "https://x", APIKey: "k",
						Models:               []string{"gpt-x"},
						EndpointCapabilities: []string{config.EndpointCapabilityChatCompletions},
					},
				},
				ModelCatalog: map[string]config.ModelInfo{
					"gpt-x": {
						ID: "gpt-x", ContextWindowTokens: 1000, MaxOutputTokens: 100,
						Operations: []string{config.ModelOperationEmbeddings}, RouteOwner: "openai",
					},
				},
			},
			wantSub: "not serviceable",
		},
		{
			name: "unsorted operations",
			cfg: config.Config{
				Providers: map[string]config.Provider{"openai": baseProvider},
				ModelCatalog: map[string]config.ModelInfo{
					"multi": {
						ID: "multi", ContextWindowTokens: 1000, MaxOutputTokens: 100,
						Operations: []string{config.ModelOperationEmbeddings, config.ModelOperationChatCompletions},
						RouteOwner: "openai",
					},
				},
			},
			wantSub: "sorted",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := requireResolvedConfig(tc.cfg)
			if err == nil || !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("error = %v, want substring %q", err, tc.wantSub)
			}
		})
	}
	// empty catalog with valid provider is allowed
	if err := requireResolvedConfig(config.Config{
		Providers:    map[string]config.Provider{"openai": baseProvider},
		ModelCatalog: map[string]config.ModelInfo{},
	}); err != nil {
		t.Fatalf("empty catalog: %v", err)
	}
	_ = validCatalog
}

func TestSingleRouteOwnerDoesNotSwitchOnRetryableStatuses(t *testing.T) {
	for _, status := range []int{http.StatusRequestTimeout, http.StatusTooManyRequests, http.StatusBadGateway, http.StatusServiceUnavailable} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			usageFile := filepath.Join(t.TempDir(), "usage.csv")
			interactionRecorder, err := archive.NewRecorder(filepath.Join(filepath.Dir(usageFile), "interactions"))
			if err != nil {
				t.Fatal(err)
			}
			hosts := []string{}
			cfg := mustHandlerConfig(config.Config{
				ListenAddr:     ":0",
				UsageFile:      usageFile,
				InteractionDir: filepath.Join(filepath.Dir(usageFile), "interactions"),
				Providers: map[string]config.Provider{
					"primary": {Name: "primary", Protocol: "openai", BaseURL: "https://primary.test", APIKey: "k", Models: []string{"gpt-test"}},
					"backup":  {Name: "backup", Protocol: "openai", BaseURL: "https://backup.test", APIKey: "k", Models: []string{"other-*"}},
				},
				ModelCatalog: map[string]config.ModelInfo{
					"gpt-test": {
						ID: "gpt-test", ContextWindowTokens: 128000, MaxOutputTokens: 16384,
						Operations: []string{config.ModelOperationChatCompletions}, RouteOwner: "primary",
					},
				},
			})
			handler := NewHandler(cfg, stats.NewCSVRecorder(usageFile), interactionRecorder, metrics.NewRegistry())
			handler.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				hosts = append(hosts, r.URL.Host)
				return testResponse(status, "application/json", `{"error":"retryable"}`), nil
			})
			rec := newResponseRecorder()
			handler.ServeHTTP(rec, newRequest(http.MethodPost, "/v1/chat/completions", `{"model":"gpt-test","messages":[{"role":"user","content":"hi"}]}`))
			if rec.Code != status {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
			if len(hosts) != 1 || hosts[0] != "primary.test" {
				t.Fatalf("hosts=%v", hosts)
			}
		})
	}
}
