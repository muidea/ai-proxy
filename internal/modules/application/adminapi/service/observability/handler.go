// Package observability 提供 gateway 的 metrics、stats 和 SSE HTTP adapter。
package observability

import (
	"net"
	"net/http"
	"strings"

	"ai-proxy/internal/pkg/aiproxymetrics"
	"ai-proxy/internal/pkg/aiproxymetricsport"
)

// HandlerOptions 控制 /metrics、/stats 端点的访问策略。
type HandlerOptions struct {
	// AllowRemote 为 true 时允许非 loopback 地址访问;默认仅允许 loopback。
	AllowRemote bool
	// AllowedCIDRs 在 AllowRemote=true 时生效;非空则仅这些网段可访问,空表示允许任意远程。
	AllowedCIDRs []string
}

// Handler 返回一个挂载 /metrics 与 /stats 端点的 http.Handler。
// /metrics 输出 Prometheus 文本格式,Content-Type 由 PrometheusContentType 给出。
// /stats 输出 JSON 快照。
func Handler(source any, opts HandlerOptions) http.Handler {
	reg := metricsport.AsPort(source)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/metrics":
			serveMetrics(w, r, reg, opts)
		case "/stats":
			serveStats(w, r, reg, opts)
		default:
			http.NotFound(w, r)
		}
	})
}

func serveMetrics(w http.ResponseWriter, r *http.Request, reg metricsport.Reader, opts HandlerOptions) {
	if !authorize(r, opts) {
		http.Error(w, "metrics access denied", http.StatusForbidden)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if reg == nil {
		http.Error(w, "metrics unavailable", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", metrics.PrometheusContentType)
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		payload, err := reg.Prometheus()
		if err == nil {
			_, _ = w.Write(payload)
		}
	}
}

func serveStats(w http.ResponseWriter, r *http.Request, reg metricsport.Reader, opts HandlerOptions) {
	if !authorize(r, opts) {
		http.Error(w, "stats access denied", http.StatusForbidden)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if reg == nil {
		http.Error(w, "metrics unavailable", http.StatusServiceUnavailable)
		return
	}
	payload, err := reg.StatsJSON()
	if err != nil {
		http.Error(w, "stats encode failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(payload)
	}
}

// authorize 校验请求来源。
// AllowRemote=false:仅 loopback。
// AllowRemote=true 且 AllowedCIDRs 非空:仅白名单网段(+loopback)。
// AllowRemote=true 且 AllowedCIDRs 为空:允许任意远程。
func authorize(r *http.Request, opts HandlerOptions) bool {
	host := clientHost(r)
	if host == "" {
		return false
	}
	if host == "::1" || host == "127.0.0.1" || host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	if ip != nil && ip.IsLoopback() {
		return true
	}
	// 允许 Unix socket 场景(由 net/http 抽象为 @ 后跟随空 host)。
	if strings.HasPrefix(r.RemoteAddr, "@") {
		return true
	}
	if !opts.AllowRemote {
		return false
	}
	if len(opts.AllowedCIDRs) == 0 {
		return true
	}
	if ip == nil {
		return false
	}
	for _, cidr := range opts.AllowedCIDRs {
		cidr = strings.TrimSpace(cidr)
		if cidr == "" {
			continue
		}
		// 允许写单 IP。
		if single := net.ParseIP(cidr); single != nil {
			if single.Equal(ip) {
				return true
			}
			continue
		}
		_, network, err := net.ParseCIDR(cidr)
		if err != nil {
			continue
		}
		if network.Contains(ip) {
			return true
		}
	}
	return false
}

func clientHost(r *http.Request) string {
	if r == nil {
		return ""
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil {
		return host
	}
	return r.RemoteAddr
}
