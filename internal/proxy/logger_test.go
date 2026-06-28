package proxy

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
)

func TestTextLoggerColorsLinesByLevel(t *testing.T) {
	t.Setenv("LOG_FORMAT", "text")

	var out bytes.Buffer
	logger := newLoggerWithWriter(&out)

	logger.WarnContext(context.Background(), "warn message")
	logger.ErrorContext(context.Background(), "error message")

	logs := out.String()
	if !strings.Contains(logs, ansiYellow+"time=") || !strings.Contains(logs, "level=WARN") {
		t.Fatalf("expected WARN log line to be yellow, got: %q", logs)
	}
	if !strings.Contains(logs, ansiRed+"time=") || !strings.Contains(logs, "level=ERROR") {
		t.Fatalf("expected ERROR log line to be red, got: %q", logs)
	}
	if got := strings.Count(logs, ansiReset); got != 2 {
		t.Fatalf("expected one reset per colored line, got %d in %q", got, logs)
	}
	if strings.Contains(logs, "\n"+ansiReset) {
		t.Fatalf("expected reset code before line break, got: %q", logs)
	}
}

func TestJSONLoggerDoesNotColorizeStructuredOutput(t *testing.T) {
	t.Setenv("LOG_FORMAT", "json")

	var out bytes.Buffer
	logger := newLoggerWithWriter(&out)

	logger.ErrorContext(context.Background(), "error message", slog.String("provider", "openai"))

	logs := out.String()
	if strings.Contains(logs, "\033[") {
		t.Fatalf("expected JSON logs without ANSI color codes, got: %q", logs)
	}
	if !strings.Contains(logs, `"level":"ERROR"`) || !strings.Contains(logs, `"provider":"openai"`) {
		t.Fatalf("expected structured ERROR log, got: %q", logs)
	}
}
