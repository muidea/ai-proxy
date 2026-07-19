package biz

import (
	"context"
	"log/slog"
	"time"

	configevents "ai-proxy/internal/modules/blocks/configruntime/pkg/events"
	"ai-proxy/internal/pkg/aiproxyusage"
)

// Runtime is the Usage Block's private DuckDB lifecycle owner.
type Runtime struct{ store usage.Store }

func NewRuntime(bootstrap configevents.Bootstrap) (*Runtime, error) {
	store, err := usage.OpenDuckDB(bootstrap.Config.UsageStore)
	if err != nil {
		return nil, err
	}
	return &Runtime{store: store}, nil
}

func (r *Runtime) Store() usage.Store {
	if r == nil {
		return nil
	}
	return r.store
}

func (r *Runtime) Close(ctx context.Context) {
	if r == nil || r.store == nil {
		return
	}
	checkpointCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	if err := r.store.Checkpoint(checkpointCtx); err != nil {
		slog.Error("usage store checkpoint failed", slog.Any("error", err))
	}
	cancel()
	_ = r.store.Close()
	r.store = nil
}
