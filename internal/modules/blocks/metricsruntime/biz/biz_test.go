package biz

import (
	"context"
	"testing"

	configevents "ai-proxy/internal/modules/blocks/configruntime/pkg/events"
	metricscommon "ai-proxy/internal/modules/blocks/metricsruntime/pkg/common"
	metricsevents "ai-proxy/internal/modules/blocks/metricsruntime/pkg/events"
	"ai-proxy/internal/pkg/aiproxyusage"

	"github.com/muidea/magicCommon/event"
)

func TestHandleRecordAcceptsPostWithoutResult(t *testing.T) {
	runtime, err := NewRuntime(context.Background(), configevents.Bootstrap{}, usage.NewMemoryStore())
	if err != nil {
		t.Fatal(err)
	}
	biz := &MetricsRuntime{runtime: runtime}
	ev := event.NewEventWithContext(
		metricsevents.TopicRecord,
		"test",
		metricscommon.UnitID,
		event.NewHeader(),
		context.Background(),
		metricsevents.RecordCommand{Kind: metricsevents.UsageStoreHealthy, Healthy: true},
	)

	// EventHub.Post invokes observers with result=nil; handlers must not reply.
	biz.handleRecord(ev, nil)
}
