// Package metricsruntime 是指标与 SLO 的 framework Block。
package metricsruntime

import (
	"bytes"
	"context"

	"ai-proxy/internal/modules/blocks/metricsruntime/service"
	"ai-proxy/internal/pkg/aiproxycontract"

	cd "github.com/muidea/magicCommon/def"
	"github.com/muidea/magicCommon/event"
	pluginmodule "github.com/muidea/magicCommon/framework/plugin/module"
	"github.com/muidea/magicCommon/task"
)

func init() { pluginmodule.Register(New()) }

type Module struct {
	runtime    *service.Runtime
	background task.BackgroundRoutine
	observer   event.SimpleObserver
}

func New() *Module            { return &Module{} }
func (m *Module) ID() string  { return aiproxycontract.MetricsBlockID }
func (m *Module) Weight() int { return 40 }

func (m *Module) Setup(ctx context.Context, hub event.Hub, background task.BackgroundRoutine) *cd.Error {
	bootstrap, err := aiproxycontract.RequestBootstrap(ctx, hub, m.ID())
	if err != nil {
		return cd.NewError(cd.IllegalParam, err.Error())
	}
	store, err := aiproxycontract.RequestUsageStore(ctx, hub, m.ID())
	if err != nil {
		return cd.NewError(cd.IllegalParam, err.Error())
	}
	runtime, err := service.NewRuntime(ctx, bootstrap, store)
	if err != nil {
		return cd.NewError(cd.Unexpected, "init metrics runtime: "+err.Error())
	}
	m.runtime = runtime
	m.background = background
	m.observer = event.NewSimpleObserver(m.ID(), hub)
	m.observer.Subscribe(aiproxycontract.TopicAcquireMetrics, m.handleAcquire)
	m.observer.Subscribe(aiproxycontract.TopicMetricsRecord, m.handleRecord)
	m.observer.Subscribe(aiproxycontract.TopicMetricsQuery, m.handleQuery)
	return nil
}

func (m *Module) Run(context.Context) *cd.Error {
	if m.runtime != nil {
		if err := m.runtime.Run(m.background); err != nil {
			return cd.NewError(cd.Unexpected, "run metrics runtime: "+err.Error())
		}
	}
	return nil
}

func (m *Module) Teardown(context.Context) {
	if m.observer != nil {
		m.observer.Unsubscribe(aiproxycontract.TopicAcquireMetrics)
		m.observer.Unsubscribe(aiproxycontract.TopicMetricsRecord)
		m.observer.Unsubscribe(aiproxycontract.TopicMetricsQuery)
	}
	m.observer = nil
	if m.runtime != nil {
		m.runtime.Close()
	}
	m.runtime = nil
	m.background = nil
}

func (m *Module) handleAcquire(ev event.Event, result event.Result) {
	if _, ok := ev.Data().(aiproxycontract.AcquireMetricsCommand); !ok {
		result.Set(nil, cd.NewError(cd.IllegalParam, "invalid metrics acquire command"))
		return
	}
	if m.runtime == nil || m.runtime.Registry() == nil {
		result.Set(nil, cd.NewError(cd.IllegalParam, "metrics block is not ready"))
		return
	}
	result.Set(aiproxycontract.AcquireMetricsResult{}, nil)
}

// handleRecord 只处理 EventHub Post 通知；Post 没有 Result，不能尝试写同步响应。
func (m *Module) handleRecord(ev event.Event, _ event.Result) {
	command, ok := ev.Data().(aiproxycontract.MetricsRecordCommand)
	if !ok || m.runtime == nil || m.runtime.Registry() == nil {
		return
	}
	registry := m.runtime.Registry()
	switch command.Kind {
	case aiproxycontract.MetricsReserveModels:
		registry.ReserveModels(command.Provider, command.Models)
	case aiproxycontract.MetricsClientUsage:
		registry.RecordClientUsage(command.APIKeyID, command.Input, command.Output)
	case aiproxycontract.MetricsUsageStoreWriteError:
		registry.RecordUsageStoreWriteError(command.Phase)
	case aiproxycontract.MetricsUsageStoreQuery:
		var err error
		if command.Failed {
			err = context.DeadlineExceeded
		}
		registry.RecordUsageStoreQuery(command.Duration, err, command.Healthy)
	case aiproxycontract.MetricsUsageStoreRecovered:
		registry.RecordUsageStoreRecovered(command.Count)
	case aiproxycontract.MetricsUsageStoreHealthy:
		registry.SetUsageStoreHealthy(command.Healthy)
	case aiproxycontract.MetricsRequestPlan:
		registry.RecordRequestPlan(command.Provider, command.Model, command.Route, command.Status, command.Duration, command.Outcome, command.ClientEndpoint, command.UpstreamProtocol, command.UpstreamEndpoint, command.ConversionMode)
	case aiproxycontract.MetricsTokens:
		registry.RecordTokens(command.Provider, command.Model, command.Input, command.Output, command.Cached, command.CacheCreation)
	case aiproxycontract.MetricsUpstreamAttempt:
		registry.RecordUpstreamAttempt(command.Provider, command.Duration, command.AttemptKind)
	case aiproxycontract.MetricsUpstreamError:
		registry.RecordUpstreamError(command.Provider, command.Status)
	default:
		return
	}
}

func (m *Module) handleQuery(ev event.Event, result event.Result) {
	if m.runtime == nil || m.runtime.Registry() == nil {
		result.Set(nil, cd.NewError(cd.IllegalParam, "metrics block is not ready"))
		return
	}
	registry := m.runtime.Registry()
	var data []byte
	var err error
	switch ev.Data().(type) {
	case aiproxycontract.MetricsPrometheusCommand:
		var buffer bytes.Buffer
		err = registry.WritePrometheus(&buffer)
		data = buffer.Bytes()
	case aiproxycontract.MetricsStatsCommand:
		data, err = registry.StatsJSON()
	default:
		result.Set(nil, cd.NewError(cd.IllegalParam, "invalid metrics query command"))
		return
	}
	if err != nil {
		result.Set(nil, cd.NewError(cd.Unexpected, err.Error()))
		return
	}
	result.Set(aiproxycontract.MetricsBytesResult{Data: data}, nil)
}
