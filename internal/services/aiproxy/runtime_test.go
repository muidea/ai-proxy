package aiproxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	registrycommon "ai-proxy/internal/initiators/routeregistry/pkg/common"
	configevents "ai-proxy/internal/modules/blocks/configruntime/pkg/events"
	"ai-proxy/internal/pkg/aiproxyconfig"
	"github.com/muidea/magicCommon/framework/plugin/initiator"
	enginehttp "github.com/muidea/magicEngine/http"
)

func TestRuntimeNeedsRegisteredFrameworkComponents(t *testing.T) {
	runtime := NewRuntime(configevents.Bootstrap{Config: testConfig(t.TempDir())})
	if err := runtime.Startup(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { runtime.Shutdown(context.Background()) })

	// Startup 期间已验证 Initiator 与各 Block/Module 的依赖接线。
	// 继续执行 plugin Run，并验证 Application Module 已通过 RouteRegistry Initiator 注册路由。
	if err := runtime.application.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertGatewayRoutes(t)
	if err := startGateway(); err != nil {
		t.Fatalf("start gateway: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := waitGateway(ctx); err != nil {
		t.Fatalf("wait gateway command: %v", err)
	}
}

func assertGatewayRoutes(t *testing.T) {
	t.Helper()
	router, err := initiator.GetEntity(registrycommon.RouteRegistryInitiator, registrycommon.RouteRegistryHelper(nil))
	if err != nil {
		t.Fatal(err)
	}
	routes := router.GetRouteRegistry()
	if routes == nil {
		t.Fatal("route registry is unavailable")
	}
	for _, tc := range []struct {
		method string
		path   string
		status int
	}{
		{method: http.MethodGet, path: "/healthz", status: http.StatusOK},
		{method: http.MethodGet, path: "/metrics", status: http.StatusOK},
		{method: http.MethodGet, path: "/admin/", status: http.StatusOK},
		{method: http.MethodGet, path: "/v1/models", status: http.StatusOK},
		{method: http.MethodPost, path: "/v1/unknown", status: http.StatusNotFound},
	} {
		req := httptest.NewRequest(tc.method, tc.path, nil)
		req.RemoteAddr = "127.0.0.1:3000"
		req.Header.Set("Authorization", "Bearer test-client-key")
		rec := httptest.NewRecorder()
		routes.Handle(context.Background(), enginehttp.NewResponseWriter(rec), req)
		if rec.Code != tc.status {
			t.Fatalf("%s %s status = %d, want %d; body=%s", tc.method, tc.path, rec.Code, tc.status, rec.Body.String())
		}
	}
}

func testConfig(dir string) config.Config {
	return config.Config{ListenAddr: "127.0.0.1:0", UsageStore: config.UsageStoreConfig{Path: filepath.Join(dir, "usage.duckdb"), MemoryLimit: "256MB", Threads: 2}, InteractionDir: filepath.Join(dir, "interactions"), InteractionRetention: 2, ClientAPIKeys: map[string]config.ClientAPIKey{"test-client": {ID: "test-client", APIKey: "test-client-key", Enabled: true}}, Providers: map[string]config.Provider{}, ModelCatalog: map[string]config.ModelInfo{}}
}
