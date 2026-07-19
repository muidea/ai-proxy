package events

import (
	"context"
	"fmt"

	"ai-proxy/internal/modules/application/proxyapi/pkg/common"
	"ai-proxy/internal/pkg/aiproxyconfig"

	"github.com/muidea/magicCommon/event"
)

const TopicUpdateConfig = "aiproxy.proxy.command.update"

type UpdateConfigCommand struct{ Config config.Config }

func UpdateConfig(ctx context.Context, hub event.Hub, source string, cfg config.Config) error {
	if hub == nil {
		return fmt.Errorf("proxy update command event hub is unavailable")
	}
	ev := event.NewEventWithContext(TopicUpdateConfig, source, common.UnitID, event.NewHeader(), ctx, UpdateConfigCommand{Config: cfg})
	result := hub.Send(ev)
	if result == nil {
		return fmt.Errorf("proxy update command received no response")
	}
	if err := result.Error(); err != nil {
		return fmt.Errorf("proxy update command failed: %s", err.Message)
	}
	return nil
}
