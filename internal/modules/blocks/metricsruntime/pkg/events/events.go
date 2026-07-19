package events

import (
	"time"

	"ai-proxy/internal/pkg/aiproxymetrics"
)

const (
	TopicAcquire    = "aiproxy.metrics.command.acquire"
	TopicRecord     = "aiproxy.metrics.event.record"
	TopicPrometheus = "aiproxy.metrics.command.prometheus"
	TopicStats      = "aiproxy.metrics.command.stats"
)

type AcquireCommand struct{}
type AcquireResult struct{}

type RecordKind string

const (
	ReserveModels        RecordKind = "reserve_models"
	ClientUsage          RecordKind = "client_usage"
	UsageStoreWriteError RecordKind = "usage_store_write_error"
	UsageStoreQuery      RecordKind = "usage_store_query"
	UsageStoreRecovered  RecordKind = "usage_store_recovered"
	UsageStoreHealthy    RecordKind = "usage_store_healthy"
	RequestPlan          RecordKind = "request_plan"
	Tokens               RecordKind = "tokens"
	UpstreamAttempt      RecordKind = "upstream_attempt"
	UpstreamError        RecordKind = "upstream_error"
)

type RecordCommand struct {
	Kind             RecordKind
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

type PrometheusCommand struct{}
type StatsCommand struct{}
type BytesResult struct{ Data []byte }
