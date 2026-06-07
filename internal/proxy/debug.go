package proxy

import (
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"

	"ai-proxy/internal/archive"
	"ai-proxy/internal/config"
)

type requestDebugInfo struct {
	RoundID       int                 `json:"round_id"`
	ReceivedAt    time.Time           `json:"received_at"`
	Method        string              `json:"method"`
	Path          string              `json:"path"`
	RawQuery      string              `json:"raw_query,omitempty"`
	RequestURI    string              `json:"request_uri"`
	Host          string              `json:"host"`
	RemoteAddr    string              `json:"remote_addr"`
	UserAgent     string              `json:"user_agent,omitempty"`
	ContentLength int64               `json:"content_length"`
	BodyBytes     int                 `json:"body_bytes"`
	Headers       map[string][]string `json:"headers"`
	BodyPath      string              `json:"body_path"`
}

type upstreamDebugInfo struct {
	RoundID   int                 `json:"round_id"`
	At        time.Time           `json:"at"`
	Provider  string              `json:"provider"`
	Protocol  string              `json:"protocol"`
	Method    string              `json:"method"`
	URL       string              `json:"url"`
	BodyBytes int                 `json:"body_bytes"`
	Headers   map[string][]string `json:"headers"`
}

type upstreamResponseDebugInfo struct {
	RoundID       int       `json:"round_id"`
	At            time.Time `json:"at"`
	Provider      string    `json:"provider"`
	Protocol      string    `json:"protocol"`
	Status        int       `json:"status"`
	DurationMS    int64     `json:"duration_ms"`
	ContentType   string    `json:"content_type,omitempty"`
	ContentLength int64     `json:"content_length"`
	Error         string    `json:"error,omitempty"`
}

type fallbackAttemptDebugInfo struct {
	Provider   string `json:"provider"`
	Protocol   string `json:"protocol"`
	Status     int    `json:"status,omitempty"`
	Error      string `json:"error,omitempty"`
	Fallback   bool   `json:"fallback"`
	DurationMS int64  `json:"duration_ms"`
}

func (h *Handler) debugf(format string, args ...any) {
	if h.cfg.DebugLog {
		log.Printf("[debug] "+format, args...)
	}
}

func (h *Handler) archiveAndLogClientRequest(round *archive.Round, r *http.Request, bodyBytes int) {
	if round == nil {
		return
	}
	info := requestDebugInfo{
		RoundID:       round.ID,
		ReceivedAt:    round.StartedAt,
		Method:        r.Method,
		Path:          r.URL.Path,
		RawQuery:      r.URL.RawQuery,
		RequestURI:    r.RequestURI,
		Host:          r.Host,
		RemoteAddr:    r.RemoteAddr,
		UserAgent:     r.UserAgent(),
		ContentLength: r.ContentLength,
		BodyBytes:     bodyBytes,
		Headers:       sanitizeHeaders(r.Header),
		BodyPath:      "request.json",
	}
	if err := round.WriteJSON("request.meta.json", info); err != nil {
		log.Printf("archive request metadata: %v", err)
	}
	h.debugf("round=%06d client request method=%s path=%s query=%q remote=%s host=%s user_agent=%q body_bytes=%d headers=%s",
		round.ID,
		info.Method,
		info.Path,
		info.RawQuery,
		info.RemoteAddr,
		info.Host,
		info.UserAgent,
		bodyBytes,
		headerSummary(info.Headers),
	)
}

func (h *Handler) archiveAndLogProviderSelection(round *archive.Round, providerName string, provider config.Provider, model string, stream bool) {
	if round == nil {
		return
	}
	h.debugf("round=%06d selected provider=%s protocol=%s model=%s stream=%t base_url=%s",
		round.ID,
		providerName,
		provider.Protocol,
		model,
		stream,
		provider.BaseURL,
	)
}

func (h *Handler) archiveAndLogUpstreamRequest(round *archive.Round, providerName string, provider config.Provider, req *http.Request, bodyBytes int) {
	if round == nil || req == nil {
		return
	}
	info := upstreamDebugInfo{
		RoundID:   round.ID,
		At:        time.Now(),
		Provider:  providerName,
		Protocol:  provider.Protocol,
		Method:    req.Method,
		URL:       req.URL.String(),
		BodyBytes: bodyBytes,
		Headers:   sanitizeHeaders(req.Header),
	}
	if err := round.WriteJSON("upstream_request.json", info); err != nil {
		log.Printf("archive upstream request metadata: %v", err)
	}
	h.debugf("round=%06d upstream request provider=%s protocol=%s method=%s url=%s body_bytes=%d headers=%s",
		round.ID,
		providerName,
		provider.Protocol,
		req.Method,
		req.URL.String(),
		bodyBytes,
		headerSummary(info.Headers),
	)
}

func (h *Handler) archiveAndLogUpstreamResponse(round *archive.Round, providerName string, provider config.Provider, resp *http.Response, duration time.Duration, err error) {
	if round == nil {
		return
	}
	info := upstreamResponseDebugInfo{
		RoundID:    round.ID,
		At:         time.Now(),
		Provider:   providerName,
		Protocol:   provider.Protocol,
		DurationMS: duration.Milliseconds(),
	}
	if resp != nil {
		info.Status = resp.StatusCode
		info.ContentType = resp.Header.Get("Content-Type")
		info.ContentLength = resp.ContentLength
	}
	if err != nil {
		info.Error = err.Error()
	}
	if writeErr := round.WriteJSON("upstream_response.json", info); writeErr != nil {
		log.Printf("archive upstream response metadata: %v", writeErr)
	}
	h.debugf("round=%06d upstream response provider=%s protocol=%s status=%d duration=%s content_type=%q content_length=%d error=%q",
		round.ID,
		providerName,
		provider.Protocol,
		info.Status,
		duration.Truncate(time.Millisecond),
		info.ContentType,
		info.ContentLength,
		info.Error,
	)
	h.logUpstreamAlert(round, providerName, provider.Protocol, info.Status, duration, info.Error, false, "")
}

func (h *Handler) archiveAndLogFallbackAttempts(round *archive.Round, attempts []fallbackAttemptDebugInfo) {
	if round == nil || len(attempts) == 0 {
		return
	}
	if err := round.WriteJSON("fallback_attempts.json", attempts); err != nil {
		log.Printf("archive fallback attempts: %v", err)
	}
}

func (h *Handler) logUpstreamAlert(round *archive.Round, providerName, protocol string, status int, duration time.Duration, errMessage string, fallback bool, nextProvider string) {
	if !h.cfg.DebugLog {
		return
	}
	if status < 400 && errMessage == "" {
		return
	}
	roundID := 0
	if round != nil {
		roundID = round.ID
	}
	level := "WARN"
	color := "\033[33m"
	if errMessage != "" || status >= 500 {
		level = "ERROR"
		color = "\033[31m"
	}
	reset := "\033[0m"
	message := errMessage
	if message == "" {
		message = http.StatusText(status)
	}
	suffix := ""
	if fallback {
		suffix = fmt.Sprintf(" fallback=true next=%s", nextProvider)
	}
	log.Printf("%s[ai-proxy][UPSTREAM %s] round=%06d provider=%s protocol=%s status=%d duration=%s message=%q%s%s",
		color,
		level,
		roundID,
		providerName,
		protocol,
		status,
		duration.Truncate(time.Millisecond),
		message,
		suffix,
		reset,
	)
}

func sanitizeHeaders(headers http.Header) map[string][]string {
	sanitized := make(map[string][]string, len(headers))
	for key, values := range headers {
		canonical := http.CanonicalHeaderKey(key)
		if isSensitiveHeader(canonical) {
			sanitized[canonical] = []string{"<redacted>"}
			continue
		}
		copied := make([]string, len(values))
		copy(copied, values)
		sanitized[canonical] = copied
	}
	return sanitized
}

func isSensitiveHeader(key string) bool {
	key = strings.ToLower(key)
	return key == "authorization" ||
		key == "proxy-authorization" ||
		key == "x-api-key" ||
		key == "api-key" ||
		key == "cookie" ||
		key == "set-cookie"
}

func headerSummary(headers map[string][]string) string {
	if len(headers) == 0 {
		return "{}"
	}
	keys := make([]string, 0, len(headers))
	for key := range headers {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		values := headers[key]
		if len(values) == 0 {
			parts = append(parts, key+"=")
			continue
		}
		parts = append(parts, key+"="+strings.Join(values, "|"))
	}
	return "{" + strings.Join(parts, ", ") + "}"
}
