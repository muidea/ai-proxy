package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"ai-proxy/internal/config"
	"ai-proxy/internal/proxy"
)

func main() {
	configPath := flag.String("config", os.Getenv("AI_PROXY_CONFIG"), "config file path")
	flag.Parse()

	resolvedConfigPath := config.ResolvePath(*configPath)
	cfg, err := config.Load(resolvedConfigPath)
	if err != nil {
		slog.Error("load config", slog.Any("error", err))
		os.Exit(1)
	}
	proxy.ConfigureLogger(cfg.LogFormat, cfg.DebugLog)

	application, err := buildAppWithConfigPath(cfg, resolvedConfigPath)
	if err != nil {
		slog.Error("build app", slog.Any("error", err))
		os.Exit(1)
	}
	defer application.Close()

	errCh := make(chan error, 1)
	go func() {
		slog.Info("ai-proxy listening",
			slog.String("addr", cfg.ListenAddr),
			slog.String("usage_file", cfg.UsageFile),
			slog.String("interactions", cfg.InteractionDir),
			slog.String("metrics", metricsURL(cfg.MetricsRemoteAccess)),
		)
		errCh <- application.server.ListenAndServe()
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	select {
	case sig := <-stop:
		slog.Info("received signal, shutting down", slog.String("signal", sig.String()))
	case err := <-errCh:
		if !errors.Is(err, http.ErrServerClosed) {
			slog.Error("server error", slog.Any("error", err))
			os.Exit(1)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := application.server.Shutdown(ctx); err != nil {
		slog.Error("shutdown", slog.Any("error", err))
		os.Exit(1)
	}
}
