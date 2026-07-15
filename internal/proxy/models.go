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

// ModelsListResponse 是 GET/POST /v1/models 的具体外部协议 DTO。
// 禁止使用 map[string]any / []any 动态组装。
type ModelsListResponse struct {
	Object string        `json:"object"`
	Data   []ModelRecord `json:"data"`
}

// ModelRecord 是 catalog 中单个模型的稳定输出。
type ModelRecord struct {
	ID                  string   `json:"id"`
	Object              string   `json:"object"`
	Operations          []string `json:"operations"`
	ContextWindowTokens int      `json:"contextWindowTokens,omitempty"`
	MaxOutputTokens     int      `json:"maxOutputTokens,omitempty"`
}

// handleModels 返回本地 model_catalog 合成的 OpenAI-compatible 模型列表。
// 不转发上游;字段 contextWindowTokens / maxOutputTokens / operations 为扩展元数据。
// RouteOwner 仅用于内部路由、归档与观测，不作为客户端发现接口的一部分。
func (h *Handler) handleModels(w http.ResponseWriter, r *http.Request, requestID string) {
	start := time.Now()
	bodyBytes := []byte(nil)
	if r.Body != nil && r.Method == http.MethodPost {
		var err error
		bodyBytes, err = h.readLimitedBody(w, r)
		if err != nil {
			status := http.StatusBadRequest
			code := ErrorCodeInvalidRequest
			if isRequestTooLarge(err) {
				status = http.StatusRequestEntityTooLarge
				code = ErrorCodeRequestTooLarge
			}
			writeClientProtocolError(w, status, clientProtocolFromRequest(r), APIError{
				Code:           code,
				Message:        err.Error(),
				ClientProtocol: clientProtocolFromRequest(r),
				ClientEndpoint: NormalizeClientEndpoint(r.URL.Path),
				Operation:      OperationForPath(r.URL.Path),
			})
			return
		}
	}
	if r.Body != nil {
		_ = r.Body.Close()
	}
	round, err := h.startRound()
	if err != nil {
		writeClientProtocolError(w, http.StatusInternalServerError, clientProtocolFromRequest(r), APIError{
			Code: ErrorCodeProxyInternalError, Message: "start interaction archive failed",
			ClientProtocol: clientProtocolFromRequest(r),
			ClientEndpoint: NormalizeClientEndpoint(r.URL.Path), Operation: OperationForPath(r.URL.Path),
		})
		return
	}
	round.SetRequestID(requestID)
	if len(bodyBytes) > 0 {
		if err := round.WriteRequest(bodyBytes); err != nil {
			// best-effort
		}
	}
	h.archiveAndLogClientRequest(round, r, len(bodyBytes))

	payload := buildModelsListResponse(h.cfg.ModelCatalog)
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

func buildModelsListResponse(catalog map[string]config.ModelInfo) ModelsListResponse {
	items := config.CatalogModelsSorted(catalog)
	data := make([]ModelRecord, 0, len(items))
	for _, info := range items {
		operations := info.Operations
		if operations == nil {
			operations = []string{}
		} else {
			// 防御性拷贝,避免后续修改共享底层数组。
			operations = append([]string(nil), operations...)
		}
		rec := ModelRecord{
			ID:         info.ID,
			Object:     "model",
			Operations: operations,
		}
		if info.ContextWindowTokens > 0 {
			rec.ContextWindowTokens = info.ContextWindowTokens
		}
		if info.MaxOutputTokens > 0 {
			rec.MaxOutputTokens = info.MaxOutputTokens
		}
		data = append(data, rec)
	}
	return ModelsListResponse{
		Object: "list",
		Data:   data,
	}
}

// ReserveMetricsModels 为 metrics 预占 model label 槽位。
// 1) 各 provider 的精确 models(不含通配);
// 2) model_catalog 中已确定 RouteOwner 的 ID。
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
		info, ok := cfg.ModelCatalog[id]
		if !ok || strings.TrimSpace(info.RouteOwner) == "" {
			continue
		}
		reg.ReserveModels(info.RouteOwner, []string{id})
	}
}
