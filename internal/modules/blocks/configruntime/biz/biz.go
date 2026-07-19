package biz

import (
	"context"
	"sync"

	proxyevents "ai-proxy/internal/modules/application/proxyapi/pkg/events"
	basebiz "ai-proxy/internal/modules/base/biz"
	"ai-proxy/internal/modules/blocks/configruntime/pkg/common"
	configevents "ai-proxy/internal/modules/blocks/configruntime/pkg/events"
	"ai-proxy/internal/pkg/aiproxybootstrap"

	cd "github.com/muidea/magicCommon/def"
	"github.com/muidea/magicCommon/event"
	"github.com/muidea/magicCommon/task"
)

type ConfigRuntime struct {
	basebiz.Base

	mu        sync.RWMutex
	bootstrap configevents.Bootstrap
}

func New(hub event.Hub, background task.BackgroundRoutine) (*ConfigRuntime, *cd.Error) {
	bootstrap, ok := aiproxybootstrap.Current()
	if !ok {
		return nil, cd.NewError(cd.IllegalParam, "ai-proxy bootstrap is not configured")
	}
	if hub == nil {
		return nil, cd.NewError(cd.IllegalParam, "event hub is unavailable")
	}

	biz := &ConfigRuntime{
		Base:      basebiz.New(common.UnitID, hub, background),
		bootstrap: configevents.Bootstrap{Config: bootstrap.Config, ConfigPath: bootstrap.ConfigPath},
	}
	biz.SubscribeFunc(configevents.TopicBootstrap, biz.handleBootstrap)
	biz.SubscribeFunc(configevents.TopicActivate, biz.handleActivate)
	return biz, nil
}

func (s *ConfigRuntime) Run(context.Context) *cd.Error { return nil }

func (s *ConfigRuntime) Teardown(context.Context) {
	s.UnsubscribeFunc(configevents.TopicBootstrap)
	s.UnsubscribeFunc(configevents.TopicActivate)
	s.mu.Lock()
	s.bootstrap = configevents.Bootstrap{}
	s.mu.Unlock()
}

func (s *ConfigRuntime) handleBootstrap(ev event.Event, result event.Result) {
	s.mu.RLock()
	bootstrap := s.bootstrap
	s.mu.RUnlock()
	switch ev.Data().(type) {
	case configevents.BootstrapCommand:
		result.Set(configevents.BootstrapResult{Bootstrap: bootstrap}, nil)
	default:
		result.Set(nil, cd.NewError(cd.IllegalParam, "invalid bootstrap command"))
	}
}

func (s *ConfigRuntime) handleActivate(ev event.Event, result event.Result) {
	command, ok := ev.Data().(configevents.ActivateCommand)
	if !ok {
		result.Set(nil, cd.NewError(cd.IllegalParam, "invalid config activate command"))
		return
	}
	if err := proxyevents.UpdateConfig(ev.Context(), s.EventHub(), s.ID(), command.Config); err != nil {
		result.Set(nil, cd.NewError(cd.Unexpected, "activate proxy config: "+err.Error()))
		return
	}
	s.mu.Lock()
	s.bootstrap.Config = command.Config
	s.mu.Unlock()
	result.Set(struct{}{}, nil)
}
