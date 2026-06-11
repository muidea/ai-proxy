package metrics

import (
	"net"
	"net/http"
	"strings"
)

// HandlerOptions 控制 /metrics、/stats 端点的访问策略。
type HandlerOptions struct {
	// AllowRemote 为 true 时允许非 loopback 地址访问;默认仅允许 loopback。
	AllowRemote bool
}

// Handler 返回一个挂载 /metrics 与 /stats 端点的 http.Handler。
// /metrics 输出 Prometheus 文本格式,Content-Type 由 PrometheusContentType 给出。
// /stats 输出 JSON 快照。
func Handler(reg *Registry, opts HandlerOptions) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		if !authorize(r, opts) {
			http.Error(w, "metrics access denied", http.StatusForbidden)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", PrometheusContentType)
		w.WriteHeader(http.StatusOK)
		if r.Method == http.MethodHead {
			return
		}
		_ = reg.WritePrometheus(w)
	})
	mux.HandleFunc("/stats", func(w http.ResponseWriter, r *http.Request) {
		if !authorize(r, opts) {
			http.Error(w, "stats access denied", http.StatusForbidden)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		payload, err := reg.StatsJSON()
		if err != nil {
			http.Error(w, "stats encode failed", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		if r.Method == http.MethodHead {
			return
		}
		_, _ = w.Write(payload)
	})
	return mux
}

// authorize 校验请求来源。AllowRemote=false 时仅 loopback 可访问。
func authorize(r *http.Request, opts HandlerOptions) bool {
	if opts.AllowRemote {
		return true
	}
	host := clientHost(r)
	if host == "" {
		return false
	}
	if host == "::1" || host == "127.0.0.1" || host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	if ip.IsLoopback() {
		return true
	}
	// 允许 Unix socket 场景(由 net/http 抽象为 @ 后跟随空 host)。
	if strings.HasPrefix(r.RemoteAddr, "@") {
		return true
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
