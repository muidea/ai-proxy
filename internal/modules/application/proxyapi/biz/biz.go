package biz

import (
	"context"
	"sync"

	proxycommon "ai-proxy/internal/modules/application/proxyapi/pkg/common"
	proxyevents "ai-proxy/internal/modules/application/proxyapi/pkg/events"
	basebiz "ai-proxy/internal/modules/base/biz"
	configevents "ai-proxy/internal/modules/blocks/configruntime/pkg/events"
	metricsevents "ai-proxy/internal/modules/blocks/metricsruntime/pkg/events"
	usageevents "ai-proxy/internal/modules/blocks/usageruntime/pkg/events"
	"ai-proxy/internal/pkg/aiproxyarchive"
	"ai-proxy/internal/pkg/aiproxyconfig"
	"ai-proxy/internal/pkg/aiproxymetricsport"
	"ai-proxy/internal/pkg/aiproxyusage"

	cd "github.com/muidea/magicCommon/def"
	"github.com/muidea/magicCommon/event"
	"github.com/muidea/magicCommon/task"
)

// ConfigUpdater is the narrow service-side capability required to atomically
// switch proxy configuration. Biz owns the EventHub command; the HTTP adapter
// only supplies this local owner implementation.
type ConfigUpdater interface {
	UpdateConfig(config.Config) error
}

type Proxy struct {
	basebiz.Base
	config   config.Config
	usage    usage.Store
	metrics  metricsport.Port
	recorder *archive.Recorder

	mu      sync.RWMutex
	updater ConfigUpdater
}

func New(ctx context.Context, hub event.Hub, background task.BackgroundRoutine) (*Proxy, *cd.Error) {
	biz := &Proxy{Base: basebiz.New(proxycommon.UnitID, hub, background)}
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
	recorder, err := archive.NewRecorderOptions(bootstrap.Config.InteractionDir, archive.RecorderOptions{
		MaxRounds: bootstrap.Config.InteractionRetention, FullContent: bootstrap.Config.ArchiveFullContent,
	})
	if err != nil {
		return nil, cd.NewError(cd.Unexpected, "init interaction archive: "+err.Error())
	}
	biz.config = bootstrap.Config
	biz.usage = usageStore
	biz.metrics = metrics
	biz.recorder = recorder
	biz.SubscribeFunc(proxyevents.TopicUpdateConfig, biz.handleUpdate)
	return biz, nil
}

func (s *Proxy) Run(context.Context) *cd.Error { return nil }

func (s *Proxy) Teardown(context.Context) {
	s.UnsubscribeFunc(proxyevents.TopicUpdateConfig)
	s.mu.Lock()
	s.updater = nil
	s.mu.Unlock()
	s.config = config.Config{}
	s.usage = nil
	s.metrics = nil
	s.recorder = nil
}

func (s *Proxy) Config() config.Config       { return s.config }
func (s *Proxy) UsageStore() usage.Store     { return s.usage }
func (s *Proxy) Metrics() metricsport.Port   { return s.metrics }
func (s *Proxy) Recorder() *archive.Recorder { return s.recorder }

func (s *Proxy) BindConfigUpdater(updater ConfigUpdater) {
	s.mu.Lock()
	s.updater = updater
	s.mu.Unlock()
}

func (s *Proxy) handleUpdate(ev event.Event, result event.Result) {
	command, ok := ev.Data().(proxyevents.UpdateConfigCommand)
	if !ok {
		result.Set(nil, cd.NewError(cd.IllegalParam, "invalid proxy update command"))
		return
	}
	s.mu.RLock()
	updater := s.updater
	s.mu.RUnlock()
	if updater == nil {
		result.Set(nil, cd.NewError(cd.IllegalParam, "proxy module is not ready"))
		return
	}
	if err := updater.UpdateConfig(command.Config); err != nil {
		result.Set(nil, cd.NewError(cd.IllegalParam, "update proxy config: "+err.Error()))
		return
	}
	s.config = command.Config
	result.Set(struct{}{}, nil)
}
