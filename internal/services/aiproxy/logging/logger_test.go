package logging

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
)

func TestTextLoggerColorsLevelTokenOnly(t *testing.T) {
	t.Setenv("LOG_FORMAT", "text")

	var out bytes.Buffer
	logger := newLoggerWithWriter(&out)

	logger.WarnContext(context.Background(), "warn message")
	logger.ErrorContext(context.Background(), "error message")

	logs := out.String()
	if !strings.Contains(logs, ansiYellow+"level=WARN"+ansiReset) {
		t.Fatalf("expected only WARN level token to be yellow, got: %q", logs)
	}
	if !strings.Contains(logs, ansiRed+"level=ERROR"+ansiReset) {
		t.Fatalf("expected only ERROR level token to be red, got: %q", logs)
	}
	if strings.Contains(logs, ansiYellow+"time=") || strings.Contains(logs, ansiRed+"time=") {
		t.Fatalf("expected timestamp to remain uncolored, got: %q", logs)
	}
	if got := strings.Count(logs, ansiReset); got != 2 {
		t.Fatalf("expected one reset per colored level token, got %d in %q", got, logs)
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
