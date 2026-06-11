package proxy

import (
	"log/slog"
	"os"
	"strings"
)

func init() {
	slog.SetDefault(newLogger())
}

// newLogger 根据环境变量构造 ai-proxy 使用的 slog logger。
// LOG_FORMAT=text 时输出人类可读 key=value,否则默认 JSON。
// AI_PROXY_DEBUG_LOG=true 启用 Debug 级别,否则 Info 起跳。
func newLogger() *slog.Logger {
	level := slog.LevelInfo
	if parseDebugFlag() {
		level = slog.LevelDebug
	}
	opts := &slog.HandlerOptions{Level: level}
	var handler slog.Handler
	if strings.EqualFold(os.Getenv("LOG_FORMAT"), "text") {
		handler = slog.NewTextHandler(os.Stderr, opts)
	} else {
		handler = slog.NewJSONHandler(os.Stderr, opts)
	}
	return slog.New(handler)
}

func parseDebugFlag() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("AI_PROXY_DEBUG_LOG")))
	return v == "1" || v == "true" || v == "yes"
}
