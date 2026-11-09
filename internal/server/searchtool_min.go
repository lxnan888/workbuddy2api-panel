package server

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strings"

	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// searchTool POST /v1/search：OpenAI/agenttool 形状的联网搜索入口。
// 直接调用上游 /agenttool/v1/search，返回 {query, results, total_results}。
func (h *Handler) searchTool(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "read body: "+err.Error())
		return
	}
	var req struct {
		Query      string `json:"query"`
		Input      string `json:"input"`
		MaxResults int    `json:"max_results"`
		Count      int    `json:"count"`
		Model      string `json:"model"`
		Realm      string `json:"realm"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "invalid json: "+err.Error())
		return
	}
	query := strings.TrimSpace(req.Query)
	if query == "" {
		query = strings.TrimSpace(req.Input)
	}
	if query == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "query is required")
		return
	}
	maxResults := req.MaxResults
	if maxResults <= 0 {
		maxResults = req.Count
	}
	realm, _ := resolveModel(req.Model)
	if req.Realm != "" {
		realm = req.Realm
	}
	if realm != "global" {
		realm = "cn"
	}

	tried := map[string]bool{}
	var lastErr error
	for attempt := 0; attempt < searchAttempts; attempt++ {
		a := h.cfg.Pool.PickExcludingForRealm(tried, "", realm)
		if a == nil {
			break
		}
		tried[a.UID] = true
		if !h.cfg.Pool.Acquire(a.UID) {
			continue
		}
		out, err := h.cfg.Upstream.Search(r.Context(), a, query, maxResults, upstream.ChatMeta{})
		h.cfg.Pool.Release(a.UID)
		if err == nil {
			writeJSON(w, http.StatusOK, out)
			return
		}
		lastErr = err
		if requestLevelFailFastKind(errKindOf(err)) {
			break
		}
	}
	status := http.StatusBadGateway
	if len(tried) == 0 {
		status = http.StatusServiceUnavailable
	}
	msg := "search failed"
	if lastErr != nil {
		msg = lastErr.Error()
	}
	log.Printf("search realm=%s: %s", realm, msg)
	writeOpenAIError(w, status, "upstream_error", msg)
}

// errKindOf 提取 upstream.Error 的分类；非 upstream 错误返回 ErrNone。
func errKindOf(err error) upstream.ErrKind {
	if ue, ok := err.(*upstream.Error); ok {
		return ue.Kind
	}
	return upstream.ErrNone
}
