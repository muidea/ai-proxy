package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"ai-proxy/internal/admin"
	"ai-proxy/internal/archive"
	"ai-proxy/internal/config"
	"ai-proxy/internal/metrics"
	"ai-proxy/internal/proxy"
	"ai-proxy/internal/usage"
)

// app 是可测试的服务装配结果。
type app struct {
	cfg        config.Config
	server     *http.Server
	evaluator  *metrics.SLOEvaluator
	sloCancel  context.CancelFunc
	sloDone    chan struct{} // Run goroutine 退出信号;未启动时为 nil
	registry   *metrics.Registry
	usageStore usage.Store
	closeOnce  sync.Once
}

// buildApp 根据配置装配 HTTP server、metrics 与 SLO,不监听端口。
func buildApp(cfg config.Config) (*app, error) {
	return buildAppWithConfigPath(cfg, "")
}

// buildAppWithConfigPath 在常规服务装配基础上启用项目内 Provider 管理页。
// configPath 为空时页面保持可查看，但禁止写回配置。
func buildAppWithConfigPath(cfg config.Config, configPath string) (*app, error) {
	usageStore, err := usage.OpenDuckDB(cfg.UsageStore)
	if err != nil {
		return nil, fmt.Errorf("init usage store: %w", err)
	}
	interactionRecorder, err := archive.NewRecorderOptions(cfg.InteractionDir, archive.RecorderOptions{
		MaxRounds:   cfg.InteractionRetention,
		FullContent: cfg.ArchiveFullContent,
	})
	if err != nil {
		_ = usageStore.Close()
		return nil, fmt.Errorf("init interaction recorder: %w", err)
	}
	registry := metrics.NewRegistry()
	if allTime, err := usageStore.AllTimeByKey(context.Background()); err != nil {
		registry.RecordUsageStoreQuery(0, err, usageStore.Healthy())
	} else {
		mirror := make(map[string]metrics.ClientUsage, len(allTime))
		for id, total := range allTime {
			mirror[id] = metrics.ClientUsage{Requests: total.Requests, InputTokens: total.InputTokens, OutputTokens: total.OutputTokens, TotalTokens: total.TotalTokens}
		}
		registry.InitializeClientUsage(mirror)
		registry.SetUsageStoreHealthy(usageStore.Healthy())
	}
	if recovered, ok := any(usageStore).(interface{ RecoveredEvents() int64 }); ok {
		registry.RecordUsageStoreRecovered(recovered.RecoveredEvents())
	}
	proxy.ReserveMetricsModels(registry, cfg)
	handler := proxy.NewHandler(cfg, usageStore, interactionRecorder, registry)
	metricsHandler := metrics.Handler(registry, metrics.HandlerOptions{
		AllowRemote:  cfg.MetricsRemoteAccess,
		AllowedCIDRs: cfg.MetricsAllowedCIDRs,
	})
	streamHandler := metrics.StreamHandler(registry, metrics.StreamHandlerOptions{
		AllowRemote:  cfg.MetricsRemoteAccess,
		AllowedCIDRs: cfg.MetricsAllowedCIDRs,
	})

	evaluator := metrics.NewSLOEvaluator(registry, metrics.SLOConfig{
		CacheHitRateMin:      cfg.SLO.CacheHitRateMin,
		UpstreamErrorRateMax: cfg.SLO.UpstreamErrorRateMax,
		P99LatencyMaxMS:      cfg.SLO.P99LatencyMaxMS,
		CheckInterval:        time.Duration(cfg.SLO.CheckIntervalSeconds) * time.Second,
	}, cfg.SLO.ViolationWebhook, func(ev metrics.SLOStateChange) {
		v := ev.Violation
		attrs := []any{
			slog.String("state", ev.State),
			slog.String("provider", v.Provider),
			slog.String("rule", v.Rule),
			slog.Float64("observed", v.Observed),
			slog.Float64("threshold", v.Threshold),
			slog.String("detail", v.Detail),
		}
		switch ev.State {
		case metrics.SLOStateResolved:
			slog.Info("slo recovered", attrs...)
		default:
			slog.Warn("slo violation", attrs...)
		}
	})
	// 挂接后 /metrics 可读取 webhook 队列与投递计数。
	registry.AttachSLO(evaluator)

	sloCtx, sloCancel := context.WithCancel(context.Background())
	var sloDone chan struct{}
	if cfg.SLO.CheckIntervalSeconds > 0 {
		sloDone = make(chan struct{})
		go func() {
			defer close(sloDone)
			evaluator.Run(sloCtx)
		}()
	}

	mux := http.NewServeMux()
	// 可观测性端点优先注册,避免被 proxy 兜底逻辑抢走。
	mux.Handle("/metrics", metricsHandler)
	mux.Handle("/stats", metricsHandler)
	mux.Handle("/stats/stream", streamHandler)
	adminHandler := admin.NewHandlerWithUsage(configPath, handler, usageStore).WithMetrics(registry)
	mux.Handle("/admin", adminHandler)
	mux.Handle("/admin/", adminHandler)
	mux.Handle("/healthz", handler)
	mux.Handle("/", handler)

	server := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		// 写超时不设全局值:流式响应可能持续数分钟,由 StreamIdleTimeout 控制。
	}

	return &app{
		cfg:        cfg,
		server:     server,
		evaluator:  evaluator,
		sloCancel:  sloCancel,
		sloDone:    sloDone,
		registry:   registry,
		usageStore: usageStore,
	}, nil
}

func (a *app) Close() {
	if a == nil {
		return
	}
	a.closeOnce.Do(func() {
		// 先停巡检,等待 Run 退出,避免 Close 后仍有 CheckNow 入队。
		if a.sloCancel != nil {
			a.sloCancel()
		}
		if a.sloDone != nil {
			<-a.sloDone
		}
		if a.evaluator != nil {
			a.evaluator.Close()
		}
		if a.registry != nil {
			a.registry.AttachSLO(nil)
		}
		// Shutdown 末尾 checkpoint + 关闭 usage store。
		if a.usageStore != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			if err := a.usageStore.Checkpoint(ctx); err != nil {
				slog.Error("usage store checkpoint failed", slog.Any("error", err))
				if a.registry != nil {
					a.registry.RecordUsageStoreCheckpointError()
				}
			}
			cancel()
			_ = a.usageStore.Close()
		}
	})
}

func metricsURL(remote bool) string {
	if remote {
		return "remote-access"
	}
	return "loopback-only"
}
