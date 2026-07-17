// Package metricsport 定义跨运行单元使用指标能力的稳定窄契约。
package metricsport

import (
	"bytes"
	"time"

	"ai-proxy/internal/pkg/aiproxymetrics"
)

type Reporter interface {
	ReserveModels(provider string, models []string)
	RecordClientUsage(apiKeyID string, input, output int)
	RecordUsageStoreWriteError(phase string)
	RecordUsageStoreQuery(duration time.Duration, err error, healthy bool)
	RecordUsageStoreRecovered(count int64)
	SetUsageStoreHealthy(healthy bool)
	RecordRequestPlan(provider, model, route string, status int, duration time.Duration, outcome, clientEndpoint, upstreamProtocol, upstreamEndpoint, conversionMode string)
	RecordTokens(provider, model string, input, output, cached, cacheCreation int)
	RecordUpstreamAttempt(provider string, duration time.Duration, kind metrics.AttemptLatencyKind)
	RecordUpstreamError(provider string, status int)
}

type Reader interface {
	Prometheus() ([]byte, error)
	StatsJSON() ([]byte, error)
}

type Port interface {
	Reporter
	Reader
}

// Direct 仅供 Block 内部和单元测试把 Registry 适配为 Port；生产跨单元调用应使用 EventHub client。
func Direct(registry *metrics.Registry) Port {
	if registry == nil {
		return nil
	}
	return directPort{Registry: registry}
}

func AsPort(value any) Port {
	if port, ok := value.(Port); ok {
		return port
	}
	if registry, ok := value.(*metrics.Registry); ok {
		return Direct(registry)
	}
	return nil
}

type directPort struct{ *metrics.Registry }

func (p directPort) Prometheus() ([]byte, error) {
	var buffer bytes.Buffer
	err := p.WritePrometheus(&buffer)
	return buffer.Bytes(), err
}

func (p directPort) StatsJSON() ([]byte, error) { return p.Registry.StatsJSON() }
