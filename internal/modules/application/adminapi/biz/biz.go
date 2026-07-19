package biz

import (
	"context"

	admincommon "ai-proxy/internal/modules/application/adminapi/pkg/common"
	basebiz "ai-proxy/internal/modules/base/biz"
	configevents "ai-proxy/internal/modules/blocks/configruntime/pkg/events"
	metricsevents "ai-proxy/internal/modules/blocks/metricsruntime/pkg/events"
	usageevents "ai-proxy/internal/modules/blocks/usageruntime/pkg/events"
	"ai-proxy/internal/pkg/aiproxyconfig"
	"ai-proxy/internal/pkg/aiproxymetricsport"
	"ai-proxy/internal/pkg/aiproxyusage"

	cd "github.com/muidea/magicCommon/def"
	"github.com/muidea/magicCommon/event"
	"github.com/muidea/magicCommon/task"
)

// Admin owns the EventHub-backed dependencies used by the admin application
// adapter. HTTP handlers depend on this narrow RuntimeConfig implementation,
// never on EventHub directly.
type Admin struct {
	basebiz.Base
	bootstrap configevents.Bootstrap
	usage     usage.Store
	metrics   metricsport.Port
}

func New(ctx context.Context, hub event.Hub, background task.BackgroundRoutine) (*Admin, *cd.Error) {
	biz := &Admin{Base: basebiz.New(admincommon.UnitID, hub, background)}
	bootstrap, err := configevents.RequestBootstrap(ctx, biz.EventHub(), biz.ID())
	if err != nil {
		return nil, cd.NewError(cd.IllegalParam, err.Error())
	}
	usageStore, err := usageevents.RequestStore(ctx, biz.EventHub(), biz.ID())
	if err != nil {
		return nil, cd.NewError(cd.IllegalParam, err.Error())
	}
	metrics, err := metricsevents.RequestPort(ctx, biz.EventHub(), biz.ID())
	if err != nil {
		return nil, cd.NewError(cd.IllegalParam, err.Error())
	}
	biz.bootstrap = bootstrap
	biz.usage = usageStore
	biz.metrics = metrics
	return biz, nil
}

func (s *Admin) Run(context.Context) *cd.Error { return nil }

func (s *Admin) Teardown(context.Context) {
	s.bootstrap = configevents.Bootstrap{}
	s.usage = nil
	s.metrics = nil
}

func (s *Admin) ConfigPath() string        { return s.bootstrap.ConfigPath }
func (s *Admin) Config() config.Config     { return s.bootstrap.Config }
func (s *Admin) UsageStore() usage.Store   { return s.usage }
func (s *Admin) Metrics() metricsport.Port { return s.metrics }

func (s *Admin) ConfigSnapshot() config.Config {
	bootstrap, err := configevents.RequestBootstrap(context.Background(), s.EventHub(), s.ID())
	if err != nil {
		return config.Config{}
	}
	return bootstrap.Config
}

func (s *Admin) UpdateConfig(cfg config.Config) error {
	return configevents.Activate(context.Background(), s.EventHub(), s.ID(), cfg)
}
