// Package usageruntime 是 DuckDB 用量存储的 framework Block。
package usageruntime

import (
	"context"

	"ai-proxy/internal/modules/blocks/usageruntime/biz"
	usagecommon "ai-proxy/internal/modules/blocks/usageruntime/pkg/common"

	cd "github.com/muidea/magicCommon/def"
	"github.com/muidea/magicCommon/event"
	pluginmodule "github.com/muidea/magicCommon/framework/plugin/module"
	"github.com/muidea/magicCommon/task"
)

func init() { pluginmodule.Register(New()) }

type Module struct {
	bizPtr *biz.UsageRuntime
}

func New() *Module            { return &Module{} }
func (m *Module) ID() string  { return usagecommon.UnitID }
func (m *Module) Weight() int { return 20 }

func (m *Module) Setup(ctx context.Context, hub event.Hub, background task.BackgroundRoutine) *cd.Error {
	bizPtr, err := biz.New(ctx, hub, background)
	if err != nil {
		return err
	}
	m.bizPtr = bizPtr
	return nil
}

func (m *Module) Run(ctx context.Context) *cd.Error {
	if m.bizPtr == nil {
		return cd.NewError(cd.IllegalParam, "usage runtime biz is not configured")
	}
	return m.bizPtr.Run(ctx)
}

func (m *Module) Teardown(ctx context.Context) {
	if m.bizPtr != nil {
		m.bizPtr.Teardown(ctx)
	}
	m.bizPtr = nil
}
