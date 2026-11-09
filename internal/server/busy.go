// busy.go 对外暴露官方客户端的「模型忙」预检端点与只读容量查询。
//
// POST /v2/chat/busy：官方客户端在每次对话前会调这个端点，body 形如
// {"models":["deepseek-v4.1-flash", ...]}，上游回
// {"code":0,"msg":"OK","data":[{"model":"...","busy_level":"idle|busy|unknown"}]}，
// 客户端据此给模型选择器打「繁忙」标记。网关本地按池状态算，不打上游：
//   - 上游国区可用、国际版 404，透传会让两个 realm 行为不一致；
//   - 网关自己就有权威的模型级冷却台账，比上游的 "unknown" 更准；
//   - 不引入额外上游调用，不消耗额度、不放大风控面。
//
// GET /v1/capacity：按请求模型返回本地池的可服务账号数，同样零上游调用。
package server

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/linguo2625469/workbuddy2api-panel/internal/session"
)

// busyLevel 取值与官方一致：idle / busy / unknown。
const (
	busyLevelIdle    = "idle"
	busyLevelBusy    = "busy"
	busyLevelUnknown = "unknown"
)

// chatBusy 处理 POST /v2/chat/busy。
//
// 语义：每个请求模型映射成 busy_level
//   - busy：该模型在本域所有可用账号上都处于模型级冷却（等同 chat 会走到
//     model_busy / model_unavailable 的那一档）；
//   - idle：还有账号能服务该模型；
//   - unknown：模型不在本网关的本地服务目录快照里（拼错/未适配/被隐藏/缓存未暖），
//     或请求为空。
//
// 未知模型回 unknown 而不是 idle：与官方口径一致，也不会让客户端误以为可以选。
func (h *Handler) chatBusy(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Models []string `json:"models"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "invalid JSON body: "+err.Error())
		return
	}
	out := make([]map[string]any, 0, len(req.Models))
	known := h.servableModelSet()
	for _, m := range req.Models {
		realm, bare := resolveModel(m)
		lvl := busyLevelUnknown
		if _, ok := known[realm+":"+bare]; ok && h.cfg.Pool != nil {
			if h.cfg.Pool.ModelBlockedForRealm(bare, realm).Blocked {
				lvl = busyLevelBusy
			} else if h.cfg.Pool.ServableForRealm(realm) {
				lvl = busyLevelIdle
			} else {
				lvl = busyLevelBusy
			}
		}

		out = append(out, map[string]any{"model": m, "busy_level": lvl})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"code":      0,
		"msg":       "OK",
		"requestId": session.NewMessageID(),
		"data":      out,
	})
}

// servableModelSet 返回网关当前对外提供的模型 id 集合（键为 "realm:裸名"）。
//
// **只读本地缓存快照**：不调用 modelList/fetchDynamicModels，不发起任何上游探测。
// 缓存为空（还没拉过/已过期）时回空集，调用方据此把模型判为 unknown——不编造
// 「这个模型我们有」。hidden 黑名单与 /v1/models 列表同口径过滤：被隐藏的模型
// 不该在 busy 预检里显示 idle。本地缓存目前只有 CN 目录，因此集合里只出现
// "cn:<模型>"；global 单独隐藏的模型也不会被误报为 idle。
func (h *Handler) servableModelSet() map[string]struct{} {
	live := h.loadLive()
	set := make(map[string]struct{})
	for _, mi := range cachedModelsSnapshot() {
		if mi.ID == "" {
			continue
		}
		realm, bare := resolveModel(mi.ID)
		if bare == "" {
			continue
		}
		if _, hide := live.HiddenModels[realm+":"+bare]; hide {
			continue
		}
		set[realm+":"+bare] = struct{}{}
	}
	return set
}

// capacity 处理 GET /v1/capacity?model=...
//
// 只读本地池状态：model_available_accounts 与选号口径一致（账号健康 + 该模型无
// 模型级冷却 + 未触积分保底），available_accounts 是全池健康账号数。不看在途、
// 不打上游，并禁止共享缓存（容量随账号冷却实时变化，缓存会误导客户端）。
func (h *Handler) capacity(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if strings.TrimSpace(h.loadLive().APIKey) == "" {
		writeOpenAIError(w, http.StatusUnauthorized, "authentication_error", "API key is required")
		return
	}
	model := strings.TrimSpace(r.URL.Query().Get("model"))
	if model == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "model is required")
		return
	}

	live := h.loadLive()
	realm, bareModel, _ := resolveModelDetailDefault(model, live.PreferredRealm)
	modelAvailable, poolAvailable := 0, 0
	if h.cfg.Pool != nil {
		modelAvailable, poolAvailable = h.cfg.Pool.CapacityForModel(bareModel, realm)
	}
	if _, hide := live.HiddenModels[realm+":"+bareModel]; hide {
		modelAvailable = 0
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"model":                    model,
		"model_available_accounts": modelAvailable,
		"available_accounts":       poolAvailable,
	})
}
