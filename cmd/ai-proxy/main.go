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

	"ai-proxy/internal/archive"
	"ai-proxy/internal/config"
	"ai-proxy/internal/metrics"
	"ai-proxy/internal/proxy"
	"ai-proxy/internal/stats"
)

func main() {
	configPath := flag.String("config", os.Getenv("AI_PROXY_CONFIG"), "config file path")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		slog.Error("load config", slog.Any("error", err))
		os.Exit(1)
	}
	proxy.ConfigureLogger(cfg.LogFormat, cfg.DebugLog)

	recorder := stats.NewCSVRecorder(cfg.UsageFile)
	interactionRecorder, err := archive.NewRecorder(cfg.InteractionDir, cfg.InteractionRetention)
	if err != nil {
		slog.Error("init interaction recorder", slog.Any("error", err))
		os.Exit(1)
	}
	registry := metrics.NewRegistry()
	handler := proxy.NewHandler(cfg, recorder, interactionRecorder, registry)
	metricsHandler := metrics.Handler(registry, metrics.HandlerOptions{AllowRemote: cfg.MetricsRemoteAccess})
	streamHandler := metrics.StreamHandler(registry, metrics.StreamHandlerOptions{AllowRemote: cfg.MetricsRemoteAccess})

	// SLO evaluator:周期检查当前聚合是否满足阈值,命中时通过 slog 输出。
	evaluator := metrics.NewSLOEvaluator(registry, metrics.SLOConfig{
		CacheHitRateMin:      cfg.SLO.CacheHitRateMin,
		UpstreamErrorRateMax: cfg.SLO.UpstreamErrorRateMax,
		P99LatencyMaxMS:      cfg.SLO.P99LatencyMaxMS,
		CheckInterval:        time.Duration(cfg.SLO.CheckIntervalSeconds) * time.Second,
	}, cfg.SLO.ViolationWebhook, func(v metrics.SLOViolation) {
		slog.Warn("slo violation",
			slog.String("provider", v.Provider),
			slog.String("rule", v.Rule),
			slog.Float64("observed", v.Observed),
			slog.Float64("threshold", v.Threshold),
			slog.String("detail", v.Detail),
		)
	})
	sloCtx, sloCancel := context.WithCancel(context.Background())
	if cfg.SLO.CheckIntervalSeconds > 0 {
		go evaluator.Run(sloCtx)
	}
	defer sloCancel()

	mux := http.NewServeMux()
	// 可观测性端点优先注册,避免被 proxy 兜底逻辑抢走。
	mux.Handle("/metrics", metricsHandler)
	mux.Handle("/stats", metricsHandler)
	mux.Handle("/stats/stream", streamHandler)
	mux.Handle("/healthz", handler)
	mux.Handle("/", handler)

	server := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		slog.Info("ai-proxy listening",
			slog.String("addr", cfg.ListenAddr),
			slog.String("usage_file", cfg.UsageFile),
			slog.String("interactions", cfg.InteractionDir),
			slog.String("metrics", metricsURL(cfg.MetricsRemoteAccess)),
		)
		errCh <- server.ListenAndServe()
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
	if err := server.Shutdown(ctx); err != nil {
		slog.Error("shutdown", slog.Any("error", err))
		os.Exit(1)
	}
}

func metricsURL(remote bool) string {
	if remote {
		return "remote-access"
	}
	return "loopback-only"
}
