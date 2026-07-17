package aiproxycontract

import (
	"context"
	"fmt"
	"io"
	"time"

	"ai-proxy/internal/pkg/aiproxyconfig"
	"ai-proxy/internal/pkg/aiproxymetrics"
	"ai-proxy/internal/pkg/aiproxymetricsport"
	"ai-proxy/internal/pkg/aiproxyusage"

	"github.com/muidea/magicCommon/event"
)

// RequestBootstrap 从配置 Block 获取当前进程的启动快照。
func RequestBootstrap(ctx context.Context, hub event.Hub, source string) (Bootstrap, error) {
	ev := event.NewEventWithContext(TopicBootstrap, source, ConfigBlockID, event.NewHeader(), ctx, BootstrapCommand{})
	result, err := send(hub, ev, "bootstrap")
	if err != nil {
		return Bootstrap{}, err
	}
	data, getErr := result.Get()
	if getErr != nil {
		return Bootstrap{}, fmt.Errorf("bootstrap command failed: %s", getErr.Message)
	}
	response, ok := data.(BootstrapResult)
	if !ok {
		return Bootstrap{}, fmt.Errorf("invalid bootstrap response")
	}
	return response.Bootstrap, nil
}

func ActivateConfig(ctx context.Context, hub event.Hub, source string, cfg config.Config) error {
	ev := event.NewEventWithContext(TopicActivateConfig, source, ConfigBlockID, event.NewHeader(), ctx, ActivateConfigCommand{Config: cfg})
	_, err := send(hub, ev, "config activate")
	return err
}

func RequestUsageStore(ctx context.Context, hub event.Hub, source string) (usage.Store, error) {
	ev := event.NewEventWithContext(TopicAcquireUsage, source, UsageBlockID, event.NewHeader(), ctx, AcquireUsageCommand{})
	result, err := send(hub, ev, "usage runtime")
	if err != nil {
		return nil, err
	}
	data, getErr := result.Get()
	if getErr != nil {
		return nil, fmt.Errorf("usage runtime command failed: %w", getErr)
	}
	if _, ok := data.(AcquireUsageResult); !ok {
		return nil, fmt.Errorf("invalid usage runtime response")
	}
	return usageClient{hub: hub, source: source}, nil
}

type usageClient struct {
	hub    event.Hub
	source string
}

func (c usageClient) Start(ctx context.Context, v usage.StartRecord) error {
	_, err := c.call(ctx, UsageStartCommand{Record: v})
	return err
}
func (c usageClient) Complete(ctx context.Context, v usage.CompleteRecord) error {
	_, err := c.call(ctx, UsageCompleteCommand{Record: v})
	return err
}
func (c usageClient) Dashboard(ctx context.Context, v usage.UsageFilter) (usage.Dashboard, error) {
	r, e := c.call(ctx, UsageDashboardCommand{Filter: v})
	x, _ := r.(usage.Dashboard)
	return x, e
}
func (c usageClient) Count(ctx context.Context, v usage.UsageFilter) (int64, error) {
	r, e := c.call(ctx, UsageCountCommand{Filter: v})
	x, _ := r.(int64)
	return x, e
}
func (c usageClient) Events(ctx context.Context, v usage.EventFilter) (usage.EventPage, error) {
	r, e := c.call(ctx, UsageEventsCommand{Filter: v})
	x, _ := r.(usage.EventPage)
	return x, e
}
func (c usageClient) ExportCSV(ctx context.Context, v usage.UsageFilter, w io.Writer) error {
	r, e := c.call(ctx, UsageExportCommand{Filter: v})
	if e != nil {
		return e
	}
	result, ok := r.(UsageExportResult)
	if !ok {
		return fmt.Errorf("invalid usage export response")
	}
	_, e = w.Write(result.Data)
	return e
}
func (c usageClient) RecoverInterrupted(ctx context.Context, v time.Time) (int64, error) {
	r, e := c.call(ctx, UsageRecoverCommand{At: v})
	x, _ := r.(int64)
	return x, e
}
func (c usageClient) Checkpoint(ctx context.Context) error {
	_, e := c.call(ctx, UsageCheckpointCommand{})
	return e
}
func (c usageClient) Close() error { return nil }
func (c usageClient) Healthy() bool {
	r, e := c.call(context.Background(), UsageHealthyCommand{})
	x, _ := r.(bool)
	return e == nil && x
}
func (c usageClient) AllTimeByKey(ctx context.Context) (map[string]usage.Summary, error) {
	r, e := c.call(ctx, UsageAllTimeCommand{})
	x, _ := r.(map[string]usage.Summary)
	return x, e
}
func (c usageClient) call(ctx context.Context, command any) (any, error) {
	ev := event.NewEventWithContext(TopicUsageCall, c.source, UsageBlockID, event.NewHeader(), ctx, command)
	result, err := send(c.hub, ev, "usage command")
	if err != nil {
		return nil, err
	}
	data, getErr := result.Get()
	if getErr != nil {
		return nil, fmt.Errorf("usage command failed: %s", getErr.Message)
	}
	return data, nil
}

func RequestMetrics(ctx context.Context, hub event.Hub, source string) (metricsport.Port, error) {
	ev := event.NewEventWithContext(TopicAcquireMetrics, source, MetricsBlockID, event.NewHeader(), ctx, AcquireMetricsCommand{})
	result, err := send(hub, ev, "metrics runtime")
	if err != nil {
		return nil, err
	}
	data, getErr := result.Get()
	if getErr != nil {
		return nil, fmt.Errorf("metrics runtime command failed: %w", getErr)
	}
	if _, ok := data.(AcquireMetricsResult); !ok {
		return nil, fmt.Errorf("invalid metrics runtime response")
	}
	return metricsClient{hub: hub, source: source}, nil
}

type metricsClient struct {
	hub    event.Hub
	source string
}

func (c metricsClient) record(command MetricsRecordCommand) {
	if c.hub == nil {
		return
	}
	c.hub.Post(event.NewEventWithContext(TopicMetricsRecord, c.source, MetricsBlockID, event.NewHeader(), context.Background(), command))
}
func (c metricsClient) ReserveModels(provider string, models []string) {
	c.record(MetricsRecordCommand{Kind: MetricsReserveModels, Provider: provider, Models: append([]string(nil), models...)})
}
func (c metricsClient) RecordClientUsage(apiKeyID string, input, output int) {
	c.record(MetricsRecordCommand{Kind: MetricsClientUsage, APIKeyID: apiKeyID, Input: input, Output: output})
}
func (c metricsClient) RecordUsageStoreWriteError(phase string) {
	c.record(MetricsRecordCommand{Kind: MetricsUsageStoreWriteError, Phase: phase})
}
func (c metricsClient) RecordUsageStoreQuery(duration time.Duration, err error, healthy bool) {
	c.record(MetricsRecordCommand{Kind: MetricsUsageStoreQuery, Duration: duration, Failed: err != nil, Healthy: healthy})
}
func (c metricsClient) RecordUsageStoreRecovered(count int64) {
	c.record(MetricsRecordCommand{Kind: MetricsUsageStoreRecovered, Count: count})
}
func (c metricsClient) SetUsageStoreHealthy(healthy bool) {
	c.record(MetricsRecordCommand{Kind: MetricsUsageStoreHealthy, Healthy: healthy})
}
func (c metricsClient) RecordRequestPlan(provider, model, route string, status int, duration time.Duration, outcome, clientEndpoint, upstreamProtocol, upstreamEndpoint, conversionMode string) {
	c.record(MetricsRecordCommand{Kind: MetricsRequestPlan, Provider: provider, Model: model, Route: route, Status: status, Duration: duration, Outcome: outcome, ClientEndpoint: clientEndpoint, UpstreamProtocol: upstreamProtocol, UpstreamEndpoint: upstreamEndpoint, ConversionMode: conversionMode})
}
func (c metricsClient) RecordTokens(provider, model string, input, output, cached, cacheCreation int) {
	c.record(MetricsRecordCommand{Kind: MetricsTokens, Provider: provider, Model: model, Input: input, Output: output, Cached: cached, CacheCreation: cacheCreation})
}
func (c metricsClient) RecordUpstreamAttempt(provider string, duration time.Duration, kind metrics.AttemptLatencyKind) {
	c.record(MetricsRecordCommand{Kind: MetricsUpstreamAttempt, Provider: provider, Duration: duration, AttemptKind: kind})
}
func (c metricsClient) RecordUpstreamError(provider string, status int) {
	c.record(MetricsRecordCommand{Kind: MetricsUpstreamError, Provider: provider, Status: status})
}
func (c metricsClient) Prometheus() ([]byte, error) { return c.query(MetricsPrometheusCommand{}) }
func (c metricsClient) StatsJSON() ([]byte, error)  { return c.query(MetricsStatsCommand{}) }
func (c metricsClient) query(command any) ([]byte, error) {
	ev := event.NewEventWithContext(TopicMetricsQuery, c.source, MetricsBlockID, event.NewHeader(), context.Background(), command)
	result, err := send(c.hub, ev, "metrics query")
	if err != nil {
		return nil, err
	}
	data, getErr := result.Get()
	if getErr != nil {
		return nil, fmt.Errorf("metrics query failed: %s", getErr.Message)
	}
	response, ok := data.(MetricsBytesResult)
	if !ok {
		return nil, fmt.Errorf("invalid metrics query response")
	}
	return response.Data, nil
}

func UpdateProxyConfig(ctx context.Context, hub event.Hub, source string, cfg config.Config) error {
	ev := event.NewEventWithContext(TopicUpdateProxy, source, ProxyModuleID, event.NewHeader(), ctx, UpdateProxyCommand{Config: cfg})
	_, err := send(hub, ev, "proxy update")
	return err
}

func send(hub event.Hub, ev event.Event, name string) (event.Result, error) {
	if hub == nil {
		return nil, fmt.Errorf("%s command event hub is unavailable", name)
	}
	result := hub.Send(ev)
	if result == nil {
		return nil, fmt.Errorf("%s command received no response", name)
	}
	if err := result.Error(); err != nil {
		return nil, fmt.Errorf("%s command failed: %s", name, err.Message)
	}
	return result, nil
}
