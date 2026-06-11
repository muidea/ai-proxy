package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
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
		log.Fatalf("load config: %v", err)
	}

	recorder := stats.NewCSVRecorder(cfg.UsageFile)
	interactionRecorder, err := archive.NewRecorder(cfg.InteractionDir, cfg.InteractionRetention)
	if err != nil {
		log.Fatalf("init interaction recorder: %v", err)
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
		fmt.Fprintf(os.Stderr, "slo_violation provider=%s rule=%s observed=%v threshold=%v detail=%q\n",
			v.Provider, v.Rule, v.Observed, v.Threshold, v.Detail)
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
		fmt.Printf("ai-proxy listening on %s, usage file: %s, interactions: %s, metrics: %s\n",
			cfg.ListenAddr,
			cfg.UsageFile,
			cfg.InteractionDir,
			metricsURL(cfg.MetricsRemoteAccess),
		)
		errCh <- server.ListenAndServe()
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	select {
	case sig := <-stop:
		fmt.Printf("received %s, shutting down\n", sig)
	case err := <-errCh:
		if !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("server error: %v", err)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := server.Shutdown(ctx); err != nil {
		log.Fatalf("shutdown: %v", err)
	}
}

func metricsURL(remote bool) string {
	if remote {
		return "remote-access"
	}
	return "loopback-only"
}
