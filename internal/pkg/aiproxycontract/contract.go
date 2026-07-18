// Package aiproxycontract 定义 ai-proxy framework 组件之间的稳定 EventHub 契约。
package aiproxycontract

import (
	"time"

	"ai-proxy/internal/pkg/aiproxyconfig"
	"ai-proxy/internal/pkg/aiproxymetrics"
	"ai-proxy/internal/pkg/aiproxymetricsport"
	"ai-proxy/internal/pkg/aiproxyusage"
)

const (
	ConfigBlockID  = "aiproxy.config.block"
	UsageBlockID   = "aiproxy.usage.block"
	MetricsBlockID = "aiproxy.metrics.block"
	ProxyModuleID  = "aiproxy.proxy.module"
	AdminModuleID  = "aiproxy.admin.module"

	TopicBootstrap      = "aiproxy.config.command.bootstrap"
	TopicActivateConfig = "aiproxy.config.command.activate"
	TopicAcquireUsage   = "aiproxy.usage.command.acquire"
	TopicUsageCall      = "aiproxy.usage.command.call"
	TopicAcquireMetrics = "aiproxy.metrics.command.acquire"
	TopicMetricsRecord  = "aiproxy.metrics.event.record"
	TopicMetricsQuery   = "aiproxy.metrics.command.query"
	TopicUpdateProxy    = "aiproxy.proxy.command.update"
)

// Bootstrap 是唯一从 process service 注入 framework 的启动快照。
type Bootstrap struct {
	Config     config.Config
	ConfigPath string
}

type BootstrapCommand struct{}
type BootstrapResult struct{ Bootstrap Bootstrap }
type ActivateConfigCommand struct{ Config config.Config }

type AcquireUsageCommand struct{}
type AcquireUsageResult struct{}
type UsageStartCommand struct{ Record usage.StartRecord }
type UsageCompleteCommand struct{ Record usage.CompleteRecord }
type UsageDashboardCommand struct{ Filter usage.UsageFilter }
type UsageCountCommand struct{ Filter usage.UsageFilter }
type UsageEventsCommand struct{ Filter usage.EventFilter }
type UsageExportCommand struct{ Filter usage.UsageFilter }
type UsageExportResult struct{ Data []byte }
type UsageFilterOptionsCommand struct{ Query usage.FilterOptionsQuery }
type UsageRecoverCommand struct{ At time.Time }
type UsageCheckpointCommand struct{}
type UsageHealthyCommand struct{}
type UsageAllTimeCommand struct{}

type AcquireMetricsCommand struct{}
type AcquireMetricsResult struct{}

type MetricsRecordKind string

const (
	MetricsReserveModels        MetricsRecordKind = "reserve_models"
	MetricsClientUsage          MetricsRecordKind = "client_usage"
	MetricsUsageStoreWriteError MetricsRecordKind = "usage_store_write_error"
	MetricsUsageStoreQuery      MetricsRecordKind = "usage_store_query"
	MetricsUsageStoreRecovered  MetricsRecordKind = "usage_store_recovered"
	MetricsUsageStoreHealthy    MetricsRecordKind = "usage_store_healthy"
	MetricsRequestPlan          MetricsRecordKind = "request_plan"
	MetricsTokens               MetricsRecordKind = "tokens"
	MetricsUpstreamAttempt      MetricsRecordKind = "upstream_attempt"
	MetricsUpstreamError        MetricsRecordKind = "upstream_error"
)

type MetricsRecordCommand struct {
	Kind             MetricsRecordKind
	Provider         string
	Model            string
	Models           []string
	APIKeyID         string
	Route            string
	Outcome          string
	ClientEndpoint   string
	UpstreamProtocol string
	UpstreamEndpoint string
	ConversionMode   string
	Phase            string
	AttemptKind      metrics.AttemptLatencyKind
	Status           int
	Input            int
	Output           int
	Cached           int
	CacheCreation    int
	Count            int64
	Duration         time.Duration
	Healthy          bool
	Failed           bool
}

type MetricsPrometheusCommand struct{}
type MetricsStatsCommand struct{}
type MetricsBytesResult struct{ Data []byte }

var _ metricsport.Port = metricsClient{}

type UpdateProxyCommand struct{ Config config.Config }
