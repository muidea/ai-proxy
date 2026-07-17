package observability

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"ai-proxy/internal/pkg/aiproxymetrics"
)

func TestHandlerAccessPolicyAndHead(t *testing.T) {
	registry := metrics.NewRegistry()
	handler := Handler(registry, HandlerOptions{AllowRemote: true, AllowedCIDRs: []string{"10.0.0.0/8"}})

	for _, tc := range []struct {
		name       string
		method     string
		remoteAddr string
		status     int
		bodyEmpty  bool
	}{
		{name: "loopback", method: http.MethodGet, remoteAddr: "127.0.0.1:3000", status: http.StatusOK},
		{name: "allowed cidr", method: http.MethodGet, remoteAddr: "10.1.2.3:3000", status: http.StatusOK},
		{name: "denied cidr", method: http.MethodGet, remoteAddr: "192.168.1.1:3000", status: http.StatusForbidden},
		{name: "head", method: http.MethodHead, remoteAddr: "127.0.0.1:3000", status: http.StatusOK, bodyEmpty: true},
		{name: "method", method: http.MethodPost, remoteAddr: "127.0.0.1:3000", status: http.StatusMethodNotAllowed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, "/metrics", nil)
			req.RemoteAddr = tc.remoteAddr
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d", rec.Code, tc.status)
			}
			if tc.bodyEmpty && rec.Body.Len() != 0 {
				t.Fatalf("HEAD response should not have a body")
			}
		})
	}
}
