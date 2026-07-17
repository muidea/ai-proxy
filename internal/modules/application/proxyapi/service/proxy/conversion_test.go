package proxy

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestConvertAnthropicResponseRejectsToolUse(t *testing.T) {
	body := []byte(`{"id":"1","model":"claude","content":[{"type":"tool_use","text":""}],"stop_reason":"tool_use","usage":{"input_tokens":1,"output_tokens":1}}`)
	_, _, err := convertAnthropicResponse(body, "claude")
	if err == nil {
		t.Fatal("expected tool_use rejection")
	}
	if !strings.Contains(err.Error(), "tool_use") {
		t.Fatalf("error = %q", err)
	}
}

func TestConvertOpenAIResponseRejectsToolCalls(t *testing.T) {
	body := []byte(`{"id":"1","model":"gpt","choices":[{"message":{"role":"assistant","content":null,"tool_calls":[{"id":"c"}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	_, _, err := convertOpenAIChatToAnthropicResponse(body, "gpt")
	if err == nil {
		t.Fatal("expected tool_calls rejection")
	}
	if !strings.Contains(err.Error(), "tool_calls") {
		t.Fatalf("error = %q", err)
	}
}

func TestOutcomeFromStreamFail(t *testing.T) {
	if got := outcomeFromStreamFail(nil, 200); got != "success" {
		t.Fatalf("nil/200 = %q", got)
	}
	if got := outcomeFromStreamFail(nil, 502); got != "error" {
		t.Fatalf("nil/502 = %q", got)
	}
	f := newStreamFail(streamKindUpstreamTrunc, "read", fmt.Errorf("eof"), true)
	if got := outcomeFromStreamFail(f, 200); got != "upstream_truncated" {
		t.Fatalf("trunc = %q", got)
	}
	if shouldCountUpstreamError(f, true) != true {
		t.Fatal("should count upstream for midflight")
	}
	if shouldCountUpstreamError(f, false) != false {
		t.Fatal("should not count for non-stream")
	}
	local := newStreamFail(streamKindLimitExceeded, "limit", fmt.Errorf("too big"), false)
	if shouldCountUpstreamError(local, true) {
		t.Fatal("local limit should not count as upstream error")
	}
}

func TestAnthropicStreamEventsRejectsToolUse(t *testing.T) {
	var id, model = "x", "m"
	var usage tokenUsage
	var content strings.Builder
	var finish string
	roleSent := false
	payload := `{"type":"content_block_start","content_block":{"type":"tool_use","id":"t1","name":"fn"}}`
	_, err := anthropicStreamEvents(payload, &id, &model, &usage, &content, &finish, &roleSent, 0)
	if err == nil {
		t.Fatal("expected tool_use rejection")
	}
}

func TestIsTerminalOpenAILine(t *testing.T) {
	if !isTerminalOpenAILine([]byte("data: [DONE]\n")) {
		t.Fatal("expected [DONE] terminal")
	}
	if isTerminalOpenAILine([]byte(`data: {"choices":[]}`)) {
		t.Fatal("non-done should not be terminal")
	}
}

func TestIsTerminalAnthropicPayload(t *testing.T) {
	if !isTerminalAnthropicPayload(`{"type":"message_stop"}`) {
		t.Fatal("expected message_stop")
	}
	if isTerminalAnthropicPayload(`{"type":"message_delta"}`) {
		t.Fatal("message_delta is not terminal")
	}
}

func TestIsTerminalResponsesPayload(t *testing.T) {
	if !isTerminalResponsesPayload(`{"type":"response.completed"}`) {
		t.Fatal("expected response.completed")
	}
	if !isTerminalResponsesPayload(`{"type": "response.failed"}`) {
		t.Fatal("expected response.failed with space")
	}
	if isTerminalResponsesPayload(`{"type":"response.output_text.delta"}`) {
		t.Fatal("delta is not terminal")
	}
}

func TestParseResponsesTerminalOutcomes(t *testing.T) {
	term := parseResponsesTerminal(`{"type":"response.completed"}`)
	if !term.Terminal || term.Kind != streamKindSuccess {
		t.Fatalf("completed = %#v", term)
	}
	term = parseResponsesTerminal(`{"type":"response.failed","response":{"error":{"message":"boom"}}}`)
	if !term.Terminal || term.Kind != streamKindUpstreamFailed || term.Detail != "boom" {
		t.Fatalf("failed = %#v", term)
	}
	fail := streamFailFromTerminal(term)
	if fail == nil || fail.Kind != streamKindUpstreamFailed || !fail.CountUpstream {
		t.Fatalf("fail from terminal = %#v", fail)
	}
	term = parseResponsesTerminal(`{"type":"response.incomplete","response":{"incomplete_details":{"reason":"max_output_tokens"}}}`)
	if !term.Terminal || term.Kind != streamKindIncomplete || term.Detail != "max_output_tokens" {
		t.Fatalf("incomplete = %#v", term)
	}
	fail = streamFailFromTerminal(term)
	if fail == nil || fail.CountUpstream {
		t.Fatalf("incomplete should not count upstream: %#v", fail)
	}
}

func TestIsTerminalAnthropicPayloadWhitespace(t *testing.T) {
	if !isTerminalAnthropicPayload(`{"type" : "message_stop"}`) {
		t.Fatal("expected message_stop with spaces around colon")
	}
}

func TestStreamProtocolForPath(t *testing.T) {
	if streamProtocolForPath("/v1/responses") != streamProtoResponses {
		t.Fatal("responses")
	}
	if streamProtocolForPath("/v1/chat/completions") != streamProtoChatCompletions {
		t.Fatal("chat")
	}
	if requiresTerminalEvent(streamProtoPassthrough) {
		t.Fatal("passthrough should not require terminal")
	}
}

func TestLogStreamFailPreservesProtocolKind(t *testing.T) {
	// logStreamFail 不应把已构造的 protocol 改写成 conversion。
	fail := newStreamFail(streamKindProtocol, "convert anthropic stream: invalid SSE JSON", fmt.Errorf("invalid SSE JSON"), true)
	if fail.Kind != streamKindProtocol {
		t.Fatalf("kind = %s", fail.Kind)
	}
	if !fail.CountUpstream {
		t.Fatal("protocol should count upstream")
	}
	// 模拟产生点赋值后 outcome 仍为 protocol
	if outcomeFromStreamFail(fail, 200) != "protocol" {
		t.Fatalf("outcome = %s", outcomeFromStreamFail(fail, 200))
	}
}

func TestConvertOpenAIResponseAllowsEmptyToolCalls(t *testing.T) {
	body := []byte(`{"id":"1","model":"gpt","choices":[{"message":{"role":"assistant","content":"pong","tool_calls":[],"function_call":null},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	out, _, err := convertOpenAIChatToAnthropicResponse(body, "gpt")
	if err != nil {
		t.Fatalf("empty tool_calls should be allowed: %v", err)
	}
	if !strings.Contains(string(out), "pong") {
		t.Fatalf("response = %s", out)
	}
}

func TestHasNonEmptyConversionFeature(t *testing.T) {
	if hasNonEmptyConversionFeature(nil) {
		t.Fatal("nil")
	}
	if hasNonEmptyConversionFeature([]any{}) {
		t.Fatal("empty array")
	}
	if hasNonEmptyConversionFeature(map[string]any{}) {
		t.Fatal("empty object")
	}
	if !hasNonEmptyConversionFeature([]any{map[string]any{"id": "x"}}) {
		t.Fatal("non-empty array")
	}
}

func TestNormalizeStopSequences(t *testing.T) {
	if got, err := normalizeStopSequences("END"); err != nil || !reflect.DeepEqual(got, []string{"END"}) {
		t.Fatalf("string stop = %#v err=%v", got, err)
	}
	if got, err := normalizeStopSequences([]any{"a", " b ", ""}); err != nil || !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Fatalf("array stop = %#v err=%v", got, err)
	}
	if got, err := normalizeStopSequences(""); err != nil || got != nil {
		t.Fatalf("empty string = %#v err=%v", got, err)
	}
	if got, err := normalizeStopSequences(nil); err != nil || got != nil {
		t.Fatalf("nil = %#v err=%v", got, err)
	}
	if _, err := normalizeStopSequences([]any{"END", 1}); err == nil {
		t.Fatal("expected mixed stop array to be rejected")
	}
	if _, err := normalizeStopSequences(1); err == nil {
		t.Fatal("expected numeric stop to be rejected")
	}
	if _, err := normalizeStopSequences(map[string]any{"value": "END"}); err == nil {
		t.Fatal("expected object stop to be rejected")
	}
}

func TestConversionStreamTextDeltaRejectsNonText(t *testing.T) {
	if _, err := conversionStreamTextDelta([]any{map[string]any{"type": "image_url"}}); err == nil {
		t.Fatal("expected rejection for non-text stream content")
	}
	got, err := conversionStreamTextDelta("hello")
	if err != nil || got != "hello" {
		t.Fatalf("string delta = %q err=%v", got, err)
	}
	got, err = conversionStreamTextDelta(nil)
	if err != nil || got != "" {
		t.Fatalf("nil delta = %q err=%v", got, err)
	}
}

func TestStreamFailFromMessageConversionOutcome(t *testing.T) {
	fail := streamFailFromMessage("protocol conversion does not support response tool_calls; use a native provider")
	if fail == nil || fail.Kind != streamKindConversion {
		t.Fatalf("kind = %#v", fail)
	}
	if outcomeFromStreamFail(fail, 502) != "conversion" {
		t.Fatalf("outcome = %q", outcomeFromStreamFail(fail, 502))
	}
}
