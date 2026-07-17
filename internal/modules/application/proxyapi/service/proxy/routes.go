package proxy

import (
	"context"
	"net/http"

	enginehttp "github.com/muidea/magicEngine/http"
)

// RegisterRoutes 只声明协议合同允许的入站路由。
// 不使用 `/**` 兜底，因而无需通过 Module Weight 隐式约束 Admin 与 Proxy 的注册顺序。
func RegisterRoutes(routes enginehttp.RouteRegistry, handler http.Handler) {
	if routes == nil || handler == nil {
		return
	}
	for _, route := range []struct {
		pattern string
		method  string
	}{
		{pattern: "/healthz", method: http.MethodGet},
		{pattern: "/v1/models", method: http.MethodGet},
		{pattern: "/v1/models", method: http.MethodPost},
		{pattern: "/v1/chat/completions", method: http.MethodPost},
		{pattern: "/v1/messages", method: http.MethodPost},
		{pattern: "/v1/responses", method: http.MethodPost},
		{pattern: "/v1/completions", method: http.MethodPost},
		{pattern: "/v1/embeddings", method: http.MethodPost},
	} {
		routes.AddHandler(route.pattern, route.method, serve(handler))
	}
}

func serve(handler http.Handler) enginehttp.RouteHandleFunc {
	return func(_ context.Context, w http.ResponseWriter, req *http.Request) {
		handler.ServeHTTP(w, req)
	}
}
