// Package adminapi 是 Provider 管理与使用统计的 Application Module。
package adminapi

import (
	"context"

	registrycommon "ai-proxy/internal/initiators/routeregistry/pkg/common"
	"ai-proxy/internal/modules/application/adminapi/biz"
	admincommon "ai-proxy/internal/modules/application/adminapi/pkg/common"
	"ai-proxy/internal/modules/application/adminapi/service/admin"

	cd "github.com/muidea/magicCommon/def"
	"github.com/muidea/magicCommon/event"
	"github.com/muidea/magicCommon/framework/plugin/initiator"
	pluginmodule "github.com/muidea/magicCommon/framework/plugin/module"
	"github.com/muidea/magicCommon/task"
)

func init() { pluginmodule.Register(New()) }

type Module struct {
	handler *admin.Handler
	routes  registrycommon.RouteRegistryHelper
	bizPtr  *biz.Admin
}

func New() *Module            { return &Module{} }
func (m *Module) ID() string  { return admincommon.UnitID }
func (m *Module) Weight() int { return 110 }

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
	m.handler = admin.NewHandlerWithUsage(bizPtr.ConfigPath(), bizPtr, bizPtr.UsageStore()).WithMetrics(bizPtr.Metrics())
	m.routes = routes
	m.bizPtr = bizPtr
	return nil
}

func (m *Module) Run(ctx context.Context) *cd.Error {
	if m.bizPtr == nil || m.handler == nil || m.routes == nil || m.routes.GetRouteRegistry() == nil || m.bizPtr.Metrics() == nil {
		return cd.NewError(cd.IllegalParam, "admin routes are not configured")
	}
	if err := m.bizPtr.Run(ctx); err != nil {
		return err
	}
	admin.RegisterRoutes(m.routes.GetRouteRegistry(), m.handler, m.bizPtr.Metrics(), m.bizPtr.Config())
	return nil
}

func (m *Module) Teardown(ctx context.Context) {
	if m.bizPtr != nil {
		m.bizPtr.Teardown(ctx)
	}
	m.handler = nil
	m.routes = nil
	m.bizPtr = nil
}
