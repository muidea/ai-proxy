package metrics

import (
	"bufio"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestStreamHandlerSendsInitialSnapshot(t *testing.T) {
	r := NewRegistry()
	r.RecordRequest("openai", "gpt-4", "chat_completions", 200, 50*time.Millisecond, "success")
	h := StreamHandler(r, StreamHandlerOptions{AllowRemote: true, Interval: 50 * time.Millisecond})

	srv := httptest.NewServer(h)
	defer srv.Close()

	req, err := http.NewRequest(http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := req.Context()
	type result struct {
		stats *StatsJSON
		err   error
	}
	done := make(chan result, 1)
	go func() {
		client := &http.Client{Timeout: 500 * time.Millisecond}
		resp, err := client.Do(req)
		if err != nil {
			done <- result{nil, err}
			return
		}
		defer resp.Body.Close()
		if got := resp.Header.Get("Content-Type"); !strings.HasPrefix(got, "text/event-stream") {
			done <- result{nil, errStreamContentType{got}}
			return
		}
		reader := bufio.NewReader(resp.Body)
		// 立即推送的首条消息应当带 "data: " 前缀且为合法 JSON。
		_ = ctx
		line, err := reader.ReadString('\n')
		if err != nil {
			done <- result{nil, err}
			return
		}
		if !strings.HasPrefix(line, "data: ") {
			done <- result{nil, errStreamPrefix{line}}
			return
		}
		var snap StatsJSON
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &snap); err != nil {
			done <- result{nil, err}
			return
		}
		done <- result{&snap, nil}
	}()

	select {
	case res := <-done:
		if res.err != nil {
			t.Fatalf("stream read: %v", res.err)
		}
		if res.stats.Requests.Total != 1 {
			t.Fatalf("stats.total = %d, want 1", res.stats.Requests.Total)
		}
	case <-time.After(1 * time.Second):
		t.Fatalf("stream did not deliver first event in time")
	}
}

func TestStreamHandlerRejectsRemoteWhenLoopbackOnly(t *testing.T) {
	r := NewRegistry()
	h := StreamHandler(r, StreamHandlerOptions{AllowRemote: false})
	req := httptest.NewRequest(http.MethodGet, "/stats/stream", nil)
	req.RemoteAddr = "10.0.0.1:51234"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
}

type errStreamContentType struct{ got string }

func (e errStreamContentType) Error() string { return "unexpected content-type: " + e.got }

type errStreamPrefix struct{ line string }

func (e errStreamPrefix) Error() string { return "unexpected line: " + e.line }
