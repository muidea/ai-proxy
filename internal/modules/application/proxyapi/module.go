// Package proxyapi 是入站 OpenAI/Anthropic 协议的 Application Module。
package proxyapi

import (
	"context"

	registrycommon "ai-proxy/internal/initiators/routeregistry/pkg/common"
	"ai-proxy/internal/modules/application/proxyapi/service/proxy"
	"ai-proxy/internal/pkg/aiproxyarchive"
	"ai-proxy/internal/pkg/aiproxycontract"

	cd "github.com/muidea/magicCommon/def"
	"github.com/muidea/magicCommon/event"
	"github.com/muidea/magicCommon/framework/plugin/initiator"
	pluginmodule "github.com/muidea/magicCommon/framework/plugin/module"
	"github.com/muidea/magicCommon/task"
)

func init() { pluginmodule.Register(New()) }

type Module struct {
	handler  *proxy.Handler
	routes   registrycommon.RouteRegistryHelper
	observer event.SimpleObserver
}

func New() *Module            { return &Module{} }
func (m *Module) ID() string  { return aiproxycontract.ProxyModuleID }
func (m *Module) Weight() int { return 120 }

func (m *Module) Setup(ctx context.Context, hub event.Hub, _ task.BackgroundRoutine) *cd.Error {
	bootstrap, err := aiproxycontract.RequestBootstrap(ctx, hub, m.ID())
	if err != nil {
		return cd.NewError(cd.IllegalParam, err.Error())
	}
	store, err := aiproxycontract.RequestUsageStore(ctx, hub, m.ID())
	if err != nil {
		return cd.NewError(cd.IllegalParam, err.Error())
	}
	registry, err := aiproxycontract.RequestMetrics(ctx, hub, m.ID())
	if err != nil {
		return cd.NewError(cd.IllegalParam, err.Error())
	}
	recorder, err := archive.NewRecorderOptions(bootstrap.Config.InteractionDir, archive.RecorderOptions{
		MaxRounds: bootstrap.Config.InteractionRetention, FullContent: bootstrap.Config.ArchiveFullContent,
	})
	if err != nil {
		return cd.NewError(cd.Unexpected, "init interaction archive: "+err.Error())
	}
	routes, routeErr := initiator.GetEntity(registrycommon.RouteRegistryInitiator, registrycommon.RouteRegistryHelper(nil))
	if routeErr != nil {
		return routeErr
	}
	if routes.GetRouteRegistry() == nil {
		return cd.NewError(cd.IllegalParam, "http route registry is unavailable")
	}
	proxy.ReserveMetricsModels(registry, bootstrap.Config)
	m.handler = proxy.NewHandler(bootstrap.Config, store, recorder, registry)
	m.routes = routes
	m.observer = event.NewSimpleObserver(m.ID(), hub)
	m.observer.Subscribe(aiproxycontract.TopicUpdateProxy, m.handleUpdate)
	return nil
}

func (m *Module) Run(context.Context) *cd.Error {
	if m.handler == nil || m.routes == nil || m.routes.GetRouteRegistry() == nil {
		return cd.NewError(cd.IllegalParam, "proxy routes are not configured")
	}
	proxy.RegisterRoutes(m.routes.GetRouteRegistry(), m.handler)
	return nil
}

func (m *Module) Teardown(context.Context) {
	if m.observer != nil {
		m.observer.Unsubscribe(aiproxycontract.TopicUpdateProxy)
	}
	m.observer, m.handler, m.routes = nil, nil, nil
}

func (m *Module) handleUpdate(ev event.Event, result event.Result) {
	command, ok := ev.Data().(aiproxycontract.UpdateProxyCommand)
	if !ok {
		result.Set(nil, cd.NewError(cd.IllegalParam, "invalid proxy update command"))
		return
	}
	if m.handler == nil {
		result.Set(nil, cd.NewError(cd.IllegalParam, "proxy module is not ready"))
		return
	}
	if err := m.handler.UpdateConfig(command.Config); err != nil {
		result.Set(nil, cd.NewError(cd.IllegalParam, "update proxy config: "+err.Error()))
		return
	}
	result.Set(struct{}{}, nil)
}
