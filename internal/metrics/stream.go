package metrics

import (
	"fmt"
	"net/http"
	"time"
)

// StreamHandlerOptions 控制 /stats/stream 端点的行为。
type StreamHandlerOptions struct {
	// Interval 是两次推送之间的间隔,默认 1 秒。
	Interval time.Duration
	// AllowRemote 与 /metrics 共享访问策略。
	AllowRemote bool
	// AllowedCIDRs 与 /metrics 共享。
	AllowedCIDRs []string
}

// StreamHandler 返回挂载 /stats/stream 的 http.Handler,按 Interval 周期
// 推送 /stats JSON 快照,适合 TUI 实时面板与简易 dashboard 集成。
func StreamHandler(reg *Registry, opts StreamHandlerOptions) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !authorize(r, HandlerOptions{AllowRemote: opts.AllowRemote, AllowedCIDRs: opts.AllowedCIDRs}) {
			http.Error(w, "stats stream access denied", http.StatusForbidden)
			return
		}
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}
		interval := opts.Interval
		if interval <= 0 {
			interval = time.Second
		}

		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.WriteHeader(http.StatusOK)
		flusher.Flush()

		ctx := r.Context()
		// 立即推送一次,让客户端不用等满 interval。
		writeStreamEvent(w, flusher, reg)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if !writeStreamEvent(w, flusher, reg) {
					return
				}
			}
		}
	})
}

func writeStreamEvent(w http.ResponseWriter, flusher http.Flusher, reg *Registry) bool {
	payload, err := reg.StatsJSON()
	if err != nil {
		_, _ = fmt.Fprintf(w, "event: error\ndata: %q\n\n", err.Error())
		flusher.Flush()
		return true
	}
	_, _ = fmt.Fprintf(w, "data: %s\n\n", payload)
	flusher.Flush()
	return true
}
