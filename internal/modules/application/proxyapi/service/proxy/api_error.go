package proxy

import (
	"encoding/json"
	"net/http"
	"strings"
)

// 稳定错误码(面向 WorkOrch / 客户端合同)。
const (
	ErrorCodeModelRequired         = "model_required"
	ErrorCodeModelNotFound         = "model_not_found"
	ErrorCodeOperationUnsupported  = "operation_unsupported"
	ErrorCodeRouteContractInvalid  = "route_contract_invalid"
	ErrorCodeProviderUnavailable   = "provider_unavailable"
	ErrorCodeMultipleProviders     = "multiple_providers"
	ErrorCodeInvalidRequest        = "invalid_request"
	ErrorCodeEndpointUnsupported   = "endpoint_unsupported"
	ErrorCodeConversionUnsupported = "conversion_unsupported"
	ErrorCodeAuthenticationFailed  = "authentication_failed"
	ErrorCodeRequestTooLarge       = "request_too_large"
	ErrorCodeProxyInternalError    = "proxy_internal_error"
	ErrorCodeUpstreamUnavailable   = "upstream_unavailable"
	ErrorCodeUsageStoreUnavailable = "usage_store_unavailable"
)

// APIErrorResponse 是 OpenAI-compatible 错误 envelope。
type APIErrorResponse struct {
	Error APIError `json:"error"`
}

// AnthropicErrorResponse 是 Anthropic-compatible 错误 envelope。
type AnthropicErrorResponse struct {
	Type  string         `json:"type"`
	Error AnthropicError `json:"error"`
}

type AnthropicError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

// APIError 描述稳定错误合同;不得包含 API Key、Authorization 或上游敏感体。
// 可选上下文字段用于客户端与 WorkOrch 诊断,均不泄露 secret。
type APIError struct {
	Code             string `json:"code"`
	Message          string `json:"message"`
	Type             string `json:"type,omitempty"` // OpenAI error.type
	Model            string `json:"model,omitempty"`
	Operation        string `json:"operation,omitempty"`
	ClientEndpoint   string `json:"client_endpoint,omitempty"`
	ClientProtocol   string `json:"client_protocol,omitempty"`
	UpstreamProtocol string `json:"upstream_protocol,omitempty"`
	Feature          string `json:"feature,omitempty"`
}

func writeAPIError(w http.ResponseWriter, status int, apiErr APIError) {
	writeClientProtocolError(w, status, ClientProtocolOpenAI, apiErr)
}

// writeClientProtocolError 按客户端协议输出 SDK 可解析 envelope。
// OpenAI: {"error":{code,message,type,...}}
// Anthropic: {"type":"error","error":{"type":"...","message":"..."}}
func writeClientProtocolError(w http.ResponseWriter, status int, clientProtocol string, apiErr APIError) {
	if apiErr.Type == "" {
		apiErr.Type = openAIErrorType(apiErr.Code)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if strings.EqualFold(clientProtocol, ClientProtocolAnthropic) {
		msg := apiErr.Message
		if apiErr.Code != "" && !strings.Contains(msg, apiErr.Code) {
			msg = apiErr.Code + ": " + msg
		}
		_ = json.NewEncoder(w).Encode(AnthropicErrorResponse{
			Type: "error",
			Error: AnthropicError{
				Type:    anthropicErrorType(apiErr.Code),
				Message: msg,
			},
		})
		return
	}
	_ = json.NewEncoder(w).Encode(APIErrorResponse{Error: apiErr})
}

func openAIErrorType(code string) string {
	switch code {
	case ErrorCodeRequestTooLarge:
		return "invalid_request_error"
	case ErrorCodeProxyInternalError, ErrorCodeUpstreamUnavailable, ErrorCodeProviderUnavailable, ErrorCodeRouteContractInvalid:
		return "api_error"
	default:
		return "invalid_request_error"
	}
}

func anthropicErrorType(code string) string {
	switch code {
	case ErrorCodeRequestTooLarge, ErrorCodeInvalidRequest, ErrorCodeModelRequired, ErrorCodeModelNotFound,
		ErrorCodeOperationUnsupported, ErrorCodeEndpointUnsupported, ErrorCodeConversionUnsupported,
		ErrorCodeAuthenticationFailed:
		return "invalid_request_error"
	case ErrorCodeProviderUnavailable, ErrorCodeUpstreamUnavailable:
		return "overloaded_error"
	default:
		return "api_error"
	}
}

func clientProtocolFromRequest(r *http.Request) string {
	if r == nil || r.URL == nil {
		return ClientProtocolOpenAI
	}
	if p := ClientProtocolForPath(r.URL.Path); p != "" {
		return p
	}
	return ClientProtocolOpenAI
}

func statusForAPIError(apiErr *APIError) int {
	if apiErr == nil {
		return http.StatusBadRequest
	}
	switch apiErr.Code {
	case ErrorCodeProviderUnavailable, ErrorCodeUpstreamUnavailable:
		return http.StatusServiceUnavailable
	case ErrorCodeRouteContractInvalid, ErrorCodeProxyInternalError:
		return http.StatusInternalServerError
	case ErrorCodeRequestTooLarge:
		return http.StatusRequestEntityTooLarge
	default:
		return http.StatusBadRequest
	}
}

// writeAPIErrorFields 兼容旧调用点的位置参数写法。
func writeAPIErrorFields(w http.ResponseWriter, status int, code, message, model, operation string) {
	writeAPIError(w, status, APIError{
		Code:      code,
		Message:   message,
		Model:     model,
		Operation: operation,
	})
}
