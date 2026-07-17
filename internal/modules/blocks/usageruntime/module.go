// Package usageruntime 是 DuckDB 用量存储的 framework Block。
package usageruntime

import (
	"bytes"
	"context"

	"ai-proxy/internal/modules/blocks/usageruntime/service"
	"ai-proxy/internal/pkg/aiproxycontract"

	cd "github.com/muidea/magicCommon/def"
	"github.com/muidea/magicCommon/event"
	pluginmodule "github.com/muidea/magicCommon/framework/plugin/module"
	"github.com/muidea/magicCommon/task"
)

func init() { pluginmodule.Register(New()) }

type Module struct {
	runtime  *service.Runtime
	observer event.SimpleObserver
}

func New() *Module            { return &Module{} }
func (m *Module) ID() string  { return aiproxycontract.UsageBlockID }
func (m *Module) Weight() int { return 20 }

func (m *Module) Setup(ctx context.Context, hub event.Hub, _ task.BackgroundRoutine) *cd.Error {
	bootstrap, err := aiproxycontract.RequestBootstrap(ctx, hub, m.ID())
	if err != nil {
		return cd.NewError(cd.IllegalParam, err.Error())
	}
	runtime, err := service.NewRuntime(bootstrap)
	if err != nil {
		return cd.NewError(cd.Unexpected, "init usage runtime: "+err.Error())
	}
	m.runtime = runtime
	m.observer = event.NewSimpleObserver(m.ID(), hub)
	m.observer.Subscribe(aiproxycontract.TopicAcquireUsage, m.handleAcquire)
	m.observer.Subscribe(aiproxycontract.TopicUsageCall, m.handleCall)
	return nil
}

func (m *Module) Run(context.Context) *cd.Error { return nil }

func (m *Module) Teardown(ctx context.Context) {
	if m.observer != nil {
		m.observer.Unsubscribe(aiproxycontract.TopicAcquireUsage)
		m.observer.Unsubscribe(aiproxycontract.TopicUsageCall)
	}
	m.observer = nil
	if m.runtime != nil {
		m.runtime.Close(ctx)
	}
	m.runtime = nil
}

func (m *Module) handleAcquire(ev event.Event, result event.Result) {
	if _, ok := ev.Data().(aiproxycontract.AcquireUsageCommand); !ok {
		result.Set(nil, cd.NewError(cd.IllegalParam, "invalid usage acquire command"))
		return
	}
	if m.runtime == nil || m.runtime.Store() == nil {
		result.Set(nil, cd.NewError(cd.IllegalParam, "usage block is not ready"))
		return
	}
	result.Set(aiproxycontract.AcquireUsageResult{}, nil)
}

func (m *Module) handleCall(ev event.Event, result event.Result) {
	if m.runtime == nil || m.runtime.Store() == nil {
		result.Set(nil, cd.NewError(cd.IllegalParam, "invalid usage command"))
		return
	}
	store := m.runtime.Store()
	var value any
	var err error
	switch command := ev.Data().(type) {
	case aiproxycontract.UsageStartCommand:
		err = store.Start(ev.Context(), command.Record)
	case aiproxycontract.UsageCompleteCommand:
		err = store.Complete(ev.Context(), command.Record)
	case aiproxycontract.UsageDashboardCommand:
		value, err = store.Dashboard(ev.Context(), command.Filter)
	case aiproxycontract.UsageCountCommand:
		value, err = store.Count(ev.Context(), command.Filter)
	case aiproxycontract.UsageEventsCommand:
		value, err = store.Events(ev.Context(), command.Filter)
	case aiproxycontract.UsageExportCommand:
		var buffer bytes.Buffer
		err = store.ExportCSV(ev.Context(), command.Filter, &buffer)
		value = aiproxycontract.UsageExportResult{Data: buffer.Bytes()}
	case aiproxycontract.UsageRecoverCommand:
		value, err = store.RecoverInterrupted(ev.Context(), command.At)
	case aiproxycontract.UsageCheckpointCommand:
		err = store.Checkpoint(ev.Context())
	case aiproxycontract.UsageHealthyCommand:
		value = store.Healthy()
	case aiproxycontract.UsageAllTimeCommand:
		value, err = store.AllTimeByKey(ev.Context())
	default:
		result.Set(nil, cd.NewError(cd.IllegalParam, "unsupported usage command"))
		return
	}
	if err != nil {
		result.Set(nil, cd.NewError(cd.Unexpected, err.Error()))
		return
	}
	result.Set(value, nil)
}
