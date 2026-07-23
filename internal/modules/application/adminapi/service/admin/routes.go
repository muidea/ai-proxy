package admin

import (
	"context"
	"net/http"

	"ai-proxy/internal/modules/application/adminapi/service/observability"
	"ai-proxy/internal/pkg/aiproxyconfig"
	"ai-proxy/internal/pkg/aiproxymetricsport"

	enginehttp "github.com/muidea/magicEngine/http"
)

// RegisterRoutes 声明 Provider 管理、用量和可观测性 HTTP 路由。
// admin_base_path 为启动期路由配置；变更后必须重启。
func RegisterRoutes(routes enginehttp.RouteRegistry, handler http.Handler, registry metricsport.Port, cfg config.Config) {
	if routes == nil || handler == nil || registry == nil {
		return
	}
	base := cfg.AdminAuth.BasePath
	if base == "" {
		base = config.DefaultAdminBasePath
	}
	for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		routes.AddHandler(base, method, serve(handler))
		routes.AddHandler(base+"/**", method, serve(handler))
	}
	metricsHandler := observability.Handler(registry, observability.HandlerOptions{AllowRemote: cfg.MetricsRemoteAccess, AllowedCIDRs: cfg.MetricsAllowedCIDRs})
	routes.AddHandler("/metrics", http.MethodGet, serve(metricsHandler))
	routes.AddHandler("/metrics", http.MethodHead, serve(metricsHandler))
	routes.AddHandler("/stats", http.MethodGet, serve(metricsHandler))
	routes.AddHandler("/stats", http.MethodHead, serve(metricsHandler))
	streamHandler := observability.StreamHandler(registry, observability.StreamHandlerOptions{AllowRemote: cfg.MetricsRemoteAccess, AllowedCIDRs: cfg.MetricsAllowedCIDRs})
	routes.AddHandler("/stats/stream", http.MethodGet, serve(streamHandler))
}

func serve(handler http.Handler) enginehttp.RouteHandleFunc {
	return func(_ context.Context, w http.ResponseWriter, req *http.Request) {
		handler.ServeHTTP(w, req)
	}
}
