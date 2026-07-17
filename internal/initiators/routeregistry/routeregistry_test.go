package routeregistry

import (
	"context"
	"errors"
	"net"
	"testing"

	"ai-proxy/internal/pkg/aiproxybootstrap"
	"ai-proxy/internal/pkg/aiproxyconfig"
	"ai-proxy/internal/pkg/aiproxycontract"
)

type testAddr string

func (a testAddr) Network() string { return "tcp" }
func (a testAddr) String() string  { return string(a) }

type testListener struct{ closed bool }

func (l *testListener) Accept() (net.Conn, error) { return nil, errors.New("listener closed") }
func (l *testListener) Close() error              { l.closed = true; return nil }
func (l *testListener) Addr() net.Addr            { return testAddr("127.0.0.1:0") }

func TestSetupFailsWhenListenerCannotBind(t *testing.T) {
	oldListen := routeRegistryListen
	t.Cleanup(func() { routeRegistryListen = oldListen })
	aiproxybootstrap.Configure(aiproxycontract.Bootstrap{Config: config.Config{ListenAddr: "127.0.0.1:0"}})
	routeRegistryListen = func(string, string) (net.Listener, error) { return nil, errors.New("listen failed") }

	if err := New().Setup(context.Background(), nil, nil); err == nil {
		t.Fatal("expected listener setup error")
	}
}

func TestTeardownClosesHTTPListener(t *testing.T) {
	oldListen := routeRegistryListen
	t.Cleanup(func() { routeRegistryListen = oldListen })
	aiproxybootstrap.Configure(aiproxycontract.Bootstrap{Config: config.Config{ListenAddr: "127.0.0.1:0"}})
	listener := &testListener{}
	routeRegistryListen = func(string, string) (net.Listener, error) { return listener, nil }

	router := New()
	if err := router.Setup(context.Background(), nil, nil); err != nil {
		t.Fatal(err)
	}
	if router.GetRouteRegistry() == nil {
		t.Fatal("route registry was not initialized")
	}
	router.Teardown(context.Background())
	if !listener.closed {
		t.Fatal("expected listener to be closed")
	}
}
