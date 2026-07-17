// Package service 管理 metricsruntime Block 的 Registry 与 SLO 生命周期。
package service

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"ai-proxy/internal/pkg/aiproxycontract"
	"ai-proxy/internal/pkg/aiproxymetrics"
	"ai-proxy/internal/pkg/aiproxyusage"
	"github.com/muidea/magicCommon/task"
)

type Runtime struct {
	registry  *metrics.Registry
	evaluator *metrics.SLOEvaluator
	config    aiproxycontract.Bootstrap
	cancel    context.CancelFunc
	done      chan struct{}
}

func NewRuntime(ctx context.Context, bootstrap aiproxycontract.Bootstrap, store usage.Store) (*Runtime, error) {
	registry := metrics.NewRegistry()
	if allTime, err := store.AllTimeByKey(ctx); err != nil {
		registry.RecordUsageStoreQuery(0, err, store.Healthy())
	} else {
		mirror := make(map[string]metrics.ClientUsage, len(allTime))
		for id, total := range allTime {
			mirror[id] = metrics.ClientUsage{Requests: total.Requests, InputTokens: total.InputTokens, OutputTokens: total.OutputTokens, TotalTokens: total.TotalTokens}
		}
		registry.InitializeClientUsage(mirror)
		registry.SetUsageStoreHealthy(store.Healthy())
	}
	if recovered, ok := any(store).(interface{ RecoveredEvents() int64 }); ok {
		registry.RecordUsageStoreRecovered(recovered.RecoveredEvents())
	}
	evaluator := metrics.NewSLOEvaluator(registry, metrics.SLOConfig{
		CacheHitRateMin: bootstrap.Config.SLO.CacheHitRateMin, UpstreamErrorRateMax: bootstrap.Config.SLO.UpstreamErrorRateMax,
		P99LatencyMaxMS: bootstrap.Config.SLO.P99LatencyMaxMS, CheckInterval: time.Duration(bootstrap.Config.SLO.CheckIntervalSeconds) * time.Second,
	}, bootstrap.Config.SLO.ViolationWebhook, logSLOChange)
	registry.AttachSLO(evaluator)
	return &Runtime{registry: registry, evaluator: evaluator, config: bootstrap}, nil
}

func (r *Runtime) Registry() *metrics.Registry {
	if r == nil {
		return nil
	}
	return r.registry
}

func (r *Runtime) Run(background task.BackgroundRoutine) error {
	if r == nil || r.evaluator == nil || r.config.Config.SLO.CheckIntervalSeconds <= 0 {
		return nil
	}
	if background == nil {
		return fmt.Errorf("background routine is unavailable")
	}
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel, r.done = cancel, make(chan struct{})
	if err := background.AsyncFunction(func() { defer close(r.done); r.evaluator.Run(ctx) }); err != nil {
		r.cancel = nil
		close(r.done)
		r.done = nil
		cancel()
		return err
	}
	return nil
}

func (r *Runtime) Close() {
	if r == nil {
		return
	}
	if r.cancel != nil {
		r.cancel()
	}
	if r.done != nil {
		<-r.done
	}
	if r.evaluator != nil {
		r.evaluator.Close()
	}
	if r.registry != nil {
		r.registry.AttachSLO(nil)
	}
	r.registry, r.evaluator, r.cancel, r.done = nil, nil, nil, nil
}

func logSLOChange(ev metrics.SLOStateChange) {
	v := ev.Violation
	attrs := []any{slog.String("state", ev.State), slog.String("provider", v.Provider), slog.String("rule", v.Rule), slog.Float64("observed", v.Observed), slog.Float64("threshold", v.Threshold), slog.String("detail", v.Detail)}
	if ev.State == metrics.SLOStateResolved {
		slog.Info("slo recovered", attrs...)
	} else {
		slog.Warn("slo violation", attrs...)
	}
}
