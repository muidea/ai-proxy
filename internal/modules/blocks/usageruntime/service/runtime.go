// Package service 管理 usageruntime Block 的 DuckDB 生命周期。
package service

import (
	"context"
	"log/slog"
	"time"

	"ai-proxy/internal/pkg/aiproxycontract"
	"ai-proxy/internal/pkg/aiproxyusage"
)

type Runtime struct{ store usage.Store }

func NewRuntime(bootstrap aiproxycontract.Bootstrap) (*Runtime, error) {
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
