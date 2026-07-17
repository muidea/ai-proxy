// Package configruntime 是 Provider 配置的有状态 framework Block。
package configruntime

import (
	"context"
	"sync"

	"ai-proxy/internal/pkg/aiproxybootstrap"
	"ai-proxy/internal/pkg/aiproxycontract"

	cd "github.com/muidea/magicCommon/def"
	"github.com/muidea/magicCommon/event"
	pluginmodule "github.com/muidea/magicCommon/framework/plugin/module"
	"github.com/muidea/magicCommon/task"
)

func init() { pluginmodule.Register(New()) }

type Module struct {
	mu        sync.RWMutex
	bootstrap aiproxycontract.Bootstrap
	hub       event.Hub
	observer  event.SimpleObserver
}

func New() *Module            { return &Module{} }
func (i *Module) ID() string  { return aiproxycontract.ConfigBlockID }
func (i *Module) Weight() int { return 10 }

func (i *Module) Setup(_ context.Context, hub event.Hub, _ task.BackgroundRoutine) *cd.Error {
	bootstrap, ok := aiproxybootstrap.Current()
	if !ok {
		return cd.NewError(cd.IllegalParam, "ai-proxy bootstrap is not configured")
	}
	if hub == nil {
		return cd.NewError(cd.IllegalParam, "event hub is unavailable")
	}
	i.bootstrap = bootstrap
	i.hub = hub
	i.observer = event.NewSimpleObserver(i.ID(), hub)
	i.observer.Subscribe(aiproxycontract.TopicBootstrap, i.handleBootstrap)
	i.observer.Subscribe(aiproxycontract.TopicActivateConfig, i.handleActivate)
	return nil
}

func (i *Module) Run(context.Context) *cd.Error { return nil }

func (i *Module) Teardown(context.Context) {
	if i.observer != nil {
		i.observer.Unsubscribe(aiproxycontract.TopicBootstrap)
		i.observer.Unsubscribe(aiproxycontract.TopicActivateConfig)
	}
	i.observer = nil
	i.hub = nil
	i.bootstrap = aiproxycontract.Bootstrap{}
}

func (i *Module) handleBootstrap(ev event.Event, result event.Result) {
	if _, ok := ev.Data().(aiproxycontract.BootstrapCommand); !ok {
		result.Set(nil, cd.NewError(cd.IllegalParam, "invalid bootstrap command"))
		return
	}
	i.mu.RLock()
	bootstrap := i.bootstrap
	i.mu.RUnlock()
	result.Set(aiproxycontract.BootstrapResult{Bootstrap: bootstrap}, nil)
}

func (i *Module) handleActivate(ev event.Event, result event.Result) {
	command, ok := ev.Data().(aiproxycontract.ActivateConfigCommand)
	if !ok {
		result.Set(nil, cd.NewError(cd.IllegalParam, "invalid config activate command"))
		return
	}
	if err := aiproxycontract.UpdateProxyConfig(ev.Context(), i.hub, i.ID(), command.Config); err != nil {
		result.Set(nil, cd.NewError(cd.Unexpected, "activate proxy config: "+err.Error()))
		return
	}
	i.mu.Lock()
	i.bootstrap.Config = command.Config
	i.mu.Unlock()
	result.Set(struct{}{}, nil)
}
