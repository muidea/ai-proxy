// Package proxyapi 是入站 OpenAI/Anthropic 协议的 Application Module。
package proxyapi

import (
	"context"

	registrycommon "ai-proxy/internal/initiators/routeregistry/pkg/common"
	"ai-proxy/internal/modules/application/proxyapi/biz"
	proxycommon "ai-proxy/internal/modules/application/proxyapi/pkg/common"
	"ai-proxy/internal/modules/application/proxyapi/service/proxy"

	cd "github.com/muidea/magicCommon/def"
	"github.com/muidea/magicCommon/event"
	"github.com/muidea/magicCommon/framework/plugin/initiator"
	pluginmodule "github.com/muidea/magicCommon/framework/plugin/module"
	"github.com/muidea/magicCommon/task"
)

func init() { pluginmodule.Register(New()) }

type Module struct {
	handler *proxy.Handler
	routes  registrycommon.RouteRegistryHelper
	bizPtr  *biz.Proxy
}

func New() *Module            { return &Module{} }
func (m *Module) ID() string  { return proxycommon.UnitID }
func (m *Module) Weight() int { return 120 }

func (m *Module) Setup(ctx context.Context, hub event.Hub, background task.BackgroundRoutine) *cd.Error {
	bizPtr, err := biz.New(ctx, hub, background)
	if err != nil {
		return err
	}
	routes, routeErr := initiator.GetEntity(registrycommon.RouteRegistryInitiator, registrycommon.RouteRegistryHelper(nil))
	if routeErr != nil {
		return routeErr
	}
	if routes.GetRouteRegistry() == nil {
		return cd.NewError(cd.IllegalParam, "http route registry is unavailable")
	}
	proxy.ReserveMetricsModels(bizPtr.Metrics(), bizPtr.Config())
	m.handler = proxy.NewHandler(bizPtr.Config(), bizPtr.UsageStore(), bizPtr.Recorder(), bizPtr.Metrics())
	bizPtr.BindConfigUpdater(m.handler)
	m.routes = routes
	m.bizPtr = bizPtr
	return nil
}

func (m *Module) Run(ctx context.Context) *cd.Error {
	if m.bizPtr == nil || m.handler == nil || m.routes == nil || m.routes.GetRouteRegistry() == nil {
		return cd.NewError(cd.IllegalParam, "proxy routes are not configured")
	}
	if err := m.bizPtr.Run(ctx); err != nil {
		return err
	}
	proxy.RegisterRoutes(m.routes.GetRouteRegistry(), m.handler)
	return nil
}

func (m *Module) Teardown(ctx context.Context) {
	if m.bizPtr != nil {
		m.bizPtr.Teardown(ctx)
	}
	m.bizPtr, m.handler, m.routes = nil, nil, nil
}
