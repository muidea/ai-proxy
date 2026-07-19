package events

import (
	"context"
	"fmt"
	"time"

	"ai-proxy/internal/modules/blocks/metricsruntime/pkg/common"
	"ai-proxy/internal/pkg/aiproxymetrics"
	"ai-proxy/internal/pkg/aiproxymetricsport"

	"github.com/muidea/magicCommon/event"
)

// RequestPort returns an EventHub-backed Metrics Block port without exposing
// the block's registry to other owners.
func RequestPort(ctx context.Context, hub event.Hub, source string) (metricsport.Port, error) {
	ev := event.NewEventWithContext(TopicAcquire, source, common.UnitID, event.NewHeader(), ctx, AcquireCommand{})
	result, err := send(hub, ev, "metrics runtime")
	if err != nil {
		return nil, err
	}
	data, getErr := result.Get()
	if getErr != nil {
		return nil, fmt.Errorf("metrics runtime command failed: %w", getErr)
	}
	if _, ok := data.(AcquireResult); !ok {
		return nil, fmt.Errorf("invalid metrics runtime response")
	}
	return client{hub: hub, source: source}, nil
}

type client struct {
	hub    event.Hub
	source string
}

var _ metricsport.Port = client{}

func (c client) ReserveModels(provider string, models []string) {
	c.record(RecordCommand{Kind: ReserveModels, Provider: provider, Models: append([]string(nil), models...)})
}

func (c client) RecordClientUsage(apiKeyID string, input, output int) {
	c.record(RecordCommand{Kind: ClientUsage, APIKeyID: apiKeyID, Input: input, Output: output})
}

func (c client) RecordUsageStoreWriteError(phase string) {
	c.record(RecordCommand{Kind: UsageStoreWriteError, Phase: phase})
}

func (c client) RecordUsageStoreQuery(duration time.Duration, err error, healthy bool) {
	c.record(RecordCommand{Kind: UsageStoreQuery, Duration: duration, Failed: err != nil, Healthy: healthy})
}

func (c client) RecordUsageStoreRecovered(count int64) {
	c.record(RecordCommand{Kind: UsageStoreRecovered, Count: count})
}

func (c client) SetUsageStoreHealthy(healthy bool) {
	c.record(RecordCommand{Kind: UsageStoreHealthy, Healthy: healthy})
}

func (c client) RecordRequestPlan(provider, model, route string, status int, duration time.Duration, outcome, clientEndpoint, upstreamProtocol, upstreamEndpoint, conversionMode string) {
	c.record(RecordCommand{Kind: RequestPlan, Provider: provider, Model: model, Route: route, Status: status, Duration: duration, Outcome: outcome, ClientEndpoint: clientEndpoint, UpstreamProtocol: upstreamProtocol, UpstreamEndpoint: upstreamEndpoint, ConversionMode: conversionMode})
}

func (c client) RecordTokens(provider, model string, input, output, cached, cacheCreation int) {
	c.record(RecordCommand{Kind: Tokens, Provider: provider, Model: model, Input: input, Output: output, Cached: cached, CacheCreation: cacheCreation})
}

func (c client) RecordUpstreamAttempt(provider string, duration time.Duration, kind metrics.AttemptLatencyKind) {
	c.record(RecordCommand{Kind: UpstreamAttempt, Provider: provider, Duration: duration, AttemptKind: kind})
}

func (c client) RecordUpstreamError(provider string, status int) {
	c.record(RecordCommand{Kind: UpstreamError, Provider: provider, Status: status})
}

func (c client) Prometheus() ([]byte, error) {
	return c.query(event.NewEventWithContext(TopicPrometheus, c.source, common.UnitID, event.NewHeader(), context.Background(), PrometheusCommand{}), "metrics prometheus")
}

func (c client) StatsJSON() ([]byte, error) {
	return c.query(event.NewEventWithContext(TopicStats, c.source, common.UnitID, event.NewHeader(), context.Background(), StatsCommand{}), "metrics stats")
}

func (c client) record(command RecordCommand) {
	if c.hub == nil {
		return
	}
	c.hub.Post(event.NewEventWithContext(TopicRecord, c.source, common.UnitID, event.NewHeader(), context.Background(), command))
}

func (c client) query(ev event.Event, name string) ([]byte, error) {
	result, err := send(c.hub, ev, name)
	if err != nil {
		return nil, err
	}
	data, getErr := result.Get()
	if getErr != nil {
		return nil, fmt.Errorf("%s failed: %s", name, getErr.Message)
	}
	response, ok := data.(BytesResult)
	if !ok {
		return nil, fmt.Errorf("invalid %s response", name)
	}
	return response.Data, nil
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
