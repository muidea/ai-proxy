package proxy

import (
	"bytes"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
)

func init() {
	slog.SetDefault(newLogger())
}

func ConfigureLogger(format string, debug bool) {
	slog.SetDefault(newLoggerWithOptions(os.Stderr, format, debug))
}

// newLogger 根据环境变量构造 ai-proxy 使用的 slog logger。
// LOG_FORMAT=text 时输出人类可读 key=value,否则默认 JSON。
// AI_PROXY_DEBUG_LOG=true 启用 Debug 级别,否则 Info 起跳。
func newLogger() *slog.Logger {
	return newLoggerWithOptions(os.Stderr, os.Getenv("LOG_FORMAT"), parseDebugFlag())
}

func newLoggerWithWriter(w io.Writer) *slog.Logger {
	return newLoggerWithOptions(w, os.Getenv("LOG_FORMAT"), parseDebugFlag())
}

func newLoggerWithOptions(w io.Writer, format string, debug bool) *slog.Logger {
	level := slog.LevelInfo
	if debug {
		level = slog.LevelDebug
	}
	opts := &slog.HandlerOptions{Level: level}
	var handler slog.Handler
	if strings.EqualFold(format, "text") {
		handler = slog.NewTextHandler(newLevelColorWriter(w), opts)
	} else {
		handler = slog.NewJSONHandler(w, opts)
	}
	return slog.New(handler)
}

func parseDebugFlag() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("AI_PROXY_DEBUG_LOG")))
	return v == "1" || v == "true" || v == "yes"
}

type levelColorWriter struct {
	out io.Writer
	mu  sync.Mutex
	buf []byte
}

func newLevelColorWriter(out io.Writer) io.Writer {
	return &levelColorWriter{out: out}
}

func (w *levelColorWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.buf = append(w.buf, p...)
	for {
		idx := bytes.IndexByte(w.buf, '\n')
		if idx < 0 {
			return len(p), nil
		}
		line := append([]byte(nil), w.buf[:idx+1]...)
		w.buf = w.buf[idx+1:]
		if _, err := w.out.Write(colorizeLogLine(line)); err != nil {
			return 0, err
		}
	}
}

func colorizeLogLine(line []byte) []byte {
	color := logLevelColor(line)
	if color == "" {
		return line
	}
	if len(line) > 0 && line[len(line)-1] == '\n' {
		colored := make([]byte, 0, len(color)+len(line)+len(ansiReset))
		colored = append(colored, color...)
		colored = append(colored, line[:len(line)-1]...)
		colored = append(colored, ansiReset...)
		colored = append(colored, '\n')
		return colored
	}
	colored := make([]byte, 0, len(color)+len(line)+len(ansiReset))
	colored = append(colored, color...)
	colored = append(colored, line...)
	colored = append(colored, ansiReset...)
	return colored
}

func logLevelColor(line []byte) string {
	switch {
	case bytes.Contains(line, []byte("level=DEBUG")):
		return ansiCyan
	case bytes.Contains(line, []byte("level=INFO")):
		return ansiGreen
	case bytes.Contains(line, []byte("level=WARN")):
		return ansiYellow
	case bytes.Contains(line, []byte("level=ERROR")):
		return ansiRed
	default:
		return ""
	}
}

const (
	ansiReset  = "\033[0m"
	ansiRed    = "\033[31m"
	ansiGreen  = "\033[32m"
	ansiYellow = "\033[33m"
	ansiCyan   = "\033[36m"
)
