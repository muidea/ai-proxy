package admin

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"ai-proxy/internal/pkg/aiproxyconfig"
	"ai-proxy/internal/pkg/aiproxyusage"
)

const (
	maxUsageExportDays = 31
	maxUsageExportRows = 100000
)

func (h *Handler) observeUsageQuery(start time.Time, err error) {
	if h.metricsRegistry != nil {
		healthy := h.usageStore != nil && h.usageStore.Healthy()
		h.metricsRegistry.RecordUsageStoreQuery(time.Since(start), err, healthy)
	}
}

// UsageRuntime 可选:提供 usage store 查询能力。
type UsageRuntime interface {
	UsageStore() usage.Store
}

// WithUsageStore 挂接 usage store(可在 NewHandler 后调用)。
func (h *Handler) WithUsageStore(store usage.Store) *Handler {
	h.usageStore = store
	return h
}

// NewHandlerWithUsage 构造带 usage 查询能力的管理端。
func NewHandlerWithUsage(configPath string, runtime RuntimeConfig, store usage.Store) *Handler {
	return &Handler{configPath: configPath, runtime: runtime, usageStore: store}
}

func (h *Handler) usageAPI(w http.ResponseWriter, r *http.Request) {
	if h.usageStore == nil {
		writeError(w, http.StatusServiceUnavailable, "usage store unavailable")
		return
	}
	switch {
	case r.URL.Path == "/admin/api/usage/dashboard" && r.Method == http.MethodGet:
		h.usageDashboard(w, r)
	case r.URL.Path == "/admin/api/usage/events" && r.Method == http.MethodGet:
		h.usageEvents(w, r)
	case r.URL.Path == "/admin/api/usage/export.csv" && r.Method == http.MethodGet:
		h.usageExport(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (h *Handler) usageDashboard(w http.ResponseWriter, r *http.Request) {
	filter, err := parseUsageFilter(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	started := time.Now()
	dash, err := h.usageStore.Dashboard(ctx, filter)
	h.observeUsageQuery(started, err)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "usage query failed")
		return
	}
	// 标注 key 状态。
	cfg := h.runtime.ConfigSnapshot()
	for i := range dash.ByAPIKey {
		dash.ByAPIKey[i].Status = keyStatus(cfg, dash.ByAPIKey[i].APIKeyID)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"scope":      dash.Scope,
		"summary":    dash.Summary,
		"daily":      dash.Daily,
		"by_api_key": dash.ByAPIKey,
		"store": map[string]any{
			"engine":  "duckdb",
			"healthy": h.usageStore.Healthy(),
		},
	})
}

func (h *Handler) usageEvents(w http.ResponseWriter, r *http.Request) {
	filter, err := parseUsageFilter(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	ef := usage.EventFilter{UsageFilter: filter}
	if v := strings.TrimSpace(r.URL.Query().Get("page_size")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid page_size")
			return
		}
		ef.PageSize = n
	}
	ef.Cursor = strings.TrimSpace(r.URL.Query().Get("cursor"))
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	started := time.Now()
	page, err := h.usageStore.Events(ctx, ef)
	h.observeUsageQuery(started, err)
	if err != nil {
		if strings.Contains(err.Error(), "cursor") || strings.Contains(err.Error(), "page_size") {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeError(w, http.StatusServiceUnavailable, "usage query failed")
		return
	}
	writeJSON(w, http.StatusOK, page)
}

func (h *Handler) usageExport(w http.ResponseWriter, r *http.Request) {
	filter, err := parseUsageFilter(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if filter.AllTime || filter.To.Sub(filter.From) > maxUsageExportDays*24*time.Hour {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("export range must not exceed %d days", maxUsageExportDays))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	started := time.Now()
	count, err := h.usageStore.Count(ctx, filter)
	h.observeUsageQuery(started, err)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "usage query failed")
		return
	}
	if count > maxUsageExportRows {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("export exceeds maximum of %d rows", maxUsageExportRows))
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="usage-export.csv"`)
	w.Header().Set("Cache-Control", "no-store")
	started = time.Now()
	err = h.usageStore.ExportCSV(ctx, filter, w)
	h.observeUsageQuery(started, err)
	// CSV 流已开始时 HTTP 状态无法安全更改；预检已将数据库不可用和超限
	// 错误拦截在写 header 前，这里仅保留连接中断等不可恢复失败。
}

func parseUsageFilter(r *http.Request) (usage.UsageFilter, error) {
	q := r.URL.Query()
	f := usage.UsageFilter{
		APIKeyID: strings.TrimSpace(q.Get("api_key_id")),
		Provider: strings.TrimSpace(q.Get("provider")),
		Model:    strings.TrimSpace(q.Get("model")),
		Outcome:  strings.TrimSpace(q.Get("outcome")),
	}
	if v := strings.TrimSpace(q.Get("estimated")); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return f, fmt.Errorf("invalid estimated")
		}
		f.Estimated = &b
	}
	rangeKey := strings.TrimSpace(q.Get("range"))
	fromRaw := strings.TrimSpace(q.Get("from"))
	toRaw := strings.TrimSpace(q.Get("to"))
	now := time.Now().UTC()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)

	switch {
	case fromRaw != "" || toRaw != "":
		if fromRaw == "" || toRaw == "" {
			return f, fmt.Errorf("from and to are required together")
		}
		from, err := time.Parse(time.RFC3339, fromRaw)
		if err != nil {
			return f, fmt.Errorf("invalid from")
		}
		to, err := time.Parse(time.RFC3339, toRaw)
		if err != nil {
			return f, fmt.Errorf("invalid to")
		}
		f.From, f.To = from.UTC(), to.UTC()
	case rangeKey == "7d":
		f.From = today.AddDate(0, 0, -6)
		f.To = today.Add(24 * time.Hour)
	case rangeKey == "30d":
		f.From = today.AddDate(0, 0, -29)
		f.To = today.Add(24 * time.Hour)
	case rangeKey == "all" || rangeKey == "all_time":
		f.AllTime = true
	default:
		// 默认今日 UTC。
		f.From = today
		f.To = today.Add(24 * time.Hour)
	}
	if err := usage.ValidateUsageFilter(&f); err != nil {
		return f, err
	}
	return f, nil
}

func keyStatus(cfg config.Config, id string) string {
	if id == config.BuiltinDefaultAPIKeyID || id == "default" {
		return "builtin"
	}
	if entry, ok := cfg.ClientAPIKeys[id]; ok {
		if entry.Enabled {
			return "active"
		}
		return "disabled"
	}
	return "deleted"
}
