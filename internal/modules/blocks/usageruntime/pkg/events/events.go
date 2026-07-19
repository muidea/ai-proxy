package events

import (
	"time"

	"ai-proxy/internal/pkg/aiproxyusage"
)

const (
	TopicAcquire       = "aiproxy.usage.command.acquire"
	TopicStart         = "aiproxy.usage.command.start"
	TopicComplete      = "aiproxy.usage.command.complete"
	TopicDashboard     = "aiproxy.usage.command.dashboard"
	TopicCount         = "aiproxy.usage.command.count"
	TopicEvents        = "aiproxy.usage.command.events"
	TopicExport        = "aiproxy.usage.command.export"
	TopicFilterOptions = "aiproxy.usage.command.filter-options"
	TopicRecover       = "aiproxy.usage.command.recover"
	TopicCheckpoint    = "aiproxy.usage.command.checkpoint"
	TopicHealthy       = "aiproxy.usage.command.healthy"
	TopicAllTime       = "aiproxy.usage.command.all-time"
)

type AcquireCommand struct{}
type AcquireResult struct{}
type StartCommand struct{ Record usage.StartRecord }
type CompleteCommand struct{ Record usage.CompleteRecord }
type DashboardCommand struct{ Filter usage.UsageFilter }
type DashboardResult struct{ Value usage.Dashboard }
type CountCommand struct{ Filter usage.UsageFilter }
type CountResult struct{ Value int64 }
type EventsCommand struct{ Filter usage.EventFilter }
type EventsResult struct{ Value usage.EventPage }
type ExportCommand struct{ Filter usage.UsageFilter }
type ExportResult struct{ Data []byte }
type FilterOptionsCommand struct{ Query usage.FilterOptionsQuery }
type FilterOptionsResult struct{ Value usage.FilterOptionsResult }
type RecoverCommand struct{ At time.Time }
type RecoverResult struct{ Count int64 }
type CheckpointCommand struct{}
type HealthyCommand struct{}
type HealthyResult struct{ Value bool }
type AllTimeCommand struct{}
type AllTimeResult struct{ Value map[string]usage.Summary }
