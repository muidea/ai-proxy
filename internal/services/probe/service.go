// Package probe 是独立运维进程服务：对已配置 Provider 的 direct endpoint 做最小 live 验证。
// 它不修改 provider/catalog，不由主服务启动链调用，输出保持脱敏。
package probe

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"ai-proxy/internal/pkg/aiproxyconfig"
)

// Main 保持既有 CLI 行为；cmd/ai-proxy-probe 只负责调用该进程服务。
func Main() {
	configPath := flag.String("config", os.Getenv("AI_PROXY_CONFIG"), "config file path")
	providerName := flag.String("provider", "", "RouteOwner provider name")
	capability := flag.String("capability", "", "direct endpoint capability (chat_completions|messages|responses|completions|embeddings)")
	model := flag.String("model", "", "catalog model id (exact)")
	timeout := flag.Duration("timeout", 30*time.Second, "request timeout")
	stream := flag.Bool("stream", false, "also probe streaming when capability supports it")
	flag.Parse()

	if *configPath == "" {
		*configPath = "config.yaml"
	}
	if *providerName == "" || *capability == "" || *model == "" {
		fmt.Fprintln(os.Stderr, "usage: ai-proxy-probe -config config.yaml -provider <route-owner> -capability <cap> -model <model>")
		os.Exit(2)
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "load config: %v\n", err)
		os.Exit(1)
	}
	provider, ok := cfg.Providers[*providerName]
	if !ok || provider.Disabled {
		fmt.Fprintf(os.Stderr, "provider %q missing or disabled\n", *providerName)
		os.Exit(1)
	}
	if !config.ProviderHasDirectEndpoint(provider, *capability) {
		fmt.Fprintf(os.Stderr, "provider %q does not declare direct capability %q\n", *providerName, *capability)
		os.Exit(1)
	}
	info, ok := config.LookupModel(cfg, *model)
	if !ok {
		fmt.Fprintf(os.Stderr, "model %q not in model_catalog\n", *model)
		os.Exit(1)
	}
	if info.RouteOwner != *providerName {
		fmt.Fprintf(os.Stderr, "model %q RouteOwner=%q, not %q\n", *model, info.RouteOwner, *providerName)
		os.Exit(1)
	}

	path, body, err := buildProbeRequest(*capability, *model, false)
	if err != nil {
		fmt.Fprintf(os.Stderr, "build probe: %v\n", err)
		os.Exit(1)
	}
	result := runProbe(http.DefaultClient, *providerName, *capability, *model, provider, path, body, *timeout, false)
	printResult(result)
	if *stream {
		spath, sbody, err := buildProbeRequest(*capability, *model, true)
		if err == nil {
			sres := runProbe(http.DefaultClient, *providerName, *capability, *model, provider, spath, sbody, *timeout, true)
			fmt.Println("--- stream probe ---")
			printResult(sres)
			if !sres.OK && result.OK {
				// stream failure alone still non-zero
				os.Exit(1)
			}
		}
	}
	if !result.OK {
		os.Exit(1)
	}
}

type Result struct {
	OK           bool
	Provider     string
	Protocol     string
	Capability   string
	Model        string
	UpstreamPath string
	Status       int
	DurationMS   int64
	Stream       bool
	Summary      string
	Conclusion   string
}

// Check 对已解析配置执行一次最小、非流式 direct probe。调用方只能使用配置中的
// RouteOwner、已声明 capability 和归属该 Provider 的 catalog model。
func Check(ctx context.Context, cfg config.Config, providerName string) (Result, error) {
	provider, ok := cfg.Providers[providerName]
	if !ok || provider.Disabled {
		return Result{}, fmt.Errorf("provider %q missing or disabled", providerName)
	}
	if len(provider.EndpointCapabilities) == 0 {
		return Result{}, fmt.Errorf("provider %q has no direct capability", providerName)
	}
	models := make([]string, 0)
	for id, info := range cfg.ModelCatalog {
		if info.RouteOwner == providerName {
			models = append(models, id)
		}
	}
	if len(models) == 0 {
		return Result{}, fmt.Errorf("provider %q has no catalog model", providerName)
	}
	sort.Strings(models)
	model := models[0]
	capability := provider.EndpointCapabilities[0]
	path, body, err := buildProbeRequest(capability, model, false)
	if err != nil {
		return Result{}, err
	}
	return runProbeContext(ctx, http.DefaultClient, providerName, capability, model, provider, path, body, 30*time.Second, false), nil
}

func buildProbeRequest(capability, model string, stream bool) (path string, body []byte, err error) {
	switch capability {
	case config.EndpointCapabilityChatCompletions:
		path = "/v1/chat/completions"
		payload := map[string]any{
			"model": model,
			"messages": []map[string]string{
				{"role": "user", "content": "ping"},
			},
			"max_tokens": 8,
			"stream":     stream,
		}
		body, err = json.Marshal(payload)
	case config.EndpointCapabilityMessages:
		path = "/v1/messages"
		payload := map[string]any{
			"model":      model,
			"max_tokens": 8,
			"messages": []map[string]string{
				{"role": "user", "content": "ping"},
			},
			"stream": stream,
		}
		body, err = json.Marshal(payload)
	case config.EndpointCapabilityEmbeddings:
		if stream {
			return "", nil, fmt.Errorf("embeddings does not support stream probe")
		}
		path = "/v1/embeddings"
		payload := map[string]any{
			"model": model,
			"input": "ping",
		}
		body, err = json.Marshal(payload)
	case config.EndpointCapabilityCompletions:
		path = "/v1/completions"
		payload := map[string]any{
			"model":      model,
			"prompt":     "ping",
			"max_tokens": 8,
			"stream":     stream,
		}
		body, err = json.Marshal(payload)
	case config.EndpointCapabilityResponses:
		path = "/v1/responses"
		payload := map[string]any{
			"model":  model,
			"input":  "ping",
			"stream": stream,
		}
		body, err = json.Marshal(payload)
	default:
		err = fmt.Errorf("unknown capability %q", capability)
	}
	return
}

func runProbe(client *http.Client, providerName, capability, model string, provider config.Provider, path string, body []byte, timeout time.Duration, stream bool) Result {
	return runProbeContext(context.Background(), client, providerName, capability, model, provider, path, body, timeout, stream)
}

func runProbeContext(parent context.Context, client *http.Client, providerName, capability, model string, provider config.Provider, path string, body []byte, timeout time.Duration, stream bool) Result {
	res := Result{
		Provider:     providerName,
		Protocol:     provider.Protocol,
		Capability:   capability,
		UpstreamPath: path,
		Stream:       stream,
		Model:        model,
	}
	// reconstruct full URL
	base := strings.TrimRight(provider.BaseURL, "/")
	url := joinURL(base, path)
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		res.Summary = "build request failed"
		res.Conclusion = "error"
		return res
	}
	req.Header.Set("Content-Type", "application/json")
	if stream {
		req.Header.Set("Accept", "text/event-stream")
	} else {
		req.Header.Set("Accept", "application/json")
	}
	if provider.Protocol == "anthropic" {
		req.Header.Set("Anthropic-Version", "2023-06-01")
		if provider.APIKey != "" {
			req.Header.Set("X-API-Key", provider.APIKey)
		}
	} else if provider.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+provider.APIKey)
	}

	start := time.Now()
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	res.DurationMS = time.Since(start).Milliseconds()
	if err != nil {
		res.Summary = sanitizeSummary(err.Error())
		res.Conclusion = "environment_undetermined"
		return res
	}
	defer resp.Body.Close()
	res.Status = resp.StatusCode
	limited := io.LimitReader(resp.Body, 4096)
	raw, _ := io.ReadAll(limited)
	res.Summary = sanitizeSummary(string(raw))
	if isCapabilityDriftResponse(resp.StatusCode, res.Summary) {
		res.Conclusion = "capability_drift"
		return res
	}
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		res.OK = true
		res.Conclusion = "success"
	case resp.StatusCode == 401 || resp.StatusCode == 403:
		res.Conclusion = "credential_issue"
	case resp.StatusCode == 404 || resp.StatusCode == 405:
		res.Conclusion = "capability_drift"
	case resp.StatusCode == 408 || resp.StatusCode == 429 || resp.StatusCode >= 500:
		res.Conclusion = "environment_undetermined"
	default:
		res.Conclusion = "error"
	}
	return res
}

// isCapabilityDriftResponse 识别上游明确说明端点/relay 未实现或当前 base URL 不支持的响应。
// 5xx 本身仍归 environment_undetermined，只有有明确语义的 body 才标记为 drift。
func isCapabilityDriftResponse(status int, summary string) bool {
	if status < http.StatusBadRequest {
		return false
	}
	if status == http.StatusNotFound || status == http.StatusMethodNotAllowed {
		return true
	}
	lower := strings.ToLower(summary)
	for _, marker := range []string{
		"not support", "not supported", "unsupported", "not implemented", "unknown endpoint",
		"unknown api", "only available when using beta", "convert_request_failed",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func joinURL(base, path string) string {
	base = strings.TrimRight(base, "/")
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	if strings.HasSuffix(base, "/v1") && strings.HasPrefix(path, "/v1/") {
		return base + strings.TrimPrefix(path, "/v1")
	}
	return base + path
}

func sanitizeSummary(s string) string {
	s = strings.TrimSpace(s)
	// redact obvious secrets
	lower := strings.ToLower(s)
	if strings.Contains(lower, "bearer ") || strings.Contains(s, "sk-") ||
		strings.Contains(lower, "api_key") || strings.Contains(lower, "api key") ||
		strings.Contains(lower, "x-api-key") || strings.Contains(lower, "authorization") {
		return "upstream response (details redacted)"
	}
	if len(s) > 240 {
		s = s[:240] + "..."
	}
	// collapse whitespace
	s = strings.ReplaceAll(s, "\n", " ")
	return s
}

func printResult(r Result) {
	printResultTo(os.Stdout, r)
}

func printResultTo(w io.Writer, r Result) {
	fmt.Fprintf(w, "provider=%s protocol=%s capability=%s model=%s path=%s stream=%t status=%d duration_ms=%d conclusion=%s\n",
		r.Provider, r.Protocol, r.Capability, r.Model, r.UpstreamPath, r.Stream, r.Status, r.DurationMS, r.Conclusion)
	if r.Summary != "" {
		fmt.Fprintf(w, "summary=%s\n", r.Summary)
	}
}
