package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"ai-proxy/internal/config"
)

func TestBuildProbeRequest(t *testing.T) {
	tests := []struct {
		capability string
		path       string
		stream     bool
	}{
		{config.EndpointCapabilityChatCompletions, "/v1/chat/completions", true},
		{config.EndpointCapabilityMessages, "/v1/messages", true},
		{config.EndpointCapabilityResponses, "/v1/responses", true},
		{config.EndpointCapabilityCompletions, "/v1/completions", true},
		{config.EndpointCapabilityEmbeddings, "/v1/embeddings", false},
	}
	for _, tt := range tests {
		t.Run(tt.capability, func(t *testing.T) {
			path, body, err := buildProbeRequest(tt.capability, "DeepSeek-V4-Flash", tt.stream)
			if err != nil {
				t.Fatal(err)
			}
			if path != tt.path {
				t.Fatalf("path = %q, want %q", path, tt.path)
			}
			var payload map[string]any
			if err := json.Unmarshal(body, &payload); err != nil {
				t.Fatal(err)
			}
			if payload["model"] != "DeepSeek-V4-Flash" {
				t.Fatalf("model = %#v", payload["model"])
			}
		})
	}
	if _, _, err := buildProbeRequest(config.EndpointCapabilityEmbeddings, "m", true); err == nil {
		t.Fatal("expected embeddings stream probe rejection")
	}
	if _, _, err := buildProbeRequest("unknown", "m", false); err == nil {
		t.Fatal("expected unknown capability rejection")
	}
}

func TestJoinURL(t *testing.T) {
	tests := map[string]string{
		"https://api.example.test|/v1/chat/completions":         "https://api.example.test/v1/chat/completions",
		"https://api.example.test/v1/|/v1/chat/completions":     "https://api.example.test/v1/chat/completions",
		"https://api.example.test/codex/v1|/v1/responses":       "https://api.example.test/codex/v1/responses",
		"https://api.example.test/codex/v1|v1/chat/completions": "https://api.example.test/codex/v1/chat/completions",
	}
	for input, want := range tests {
		parts := strings.SplitN(input, "|", 2)
		if got := joinURL(parts[0], parts[1]); got != want {
			t.Fatalf("joinURL(%q, %q) = %q, want %q", parts[0], parts[1], got, want)
		}
	}
}

func TestSanitizeSummary(t *testing.T) {
	for _, secret := range []string{
		`{"error":"Authorization: Bearer secret"}`,
		`{"api_key":"secret"}`,
		`{"message":"x-api-key invalid"}`,
		`token sk-secret`,
	} {
		if got := sanitizeSummary(secret); got != "upstream response (details redacted)" {
			t.Fatalf("secret was not redacted: %q", got)
		}
	}
	if got := sanitizeSummary("line one\nline two"); got != "line one line two" {
		t.Fatalf("summary = %q", got)
	}
}

func TestIsCapabilityDriftResponse(t *testing.T) {
	for _, tt := range []struct {
		status  int
		summary string
		want    bool
	}{
		{http.StatusNotFound, "", true},
		{http.StatusInternalServerError, `{"error":{"message":"not implemented"}}`, true},
		{http.StatusBadRequest, "completions api is only available when using beta", true},
		{520, "cloudflare origin error", false},
		{http.StatusInternalServerError, "temporary upstream failure", false},
	} {
		if got := isCapabilityDriftResponse(tt.status, tt.summary); got != tt.want {
			t.Fatalf("isCapabilityDriftResponse(%d, %q) = %t, want %t", tt.status, tt.summary, got, tt.want)
		}
	}
}

func TestRunProbePopulatesContractFieldsAndOutput(t *testing.T) {
	client := &http.Client{Transport: probeRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Fatalf("path = %q", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer upstream-key" {
			t.Fatalf("authorization = %q", got)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"id":"chatcmpl-probe"}`)),
		}, nil
	})}

	provider := config.Provider{
		Name: "display-name", Protocol: "openai", BaseURL: "https://upstream.test", APIKey: "upstream-key",
	}
	result := runProbe(client, "route-owner", config.EndpointCapabilityChatCompletions,
		"DeepSeek-V4-Flash", provider, "/v1/chat/completions", []byte(`{"model":"DeepSeek-V4-Flash"}`), time.Second, false)
	if !result.OK || result.Provider != "route-owner" || result.Protocol != "openai" ||
		result.Capability != config.EndpointCapabilityChatCompletions || result.Model != "DeepSeek-V4-Flash" ||
		result.UpstreamPath != "/v1/chat/completions" || result.Status != http.StatusOK || result.Conclusion != "success" {
		t.Fatalf("result = %#v", result)
	}

	var out bytes.Buffer
	printResultTo(&out, result)
	for _, field := range []string{
		"provider=route-owner", "protocol=openai", "capability=chat_completions",
		"model=DeepSeek-V4-Flash", "path=/v1/chat/completions", "status=200", "conclusion=success",
	} {
		if !strings.Contains(out.String(), field) {
			t.Fatalf("output missing %q: %s", field, out.String())
		}
	}
}

type probeRoundTripFunc func(*http.Request) (*http.Response, error)

func (f probeRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}
