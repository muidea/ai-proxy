// Package configruntime 是 Provider 配置的有状态 framework Block。
package configruntime

import (
	"context"

	"ai-proxy/internal/modules/blocks/configruntime/biz"
	configcommon "ai-proxy/internal/modules/blocks/configruntime/pkg/common"

	cd "github.com/muidea/magicCommon/def"
	"github.com/muidea/magicCommon/event"
	pluginmodule "github.com/muidea/magicCommon/framework/plugin/module"
	"github.com/muidea/magicCommon/task"
)

func init() { pluginmodule.Register(New()) }

type Module struct {
	bizPtr *biz.ConfigRuntime
}

func New() *Module            { return &Module{} }
func (i *Module) ID() string  { return configcommon.UnitID }
func (i *Module) Weight() int { return 10 }

func (i *Module) Setup(_ context.Context, hub event.Hub, background task.BackgroundRoutine) *cd.Error {
	bizPtr, err := biz.New(hub, background)
	if err != nil {
		return err
	}
	i.bizPtr = bizPtr
	return nil
}

func (i *Module) Run(ctx context.Context) *cd.Error {
	if i.bizPtr == nil {
		return cd.NewError(cd.IllegalParam, "config runtime biz is not configured")
	}
	return i.bizPtr.Run(ctx)
}

func (i *Module) Teardown(ctx context.Context) {
	if i.bizPtr != nil {
		i.bizPtr.Teardown(ctx)
	}
	i.bizPtr = nil
}
