package proxy

import (
	"net/http"
	"strings"

	"ai-proxy/internal/config"
)

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
