// Package adminapi 是 Provider 管理与使用统计的 Application Module。
package adminapi

import (
	"context"

	registrycommon "ai-proxy/internal/initiators/routeregistry/pkg/common"
	"ai-proxy/internal/modules/application/adminapi/service/admin"
	"ai-proxy/internal/pkg/aiproxyconfig"
	"ai-proxy/internal/pkg/aiproxycontract"
	"ai-proxy/internal/pkg/aiproxymetricsport"

	cd "github.com/muidea/magicCommon/def"
	"github.com/muidea/magicCommon/event"
	"github.com/muidea/magicCommon/framework/plugin/initiator"
	pluginmodule "github.com/muidea/magicCommon/framework/plugin/module"
	"github.com/muidea/magicCommon/task"
)

func init() { pluginmodule.Register(New()) }

type Module struct {
	handler  *admin.Handler
	routes   registrycommon.RouteRegistryHelper
	registry metricsport.Port
	config   config.Config
}

func New() *Module            { return &Module{} }
func (m *Module) ID() string  { return aiproxycontract.AdminModuleID }
func (m *Module) Weight() int { return 110 }

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
	routes, routeErr := initiator.GetEntity(registrycommon.RouteRegistryInitiator, registrycommon.RouteRegistryHelper(nil))
	if routeErr != nil {
		return routeErr
	}
	if routes.GetRouteRegistry() == nil {
		return cd.NewError(cd.IllegalParam, "http route registry is unavailable")
	}
	m.handler = admin.NewHandlerWithUsage(bootstrap.ConfigPath, configRuntime{hub: hub}, store).WithMetrics(registry)
	m.routes = routes
	m.registry = registry
	m.config = bootstrap.Config
	return nil
}

func (m *Module) Run(context.Context) *cd.Error {
	if m.handler == nil || m.routes == nil || m.routes.GetRouteRegistry() == nil || m.registry == nil {
		return cd.NewError(cd.IllegalParam, "admin routes are not configured")
	}
	admin.RegisterRoutes(m.routes.GetRouteRegistry(), m.handler, m.registry, m.config)
	return nil
}

func (m *Module) Teardown(context.Context) {
	m.handler = nil
	m.routes = nil
	m.registry = nil
	m.config = config.Config{}
}

// configRuntime 只通过 EventHub 访问配置 owner 和依赖该配置的运行单元。
type configRuntime struct{ hub event.Hub }

func (r configRuntime) ConfigSnapshot() config.Config {
	bootstrap, err := aiproxycontract.RequestBootstrap(context.Background(), r.hub, aiproxycontract.AdminModuleID)
	if err != nil {
		return config.Config{}
	}
	return bootstrap.Config
}

func (r configRuntime) UpdateConfig(cfg config.Config) error {
	return aiproxycontract.ActivateConfig(context.Background(), r.hub, aiproxycontract.AdminModuleID, cfg)
}
