// Package routeregistry 提供由 magicEngine 驱动的进程级 HTTP 路由与 listener 基础设施。
package routeregistry

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"ai-proxy/internal/initiators/routeregistry/pkg/common"
	"ai-proxy/internal/pkg/aiproxybootstrap"

	cd "github.com/muidea/magicCommon/def"
	"github.com/muidea/magicCommon/event"
	"github.com/muidea/magicCommon/framework/plugin/initiator"
	"github.com/muidea/magicCommon/task"
	enginehttp "github.com/muidea/magicEngine/http"
)

func init() { initiator.Register(New()) }

// routeRegistry 是进程级技术资源 owner。业务状态和业务路由策略不应放在这里。
type routeRegistry struct {
	routes   enginehttp.RouteRegistry
	handler  http.Handler
	server   *http.Server
	listener net.Listener
	done     chan error
}

var routeRegistryListen = net.Listen

func New() *routeRegistry { return &routeRegistry{} }

func (r *routeRegistry) ID() string { return common.RouteRegistryInitiator }

func (r *routeRegistry) Setup(_ context.Context, _ event.Hub, _ task.BackgroundRoutine) *cd.Error {
	bootstrap, ok := aiproxybootstrap.Current()
	if !ok {
		return cd.NewError(cd.IllegalParam, "ai-proxy bootstrap is not configured")
	}
	if bootstrap.Config.ListenAddr == "" {
		return cd.NewError(cd.IllegalParam, "http listen address is empty")
	}

	routes := enginehttp.NewRouteRegistry()
	server := enginehttp.NewHTTPServer()
	server.Bind(routes)
	handler, ok := server.(http.Handler)
	if !ok {
		return cd.NewError(cd.Unexpected, "magicEngine HTTP server does not implement http.Handler")
	}
	listener, err := routeRegistryListen("tcp", bootstrap.Config.ListenAddr)
	if err != nil {
		return cd.NewError(cd.Unexpected, fmt.Sprintf("listen %s: %v", bootstrap.Config.ListenAddr, err))
	}

	r.routes = routes
	r.handler = handler
	r.server = &http.Server{
		Addr:              bootstrap.Config.ListenAddr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
	}
	r.listener = listener
	r.done = make(chan error, 1)
	return nil
}

func (r *routeRegistry) Run(context.Context) *cd.Error {
	if r.server == nil || r.listener == nil || r.done == nil {
		return cd.NewError(cd.IllegalParam, "http route registry is not configured")
	}
	server, listener, done := r.server, r.listener, r.done
	go func() {
		err := server.Serve(listener)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		done <- err
	}()
	return nil
}

func (r *routeRegistry) Teardown(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}
	if r.server != nil {
		_ = r.server.Shutdown(ctx)
	}
	if r.listener != nil {
		_ = r.listener.Close()
	}
	r.routes = nil
	r.handler = nil
	r.server = nil
	r.listener = nil
}

func (r *routeRegistry) GetRouteRegistry() enginehttp.RouteRegistry { return r.routes }

func (r *routeRegistry) Done() <-chan error { return r.done }
