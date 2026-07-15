package proxy

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"time"

	"ai-proxy/internal/config"
	"ai-proxy/internal/metrics"
)

// handleModels 返回本地 model_catalog 合成的 OpenAI-compatible 模型列表。
// 不转发上游;字段 contextWindowTokens / maxOutputTokens 为扩展元数据。
func (h *Handler) handleModels(w http.ResponseWriter, r *http.Request, requestID string) {
	start := time.Now()
	bodyBytes := []byte(nil)
	if r.Body != nil && r.Method == http.MethodPost {
		var err error
		bodyBytes, err = h.readLimitedBody(w, r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusRequestEntityTooLarge)
			return
		}
	}
	if r.Body != nil {
		_ = r.Body.Close()
	}
	round, err := h.startRound()
	if err != nil {
		http.Error(w, "start interaction archive failed", http.StatusInternalServerError)
		return
	}
	round.SetRequestID(requestID)
	if len(bodyBytes) > 0 {
		if err := round.WriteRequest(bodyBytes); err != nil {
			// best-effort
		}
	}
	h.archiveAndLogClientRequest(round, r, len(bodyBytes))

	payload := buildModelsListResponse(h.cfg.ModelCatalog, h.cfg.Providers)
	body, err := json.Marshal(payload)
	if err != nil {
		h.writeArchivedError(w, round, r, start, "", "", false, http.StatusInternalServerError, err.Error())
		return
	}
	body = append(body, '\n')

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
	if err := round.WriteResponse("response.json", body); err != nil {
		// best-effort archive
	}
	duration := time.Since(start)
	h.recordAndPrint(round, r, "", "", false, http.StatusOK, duration, tokenUsage{}, "")
	h.writeArchiveMetadata(round, "", "", false, http.StatusOK, duration, tokenUsage{}, "response.json", "", "", "success")
}

func buildModelsListResponse(catalog map[string]config.ModelInfo, providers map[string]config.Provider) map[string]any {
	items := make([]config.ModelInfo, 0, len(catalog))
	for _, info := range catalog {
		if strings.TrimSpace(info.ID) == "" {
			continue
		}
		items = append(items, info)
	}
	sort.Slice(items, func(i, j int) bool {
		return strings.ToLower(items[i].ID) < strings.ToLower(items[j].ID)
	})

	data := make([]any, 0, len(items))
	for _, info := range items {
		entry := map[string]any{
			"id":       info.ID,
			"object":   "model",
			"created":  0,
			"owned_by": ownedByForModel(info.ID, providers),
		}
		if info.ContextWindowTokens > 0 {
			entry["contextWindowTokens"] = info.ContextWindowTokens
		}
		if info.MaxOutputTokens > 0 {
			entry["maxOutputTokens"] = info.MaxOutputTokens
		}
		data = append(data, entry)
	}
	return map[string]any{
		"object": "list",
		"data":   data,
	}
}

func ownedByForModel(modelID string, providers map[string]config.Provider) string {
	// model 严格区分大小写,与 providers.*.models 原文匹配。
	modelID = strings.TrimSpace(modelID)
	if modelID == "" {
		return "ai-proxy"
	}
	matches := make([]string, 0, 1)
	for name, provider := range providers {
		if provider.Disabled {
			continue
		}
		if providerMatchesModel(name, provider, modelID) {
			matches = append(matches, name)
		}
	}
	if len(matches) == 1 {
		return matches[0]
	}
	return "ai-proxy"
}

// ReserveMetricsModels 为 metrics 预占 model label 槽位。
// 1) 各 provider 的精确 models(不含通配);
// 2) model_catalog 中仅唯一匹配某一 provider 的 ID(排序后稳定)。
func ReserveMetricsModels(reg *metrics.Registry, cfg config.Config) {
	if reg == nil {
		return
	}
	for name, provider := range cfg.Providers {
		if provider.Disabled {
			continue
		}
		reg.ReserveModels(name, provider.Models)
	}
	catalogIDs := make([]string, 0, len(cfg.ModelCatalog))
	for _, info := range cfg.ModelCatalog {
		if id := strings.TrimSpace(info.ID); id != "" {
			catalogIDs = append(catalogIDs, id)
		}
	}
	sort.Strings(catalogIDs)
	for _, id := range catalogIDs {
		matches := make([]string, 0, 1)
		for name, provider := range cfg.Providers {
			if provider.Disabled {
				continue
			}
			if providerMatchesModel(name, provider, id) {
				matches = append(matches, name)
			}
		}
		if len(matches) == 1 {
			reg.ReserveModels(matches[0], []string{id})
		}
	}
}
