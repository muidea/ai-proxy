package proxy

import (
	"fmt"
	"net/http"
	"strings"

	"ai-proxy/internal/config"
)

// 客户端协议与 TransportPlan 模式常量。
const (
	ClientProtocolOpenAI    = "openai"
	ClientProtocolAnthropic = "anthropic"

	TransportModeNative            = "native"
	TransportModeOpenAIToAnthropic = "openai_to_anthropic"
	TransportModeAnthropicToOpenAI = "anthropic_to_openai"
)

// TransportPlan 是请求期唯一转发计划:固定入站协议/path、上游协议/path 与转换方式。
// 只在 ResolvedModelRoute 之上解析,不允许修改 RouteOwner。
type TransportPlan struct {
	ModelID          string
	Operation        string
	ClientProtocol   string
	ClientEndpoint   string
	RouteOwner       string
	UpstreamProtocol string
	UpstreamEndpoint string
	Mode             string // native | openai_to_anthropic | anthropic_to_openai
}

// IsConversion 表示需要协议转换(非 native 直通)。
func (p TransportPlan) IsConversion() bool {
	return p.Mode == TransportModeOpenAIToAnthropic || p.Mode == TransportModeAnthropicToOpenAI
}

// RouteLabel 把入站 HTTP 路径归一化为 Prometheus 标签使用的稳定 route 名。
// 已知路径直接映射,未知路径收敛到 "other",避免基数爆炸。
func RouteLabel(r *http.Request) string {
	if r == nil {
		return "unknown"
	}
	path := strings.TrimRight(r.URL.Path, "/")
	switch path {
	case "/v1/chat/completions":
		return "chat_completions"
	case "/v1/messages":
		return "messages"
	case "/v1/responses":
		return "responses"
	case "/v1/completions":
		return "completions"
	case "/v1/embeddings":
		return "embeddings"
	case "/v1/models":
		return "models"
	case "/healthz":
		return "healthz"
	}
	if strings.HasPrefix(path, "/v1/") {
		return "v1_other"
	}
	return "other"
}

// OperationForPath 将入站 path 映射为 model_catalog.operations 合同枚举。
// /v1/models 返回空字符串(不适用)。
func OperationForPath(path string) string {
	path = strings.TrimRight(strings.TrimSpace(path), "/")
	switch path {
	case "/v1/chat/completions", "/v1/messages", "/v1/responses", "/v1/completions":
		return config.ModelOperationChatCompletions
	case "/v1/embeddings":
		return config.ModelOperationEmbeddings
	default:
		return ""
	}
}

// ClientProtocolForPath 由 method+path 决定客户端协议,不从 User-Agent/SDK/body 推断。
func ClientProtocolForPath(path string) string {
	path = strings.TrimRight(strings.TrimSpace(path), "/")
	switch path {
	case "/v1/messages":
		return ClientProtocolAnthropic
	case "/v1/chat/completions", "/v1/responses", "/v1/completions", "/v1/embeddings", "/v1/models":
		return ClientProtocolOpenAI
	default:
		return ""
	}
}

// NormalizeClientEndpoint 归一化入站 endpoint path。
func NormalizeClientEndpoint(path string) string {
	return strings.TrimRight(strings.TrimSpace(path), "/")
}

// ProviderHasDirectEndpoint 只检查配置中的上游直连 endpoint capability。
// 不得与转换后可服务 path 混用。
func ProviderHasDirectEndpoint(provider config.Provider, capability string) bool {
	return config.ProviderHasDirectEndpoint(provider, capability)
}

// ResolveTransportPlan 是请求期单一入口:在 ResolvedModelRoute 之上应用固定转发矩阵。
// 步骤 1—6 任一失败均返回 typed APIError,调用方不得创建上游请求。
func ResolveTransportPlan(cfg config.Config, method, path, modelID string) (TransportPlan, *APIError) {
	clientEndpoint := NormalizeClientEndpoint(path)
	operation := OperationForPath(clientEndpoint)
	clientProtocol := ClientProtocolForPath(clientEndpoint)
	modelID = strings.TrimSpace(modelID)

	// 入站白名单(执行端点)由 isSupportedInbound 保证;此处仍防御未知 path。
	if clientEndpoint == "" || operation == "" || clientProtocol == "" {
		return TransportPlan{}, &APIError{
			Code:           ErrorCodeEndpointUnsupported,
			Message:        fmt.Sprintf("inbound endpoint %q is not supported", path),
			Model:          modelID,
			ClientEndpoint: clientEndpoint,
			ClientProtocol: clientProtocol,
			Operation:      operation,
		}
	}
	if method != "" && method != http.MethodPost {
		// 执行端点仅 POST;/v1/models 不走本函数。
		return TransportPlan{}, &APIError{
			Code:           ErrorCodeEndpointUnsupported,
			Message:        fmt.Sprintf("method %s is not supported for endpoint %q", method, clientEndpoint),
			Model:          modelID,
			ClientEndpoint: clientEndpoint,
			ClientProtocol: clientProtocol,
			Operation:      operation,
		}
	}

	if modelID == "" {
		return TransportPlan{}, &APIError{
			Code:           ErrorCodeModelRequired,
			Message:        "model is required",
			Operation:      operation,
			ClientEndpoint: clientEndpoint,
			ClientProtocol: clientProtocol,
		}
	}

	route, ok := config.LookupResolvedModelRoute(cfg, modelID)
	if !ok {
		return TransportPlan{}, &APIError{
			Code:           ErrorCodeModelNotFound,
			Message:        fmt.Sprintf("model %q was not found in model_catalog", modelID),
			Model:          modelID,
			Operation:      operation,
			ClientEndpoint: clientEndpoint,
			ClientProtocol: clientProtocol,
		}
	}
	if !config.ModelSupportsOperation(config.ModelInfo{
		ID:         route.ModelID,
		Operations: route.Operations,
		RouteOwner: route.RouteOwner,
	}, operation) {
		return TransportPlan{}, &APIError{
			Code:           ErrorCodeOperationUnsupported,
			Message:        fmt.Sprintf("model %q does not support operation %q", modelID, operation),
			Model:          modelID,
			Operation:      operation,
			ClientEndpoint: clientEndpoint,
			ClientProtocol: clientProtocol,
		}
	}

	owner := strings.TrimSpace(route.RouteOwner)
	if owner == "" {
		return TransportPlan{}, &APIError{
			Code:           ErrorCodeRouteContractInvalid,
			Message:        fmt.Sprintf("model %q has no resolved route owner", modelID),
			Model:          modelID,
			Operation:      operation,
			ClientEndpoint: clientEndpoint,
			ClientProtocol: clientProtocol,
		}
	}
	provider, ok := cfg.Providers[owner]
	if !ok || provider.Disabled {
		return TransportPlan{}, &APIError{
			Code:           ErrorCodeProviderUnavailable,
			Message:        fmt.Sprintf("provider %q for model %q is unavailable", owner, modelID),
			Model:          modelID,
			Operation:      operation,
			ClientEndpoint: clientEndpoint,
			ClientProtocol: clientProtocol,
		}
	}

	plan, ok := applyTransportMatrix(clientEndpoint, clientProtocol, operation, modelID, owner, provider)
	if !ok {
		return TransportPlan{}, &APIError{
			Code:             ErrorCodeEndpointUnsupported,
			Message:          fmt.Sprintf("provider %q cannot serve endpoint %q for model %q", owner, clientEndpoint, modelID),
			Model:            modelID,
			Operation:        operation,
			ClientEndpoint:   clientEndpoint,
			ClientProtocol:   clientProtocol,
			UpstreamProtocol: provider.Protocol,
		}
	}
	return plan, nil
}

// applyTransportMatrix 仅应用固定转发矩阵(设计文档 §9)。
// 矩阵以外组合返回 false → endpoint_unsupported。
func applyTransportMatrix(clientEndpoint, clientProtocol, operation, modelID, owner string, provider config.Provider) (TransportPlan, bool) {
	upstreamProtocol := strings.TrimSpace(provider.Protocol)
	base := TransportPlan{
		ModelID:          modelID,
		Operation:        operation,
		ClientProtocol:   clientProtocol,
		ClientEndpoint:   clientEndpoint,
		RouteOwner:       owner,
		UpstreamProtocol: upstreamProtocol,
	}

	switch clientEndpoint {
	case "/v1/chat/completions":
		// OpenAI client → OpenAI native / Anthropic conversion
		if upstreamProtocol == "openai" && ProviderHasDirectEndpoint(provider, config.EndpointCapabilityChatCompletions) {
			base.UpstreamEndpoint = "/v1/chat/completions"
			base.Mode = TransportModeNative
			return base, true
		}
		if upstreamProtocol == "anthropic" && ProviderHasDirectEndpoint(provider, config.EndpointCapabilityMessages) {
			base.UpstreamEndpoint = "/v1/messages"
			base.Mode = TransportModeOpenAIToAnthropic
			return base, true
		}
	case "/v1/messages":
		// Anthropic client -> Anthropic native / OpenAI conversion
		if upstreamProtocol == "anthropic" && ProviderHasDirectEndpoint(provider, config.EndpointCapabilityMessages) {
			base.UpstreamEndpoint = "/v1/messages"
			base.Mode = TransportModeNative
			return base, true
		}
		if upstreamProtocol == "openai" && ProviderHasDirectEndpoint(provider, config.EndpointCapabilityChatCompletions) {
			base.UpstreamEndpoint = "/v1/chat/completions"
			base.Mode = TransportModeAnthropicToOpenAI
			return base, true
		}
	case "/v1/responses":
		if upstreamProtocol == "openai" && ProviderHasDirectEndpoint(provider, config.EndpointCapabilityResponses) {
			base.UpstreamEndpoint = "/v1/responses"
			base.Mode = TransportModeNative
			return base, true
		}
	case "/v1/completions":
		if upstreamProtocol == "openai" && ProviderHasDirectEndpoint(provider, config.EndpointCapabilityCompletions) {
			base.UpstreamEndpoint = "/v1/completions"
			base.Mode = TransportModeNative
			return base, true
		}
	case "/v1/embeddings":
		if upstreamProtocol == "openai" && ProviderHasDirectEndpoint(provider, config.EndpointCapabilityEmbeddings) {
			base.UpstreamEndpoint = "/v1/embeddings"
			base.Mode = TransportModeNative
			return base, true
		}
	}
	return TransportPlan{}, false
}

// TransportPlanForCanonical 在启动校验时判断 RouteOwner 是否能为 operation 的 canonical path 生成 plan。
// 与请求期 ResolveTransportPlan 使用同一矩阵,但不做 model catalog 查找。
func TransportPlanForCanonical(providerName string, provider config.Provider, modelID, operation string) (TransportPlan, bool) {
	path := config.OperationToPrimaryInboundPath(operation)
	if path == "" {
		return TransportPlan{}, false
	}
	clientProtocol := ClientProtocolForPath(path)
	return applyTransportMatrix(path, clientProtocol, operation, modelID, providerName, provider)
}
