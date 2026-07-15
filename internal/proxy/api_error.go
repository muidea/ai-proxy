package proxy

import (
	"encoding/json"
	"net/http"
)

// 稳定错误码(面向 WorkOrch / 客户端合同)。
const (
	ErrorCodeModelRequired        = "model_required"
	ErrorCodeModelNotFound        = "model_not_found"
	ErrorCodeOperationUnsupported = "operation_unsupported"
	ErrorCodeRouteContractInvalid = "route_contract_invalid"
	ErrorCodeProviderUnavailable  = "provider_unavailable"
	ErrorCodeMultipleProviders    = "multiple_providers"
	ErrorCodeInvalidRequest       = "invalid_request"
	ErrorCodeEndpointUnsupported  = "endpoint_unsupported"
)

// APIErrorResponse 是 LLM 请求失败时的具体错误体,避免自由文本。
type APIErrorResponse struct {
	Error APIError `json:"error"`
}

// APIError 描述稳定错误合同;不得包含 API Key、Authorization 或上游敏感体。
type APIError struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Model     string `json:"model,omitempty"`
	Operation string `json:"operation,omitempty"`
}

func writeAPIError(w http.ResponseWriter, status int, code, message, model, operation string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(APIErrorResponse{
		Error: APIError{
			Code:      code,
			Message:   message,
			Model:     model,
			Operation: operation,
		},
	})
}
