package admin

import (
	"context"
	"fmt"
	"net/http"
	"sort"
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
	case r.URL.Path == "/admin/api/usage/filter-options" && r.Method == http.MethodGet:
		h.usageFilterOptions(w, r)
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

func (h *Handler) usageFilterOptions(w http.ResponseWriter, r *http.Request) {
	// 仅解析时间窗；忽略维度筛选参数，保持选项列表在改筛选项时稳定。
	filter, err := parseUsageFilterTimeOnly(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	cfg := h.runtime.ConfigSnapshot()

	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	started := time.Now()
	usageRes, usageErr := h.usageStore.FilterOptions(ctx, usage.FilterOptionsQuery{
		From:    filter.From,
		To:      filter.To,
		AllTime: filter.AllTime,
	})
	h.observeUsageQuery(started, usageErr)

	usageOK := usageErr == nil
	if !usageOK {
		usageRes = usage.FilterOptionsResult{}
	}

	// scope：优先用 store 解析后的有效窗口。
	scopeFrom, scopeTo := filter.From, filter.To
	if usageOK {
		if !usageRes.From.IsZero() {
			scopeFrom = usageRes.From
		}
		if !usageRes.To.IsZero() {
			scopeTo = usageRes.To
		}
	} else if filter.AllTime {
		// store 失败时仍给出 all-time facet 的约定窗口。
		if from, to, err := usage.ResolveFilterOptionsRange(usage.FilterOptionsQuery{AllTime: true}); err == nil {
			scopeFrom, scopeTo = from, to
		}
	}

	apiKeys, providers, models := mergeFilterOptions(cfg, usageRes)
	writeJSON(w, http.StatusOK, map[string]any{
		"scope": map[string]any{
			"from":     scopeFrom,
			"to":       scopeTo,
			"timezone": "UTC",
			"all_time": filter.AllTime,
		},
		"api_key_ids": apiKeys,
		"providers":   providers,
		"models":      models,
		"outcomes":    usage.KnownOutcomes(),
		"limits": map[string]any{
			"max_per_dimension": usage.MaxFilterOptionValues,
			"truncated": map[string]bool{
				"api_key_ids": usageRes.Truncated.APIKeyIDs,
				"providers":   usageRes.Truncated.Providers,
				"models":      usageRes.Truncated.Models,
			},
		},
		"store": map[string]any{
			"engine":         "duckdb",
			"healthy":        h.usageStore.Healthy(),
			"usage_query_ok": usageOK,
		},
	})
}

// parseUsageFilterTimeOnly 只解析时间窗相关 query，清空维度字段。
func parseUsageFilterTimeOnly(r *http.Request) (usage.UsageFilter, error) {
	f, err := parseUsageFilter(r)
	if err != nil {
		return f, err
	}
	f.APIKeyID = ""
	f.Provider = ""
	f.Model = ""
	f.Outcome = ""
	f.Estimated = nil
	return f, nil
}

type filterOptionItem struct {
	ID       string `json:"id"`
	Status   string `json:"status,omitempty"`
	InConfig bool   `json:"in_config"`
	InUsage  bool   `json:"in_usage"`
}

func mergeFilterOptions(cfg config.Config, usageRes usage.FilterOptionsResult) (apiKeys, providers, models []filterOptionItem) {
	keyMap := map[string]*filterOptionItem{}
	ensureKey := func(id string) *filterOptionItem {
		if o, ok := keyMap[id]; ok {
			return o
		}
		o := &filterOptionItem{ID: id}
		keyMap[id] = o
		return o
	}
	// 配置中的 client keys。
	for id := range cfg.ClientAPIKeys {
		o := ensureKey(id)
		o.InConfig = true
		o.Status = keyStatus(cfg, id)
	}
	// 内置 default 始终出现。
	def := ensureKey(config.BuiltinDefaultAPIKeyID)
	if def.Status == "" {
		def.Status = "builtin"
	}
	for _, id := range usageRes.APIKeyIDs {
		if id == "" {
			continue
		}
		o := ensureKey(id)
		o.InUsage = true
		if o.Status == "" {
			o.Status = keyStatus(cfg, id)
		}
	}
	apiKeys = make([]filterOptionItem, 0, len(keyMap))
	for _, o := range keyMap {
		apiKeys = append(apiKeys, *o)
	}
	sort.Slice(apiKeys, func(i, j int) bool { return apiKeys[i].ID < apiKeys[j].ID })

	provMap := map[string]*filterOptionItem{}
	ensureProv := func(id string) *filterOptionItem {
		if o, ok := provMap[id]; ok {
			return o
		}
		o := &filterOptionItem{ID: id}
		provMap[id] = o
		return o
	}
	for name := range cfg.Providers {
		if name == "" {
			continue
		}
		ensureProv(name).InConfig = true
	}
	for _, id := range usageRes.Providers {
		if id == "" {
			continue
		}
		ensureProv(id).InUsage = true
	}
	providers = make([]filterOptionItem, 0, len(provMap))
	for _, o := range provMap {
		providers = append(providers, *o)
	}
	sort.Slice(providers, func(i, j int) bool { return providers[i].ID < providers[j].ID })

	modelMap := map[string]*filterOptionItem{}
	ensureModel := func(id string) *filterOptionItem {
		if o, ok := modelMap[id]; ok {
			return o
		}
		o := &filterOptionItem{ID: id}
		modelMap[id] = o
		return o
	}
	for id, info := range cfg.ModelCatalog {
		mid := id
		if info.ID != "" {
			mid = info.ID
		}
		if mid == "" {
			continue
		}
		ensureModel(mid).InConfig = true
	}
	for _, id := range usageRes.Models {
		if id == "" {
			continue
		}
		ensureModel(id).InUsage = true
	}
	models = make([]filterOptionItem, 0, len(modelMap))
	for _, o := range modelMap {
		models = append(models, *o)
	}
	sort.Slice(models, func(i, j int) bool { return models[i].ID < models[j].ID })
	return apiKeys, providers, models
}
