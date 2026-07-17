// Package aiproxy 提供主代理进程的 magicCommon process service。
package aiproxy

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"

	registrycommon "ai-proxy/internal/initiators/routeregistry/pkg/common"
	"ai-proxy/internal/pkg/aiproxybootstrap"
	"ai-proxy/internal/pkg/aiproxycontract"

	frameworkapplication "github.com/muidea/magicCommon/framework/application"
	"github.com/muidea/magicCommon/framework/plugin/initiator"
	frameworkservice "github.com/muidea/magicCommon/framework/service"
)

// Runtime 是进程级 service，不是业务 module。它选择并驱动 framework lifecycle；
// HTTP gateway Initiator、运行时 Block 和启动快照均由入口显式加载的组件提供。
type Runtime struct {
	application frameworkapplication.Application
	bootstrap   aiproxycontract.Bootstrap
}

func NewRuntime(cfg aiproxycontract.Bootstrap) *Runtime {
	configDir := "."
	if cfg.ConfigPath != "" {
		configDir = filepath.Dir(cfg.ConfigPath)
	}
	return &Runtime{
		bootstrap: cfg,
		application: frameworkapplication.NewApplication(frameworkapplication.Options{
			ConfigDir: configDir, ServiceName: "ai-proxy",
		}),
	}
}

func (r *Runtime) Startup(ctx context.Context) error {
	if r == nil || r.application == nil {
		return fmt.Errorf("ai-proxy runtime is not initialized")
	}
	aiproxybootstrap.Configure(r.bootstrap)
	if err := r.application.Startup(ctx, frameworkservice.DefaultService()); err != nil {
		return err
	}
	return nil
}

func (r *Runtime) Run(ctx context.Context) error {
	if r == nil || r.application == nil {
		return fmt.Errorf("ai-proxy runtime is not initialized")
	}
	if err := r.application.Run(ctx); err != nil {
		return err
	}
	return waitGateway(ctx)
}

func (r *Runtime) Shutdown(ctx context.Context) {
	if r != nil && r.application != nil {
		r.application.Shutdown(ctx)
	}
}

func waitGateway(ctx context.Context) error {
	gateway, getErr := initiator.GetEntity(registrycommon.RouteRegistryInitiator, registrycommon.GatewayRuntimeHelper(nil))
	if getErr != nil {
		return fmt.Errorf("get HTTP route registry initiator: %s", getErr.Message)
	}
	if gateway.Done() == nil {
		return fmt.Errorf("HTTP route registry completion channel is unavailable")
	}
	select {
	case err := <-gateway.Done():
		if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		return nil
	}
}

func MetricsAccessLabel(remote bool) string {
	if remote {
		return "remote-access"
	}
	return "loopback-only"
}
