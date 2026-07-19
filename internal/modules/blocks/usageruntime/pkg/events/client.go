package events

import (
	"context"
	"fmt"
	"io"
	"time"

	"ai-proxy/internal/modules/blocks/usageruntime/pkg/common"
	"ai-proxy/internal/pkg/aiproxyusage"

	"github.com/muidea/magicCommon/event"
)

// RequestStore returns an EventHub-backed Usage Block port without exposing
// the block's runtime store to other owners.
func RequestStore(ctx context.Context, hub event.Hub, source string) (usage.Store, error) {
	ev := event.NewEventWithContext(TopicAcquire, source, common.UnitID, event.NewHeader(), ctx, AcquireCommand{})
	result, err := send(hub, ev, "usage runtime")
	if err != nil {
		return nil, err
	}
	data, getErr := result.Get()
	if getErr != nil {
		return nil, fmt.Errorf("usage runtime command failed: %w", getErr)
	}
	if _, ok := data.(AcquireResult); !ok {
		return nil, fmt.Errorf("invalid usage runtime response")
	}
	return client{hub: hub, source: source}, nil
}

type client struct {
	hub    event.Hub
	source string
}

func (c client) Start(ctx context.Context, value usage.StartRecord) error {
	return c.sendEmpty(event.NewEventWithContext(TopicStart, c.source, common.UnitID, event.NewHeader(), ctx, StartCommand{Record: value}), "usage start")
}

func (c client) Complete(ctx context.Context, value usage.CompleteRecord) error {
	return c.sendEmpty(event.NewEventWithContext(TopicComplete, c.source, common.UnitID, event.NewHeader(), ctx, CompleteCommand{Record: value}), "usage complete")
}

func (c client) Dashboard(ctx context.Context, value usage.UsageFilter) (usage.Dashboard, error) {
	ev := event.NewEventWithContext(TopicDashboard, c.source, common.UnitID, event.NewHeader(), ctx, DashboardCommand{Filter: value})
	result, err := send(c.hub, ev, "usage dashboard")
	if err != nil {
		return usage.Dashboard{}, err
	}
	data, getErr := result.Get()
	if getErr != nil {
		return usage.Dashboard{}, fmt.Errorf("usage dashboard failed: %s", getErr.Message)
	}
	response, ok := data.(DashboardResult)
	if !ok {
		return usage.Dashboard{}, fmt.Errorf("invalid usage dashboard response")
	}
	return response.Value, nil
}

func (c client) Count(ctx context.Context, value usage.UsageFilter) (int64, error) {
	ev := event.NewEventWithContext(TopicCount, c.source, common.UnitID, event.NewHeader(), ctx, CountCommand{Filter: value})
	result, err := send(c.hub, ev, "usage count")
	if err != nil {
		return 0, err
	}
	data, getErr := result.Get()
	if getErr != nil {
		return 0, fmt.Errorf("usage count failed: %s", getErr.Message)
	}
	response, ok := data.(CountResult)
	if !ok {
		return 0, fmt.Errorf("invalid usage count response")
	}
	return response.Value, nil
}

func (c client) Events(ctx context.Context, value usage.EventFilter) (usage.EventPage, error) {
	ev := event.NewEventWithContext(TopicEvents, c.source, common.UnitID, event.NewHeader(), ctx, EventsCommand{Filter: value})
	result, err := send(c.hub, ev, "usage events")
	if err != nil {
		return usage.EventPage{}, err
	}
	data, getErr := result.Get()
	if getErr != nil {
		return usage.EventPage{}, fmt.Errorf("usage events failed: %s", getErr.Message)
	}
	response, ok := data.(EventsResult)
	if !ok {
		return usage.EventPage{}, fmt.Errorf("invalid usage events response")
	}
	return response.Value, nil
}

func (c client) ExportCSV(ctx context.Context, value usage.UsageFilter, writer io.Writer) error {
	ev := event.NewEventWithContext(TopicExport, c.source, common.UnitID, event.NewHeader(), ctx, ExportCommand{Filter: value})
	result, err := send(c.hub, ev, "usage export")
	if err != nil {
		return err
	}
	data, getErr := result.Get()
	if getErr != nil {
		return fmt.Errorf("usage export failed: %s", getErr.Message)
	}
	response, ok := data.(ExportResult)
	if !ok {
		return fmt.Errorf("invalid usage export response")
	}
	_, err = writer.Write(response.Data)
	return err
}

func (c client) FilterOptions(ctx context.Context, value usage.FilterOptionsQuery) (usage.FilterOptionsResult, error) {
	ev := event.NewEventWithContext(TopicFilterOptions, c.source, common.UnitID, event.NewHeader(), ctx, FilterOptionsCommand{Query: value})
	result, err := send(c.hub, ev, "usage filter options")
	if err != nil {
		return usage.FilterOptionsResult{}, err
	}
	data, getErr := result.Get()
	if getErr != nil {
		return usage.FilterOptionsResult{}, fmt.Errorf("usage filter options failed: %s", getErr.Message)
	}
	response, ok := data.(FilterOptionsResult)
	if !ok {
		return usage.FilterOptionsResult{}, fmt.Errorf("invalid usage filter options response")
	}
	return response.Value, nil
}

func (c client) RecoverInterrupted(ctx context.Context, value time.Time) (int64, error) {
	ev := event.NewEventWithContext(TopicRecover, c.source, common.UnitID, event.NewHeader(), ctx, RecoverCommand{At: value})
	result, err := send(c.hub, ev, "usage recover")
	if err != nil {
		return 0, err
	}
	data, getErr := result.Get()
	if getErr != nil {
		return 0, fmt.Errorf("usage recover failed: %s", getErr.Message)
	}
	response, ok := data.(RecoverResult)
	if !ok {
		return 0, fmt.Errorf("invalid usage recover response")
	}
	return response.Count, nil
}

func (c client) Checkpoint(ctx context.Context) error {
	return c.sendEmpty(event.NewEventWithContext(TopicCheckpoint, c.source, common.UnitID, event.NewHeader(), ctx, CheckpointCommand{}), "usage checkpoint")
}

func (c client) Close() error { return nil }

func (c client) Healthy() bool {
	ev := event.NewEventWithContext(TopicHealthy, c.source, common.UnitID, event.NewHeader(), context.Background(), HealthyCommand{})
	result, err := send(c.hub, ev, "usage healthy")
	if err != nil {
		return false
	}
	data, getErr := result.Get()
	response, ok := data.(HealthyResult)
	return getErr == nil && ok && response.Value
}

func (c client) AllTimeByKey(ctx context.Context) (map[string]usage.Summary, error) {
	ev := event.NewEventWithContext(TopicAllTime, c.source, common.UnitID, event.NewHeader(), ctx, AllTimeCommand{})
	result, err := send(c.hub, ev, "usage all time")
	if err != nil {
		return nil, err
	}
	data, getErr := result.Get()
	if getErr != nil {
		return nil, fmt.Errorf("usage all time failed: %s", getErr.Message)
	}
	response, ok := data.(AllTimeResult)
	if !ok {
		return nil, fmt.Errorf("invalid usage all time response")
	}
	return response.Value, nil
}

func (c client) sendEmpty(ev event.Event, name string) error {
	_, err := send(c.hub, ev, name)
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
