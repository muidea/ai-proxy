package proxy

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// streamKind 描述流式/请求结束后的业务结果，用于 metrics outcome。
// 完整枚举: success | client_canceled | idle_timeout | limit_exceeded |
// upstream_truncated | upstream_failed | incomplete | client_write | conversion | protocol | error
type streamKind string

const (
	streamKindSuccess        streamKind = "success"
	streamKindClientCanceled streamKind = "client_canceled"
	streamKindIdleTimeout    streamKind = "idle_timeout"
	streamKindLimitExceeded  streamKind = "limit_exceeded"
	streamKindUpstreamTrunc  streamKind = "upstream_truncated"
	streamKindUpstreamFailed streamKind = "upstream_failed" // 上游显式失败(如 response.failed)
	streamKindIncomplete     streamKind = "incomplete"      // 上游未完成(如 response.incomplete)
	streamKindClientWrite    streamKind = "client_write"
	streamKindConversion     streamKind = "conversion"
	streamKindProtocol       streamKind = "protocol"
	streamKindError          streamKind = "error"
)

// streamProtocol 选择流式终止事件语义。
type streamProtocol string

const (
	streamProtoChatCompletions streamProtocol = "chat_completions" // [DONE]
	streamProtoResponses       streamProtocol = "responses"        // response.completed / failed / incomplete
	streamProtoAnthropic       streamProtocol = "anthropic"        // message_stop
	streamProtoPassthrough     streamProtocol = "passthrough"      // 未知协议:不强制终止事件
)

// terminalResult 描述一次 SSE 行是否构成协议终止,以及对应业务结果。
type terminalResult struct {
	Terminal bool
	Kind     streamKind // success / upstream_failed / incomplete / ...
	Detail   string     // 可选原因(如 incomplete reason)
}

// streamFail 是带类型的流式/处理错误，避免依赖错误字符串做 outcome 分类。
type streamFail struct {
	Kind          streamKind
	Message       string // 完整可读消息，写入 metadata / 日志
	Err           error
	CountUpstream bool // 是否计入 provider upstream error rate
}

func (e *streamFail) Error() string {
	if e == nil {
		return ""
	}
	if e.Message != "" {
		return e.Message
	}
	if e.Err != nil {
		return e.Err.Error()
	}
	return string(e.Kind)
}

func (e *streamFail) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

func newStreamFail(kind streamKind, message string, err error, countUpstream bool) *streamFail {
	if message == "" && err != nil {
		message = err.Error()
	}
	return &streamFail{Kind: kind, Message: message, Err: err, CountUpstream: countUpstream}
}

func streamFailFromMessage(msg string) *streamFail {
	if msg == "" {
		return nil
	}
	kind := streamKindError
	lower := strings.ToLower(msg)
	switch {
	case strings.Contains(lower, "conversion") || strings.Contains(lower, "protocol conversion") || strings.Contains(lower, "conversion_unsupported"):
		kind = streamKindConversion
	case strings.Contains(lower, "invalid sse") || strings.Contains(lower, "protocol "):
		kind = streamKindProtocol
	}
	return &streamFail{Kind: kind, Message: msg, Err: errors.New(msg)}
}

func streamFailFromTerminal(term terminalResult) *streamFail {
	if !term.Terminal {
		return nil
	}
	switch term.Kind {
	case streamKindSuccess, "":
		return nil
	case streamKindUpstreamFailed:
		msg := "upstream response.failed"
		if term.Detail != "" {
			msg = msg + ": " + term.Detail
		}
		return newStreamFail(streamKindUpstreamFailed, msg, fmt.Errorf("%s", msg), true)
	case streamKindIncomplete:
		msg := "upstream response.incomplete"
		if term.Detail != "" {
			msg = msg + ": " + term.Detail
		}
		return newStreamFail(streamKindIncomplete, msg, fmt.Errorf("%s", msg), false)
	default:
		if term.Kind != streamKindSuccess {
			return newStreamFail(term.Kind, string(term.Kind), nil, false)
		}
		return nil
	}
}

func outcomeFromStreamFail(f *streamFail, httpStatus int) string {
	if f == nil {
		if httpStatus >= 400 {
			return string(streamKindError)
		}
		return string(streamKindSuccess)
	}
	if f.Kind != "" {
		return string(f.Kind)
	}
	return string(streamKindError)
}

// shouldCountUpstreamError 仅 midflight 上游读失败 / 上游显式失败计入 provider 错误率。
func shouldCountUpstreamError(f *streamFail, stream bool) bool {
	if f == nil || !stream {
		return false
	}
	return f.CountUpstream
}

// streamProtocolForPath 按入站 path 选择终止事件语义。
func streamProtocolForPath(path string) streamProtocol {
	switch path {
	case "/v1/chat/completions", "/v1/completions":
		return streamProtoChatCompletions
	case "/v1/responses":
		return streamProtoResponses
	case "/v1/messages":
		return streamProtoAnthropic
	default:
		return streamProtoPassthrough
	}
}

// parseTerminalSSELine 解析一行 SSE 是否为协议终止事件,并给出结果类型。
func parseTerminalSSELine(proto streamProtocol, line []byte) terminalResult {
	trimmed := strings.TrimSpace(string(line))
	if !strings.HasPrefix(trimmed, "data:") {
		return terminalResult{}
	}
	payload := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
	if payload == "" {
		return terminalResult{}
	}
	switch proto {
	case streamProtoChatCompletions:
		if payload == "[DONE]" {
			return terminalResult{Terminal: true, Kind: streamKindSuccess}
		}
		return terminalResult{}
	case streamProtoResponses:
		return parseResponsesTerminal(payload)
	case streamProtoAnthropic:
		if isTerminalAnthropicPayload(payload) {
			return terminalResult{Terminal: true, Kind: streamKindSuccess}
		}
		return terminalResult{}
	default:
		return terminalResult{}
	}
}

// isTerminalSSELine 兼容旧 bool API。
func isTerminalSSELine(proto streamProtocol, line []byte) bool {
	return parseTerminalSSELine(proto, line).Terminal
}

func requiresTerminalEvent(proto streamProtocol) bool {
	switch proto {
	case streamProtoChatCompletions, streamProtoResponses, streamProtoAnthropic:
		return true
	default:
		return false
	}
}

func parseResponsesTerminal(payload string) terminalResult {
	var event struct {
		Type     string `json:"type"`
		Response struct {
			Status            string `json:"status"`
			IncompleteDetails *struct {
				Reason string `json:"reason"`
			} `json:"incomplete_details"`
			Error *struct {
				Message string `json:"message"`
				Code    string `json:"code"`
			} `json:"error"`
		} `json:"response"`
	}
	if err := json.Unmarshal([]byte(payload), &event); err != nil {
		return terminalResult{}
	}
	switch event.Type {
	case "response.completed":
		return terminalResult{Terminal: true, Kind: streamKindSuccess}
	case "response.failed":
		detail := ""
		if event.Response.Error != nil {
			detail = event.Response.Error.Message
			if detail == "" {
				detail = event.Response.Error.Code
			}
		}
		return terminalResult{Terminal: true, Kind: streamKindUpstreamFailed, Detail: detail}
	case "response.incomplete":
		detail := ""
		if event.Response.IncompleteDetails != nil {
			detail = event.Response.IncompleteDetails.Reason
		}
		return terminalResult{Terminal: true, Kind: streamKindIncomplete, Detail: detail}
	default:
		return terminalResult{}
	}
}

// isTerminalAnthropicPayload 解析 JSON 顶层 type,兼容空白变体。
func isTerminalAnthropicPayload(payload string) bool {
	var event struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal([]byte(payload), &event); err != nil {
		return false
	}
	return event.Type == "message_stop"
}

// 兼容旧 helper。
func isTerminalOpenAILine(line []byte) bool {
	return isTerminalSSELine(streamProtoChatCompletions, line)
}

func isTerminalResponsesPayload(payload string) bool {
	return parseResponsesTerminal(payload).Terminal
}
