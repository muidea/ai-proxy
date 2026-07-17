package metricsruntime

import (
	"context"
	"testing"

	"ai-proxy/internal/modules/blocks/metricsruntime/service"
	"ai-proxy/internal/pkg/aiproxycontract"
	"ai-proxy/internal/pkg/aiproxyusage"

	"github.com/muidea/magicCommon/event"
)

func TestHandleRecordAcceptsPostWithoutResult(t *testing.T) {
	runtime, err := service.NewRuntime(context.Background(), aiproxycontract.Bootstrap{}, usage.NewMemoryStore())
	if err != nil {
		t.Fatal(err)
	}
	module := &Module{runtime: runtime}
	ev := event.NewEventWithContext(
		aiproxycontract.TopicMetricsRecord,
		"test",
		aiproxycontract.MetricsBlockID,
		event.NewHeader(),
		context.Background(),
		aiproxycontract.MetricsRecordCommand{Kind: aiproxycontract.MetricsUsageStoreHealthy, Healthy: true},
	)

	// EventHub.Post 调用 observer 时 result 为 nil；处理器不得尝试回写同步响应。
	module.handleRecord(ev, nil)
}
