// busy_test.go /v2/chat/busy 与 /v1/capacity 的本地只读状态接口。
package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// seedModelCache 写入本地模型目录缓存（模拟一次已完成的 /v1/models 探测）。
func seedModelCache(t *testing.T, ids ...string) {
	t.Helper()
	infos := make([]upstream.ModelInfo, 0, len(ids))
	for _, id := range ids {
		infos = append(infos, upstream.ModelInfo{ID: id})
	}
	dynamicModelsCache.Lock()
	dynamicModelsCache.ids = infos
	dynamicModelsCache.fetched = time.Now()
	dynamicModelsCache.lastFail = time.Time{}
	dynamicModelsCache.Unlock()
	t.Cleanup(func() {
		dynamicModelsCache.Lock()
		dynamicModelsCache.ids = nil
		dynamicModelsCache.fetched = time.Time{}
		dynamicModelsCache.lastFail = time.Time{}
		dynamicModelsCache.Unlock()
	})
}

// TestChatBusyLevels 钉住 /v2/chat/busy 的三种取值，并确认全程不打上游：
// 模型级冷却 → busy；有号可服务 → idle；目录里没有 → unknown。
func TestChatBusyLevels(t *testing.T) {
	seedModelCache(t, "deepseek-v4.1-flash")
	calls := 0
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		calls++
		return 200, sseOK, true
	})
	p := testPoolWith(
		&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u2", AccessToken: "at2", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: up})

	post := func(body string) map[string]any {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", "/v2/chat/busy", strings.NewReader(body)))
		if rec.Code != http.StatusOK {
			t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
		}
		var resp map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("bad JSON: %v body=%s", err, rec.Body)
		}
		return resp
	}
	levels := func(resp map[string]any) map[string]string {
		out := map[string]string{}
		for _, it := range resp["data"].([]any) {
			m := it.(map[string]any)
			out[m["model"].(string)] = m["busy_level"].(string)
		}
		return out
	}

	resp := post(`{"models":["deepseek-v4.1-flash","no-such-model"]}`)
	if resp["code"].(float64) != 0 {
		t.Errorf("code=%v want 0", resp["code"])
	}
	got := levels(resp)
	if got["deepseek-v4.1-flash"] != busyLevelIdle {
		t.Errorf("可服务模型 busy_level=%q want idle", got["deepseek-v4.1-flash"])
	}
	if got["no-such-model"] != busyLevelUnknown {
		t.Errorf("未知模型 busy_level=%q want unknown", got["no-such-model"])
	}

	// 全池两个号都对同一模型模型级冷却 → busy（客户端据此打「繁忙」标记）。
	for _, uid := range []string{"u1", "u2"} {
		p.CooldownSoftForModel(uid, time.Minute, time.Now().Add(time.Hour), "deepseek-v4.1-flash", "6004 model rate limit")
	}
	got = levels(post(`{"models":["deepseek-v4.1-flash"]}`))
	if got["deepseek-v4.1-flash"] != busyLevelBusy {
		t.Errorf("全池模型级冷却时 busy_level=%q want busy", got["deepseek-v4.1-flash"])
	}

	// 空请求体：返回空 data，不 panic、不回错。
	resp = post(`{"models":[]}`)
	if data, ok := resp["data"].([]any); !ok || len(data) != 0 {
		t.Errorf("空 models 应返回空 data: %v", resp["data"])
	}
	if calls != 0 {
		t.Fatalf("busy 端点不应打上游, calls=%d", calls)
	}
}

// TestChatBusyHiddenModelIsUnknown hidden 模型不在本地可服务目录里。
func TestChatBusyRequiresServablePoolForIdle(t *testing.T) {
	seedModelCache(t, "deepseek-v4.1-flash")
	for _, tc := range []struct {
		name string
		pool *pool.Pool
		want string
	}{
		{name: "missing pool", want: busyLevelUnknown},
		{name: "no healthy account", pool: testPoolWith(&auth.Auth{UID: "disabled"}), want: busyLevelBusy},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.pool != nil {
				tc.pool.Disable("disabled", "test")
			}
			h := NewHandler(Config{Pool: tc.pool})
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest("POST", "/v2/chat/busy",
				strings.NewReader(`{"models":["deepseek-v4.1-flash"]}`)))
			if rec.Code != http.StatusOK {
				t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
			}
			var resp struct {
				Data []struct {
					BusyLevel string `json:"busy_level"`
				} `json:"data"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
				t.Fatal(err)
			}
			if len(resp.Data) != 1 || resp.Data[0].BusyLevel != tc.want {
				t.Fatalf("levels=%+v want %q", resp.Data, tc.want)
			}
		})
	}
}

func TestChatBusyHiddenModelIsUnknown(t *testing.T) {
	seedModelCache(t, "deepseek-v4.1-flash", "hidden-model")
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, ModelsHidden: []string{"cn:hidden-model"}})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v2/chat/busy",
		strings.NewReader(`{"models":["cn:hidden-model","cn:deepseek-v4.1-flash"]}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var resp struct {
		Data []struct {
			Model     string `json:"model"`
			BusyLevel string `json:"busy_level"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Data[0].BusyLevel != busyLevelUnknown {
		t.Errorf("hidden 模型 busy_level=%q want unknown", resp.Data[0].BusyLevel)
	}
	if resp.Data[1].BusyLevel != busyLevelIdle {
		t.Errorf("可见模型 busy_level=%q want idle", resp.Data[1].BusyLevel)
	}
}

// TestChatBusyBadJSON 非法 body → 400，不应 500。
func TestChatBusyBadJSON(t *testing.T) {
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true }),
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v2/chat/busy", strings.NewReader("{not json")))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d want 400 body=%s", rec.Code, rec.Body)
	}
}

// TestCapacityRequiresBearerKeyAndModel 鉴权与参数契约。
func TestCapacityRequiresBearerKeyAndModel(t *testing.T) {
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1"}), APIKey: "secret"})

	for _, tc := range []struct {
		name   string
		target string
		auth   string
		want   int
	}{
		{name: "missing key", target: "/v1/capacity?model=gpt-4", want: http.StatusUnauthorized},
		{name: "wrong key", target: "/v1/capacity?model=gpt-4", auth: "Bearer wrong", want: http.StatusUnauthorized},
		{name: "wrong scheme", target: "/v1/capacity?model=gpt-4", auth: "Basic secret", want: http.StatusUnauthorized},
		{name: "missing model", target: "/v1/capacity", auth: "Bearer secret", want: http.StatusBadRequest},
		{name: "blank model", target: "/v1/capacity?model=%20", auth: "Bearer secret", want: http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tc.target, nil)
			if tc.auth != "" {
				req.Header.Set("Authorization", tc.auth)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("status=%d want %d body=%s", rec.Code, tc.want, rec.Body)
			}
		})
	}
}

// TestCapacityRejectsUnconfiguredKey 未配置 api_key 时容量端点也拒绝，避免把池状态
// 暴露在一个默认不鉴权的端口上。
func TestCapacityRejectsUnconfiguredKey(t *testing.T) {
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1"})})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/capacity?model=gpt-4", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d want 401", rec.Code)
	}
}

// TestCapacityCountsHealthyModelAccountsWithoutInFlightFiltering 容量按账号健康 +
// 模型冷却 + 积分保底判定，不按在途过滤。
func TestCapacityCountsHealthyModelAccountsWithoutInFlightFiltering(t *testing.T) {
	p := testPoolWith(
		&auth.Auth{UID: "available"},
		&auth.Auth{UID: "model-off"},
		&auth.Auth{UID: "model-cooling"},
		&auth.Auth{UID: "paused"},
		&auth.Auth{UID: "disabled"},
		&auth.Auth{UID: "cooling"},
		&auth.Auth{UID: "in-flight-full"},
	)
	p.BlockModelBackoff("model-off", "gpt-4", "11102 model not found")
	p.CooldownSoftForModel("model-cooling", time.Minute, time.Now().Add(time.Hour), "gpt-4", "test")
	p.Pause("paused")
	p.Disable("disabled", "test")
	p.Cooldown("cooling", pool.CoolSoft, time.Hour, "test")
	p.SetDefaultConcurrency(1)
	if !p.Acquire("in-flight-full") {
		t.Fatal("expected in-flight slot to be acquired")
	}
	t.Cleanup(func() { p.Release("in-flight-full") })

	h := NewHandler(Config{Pool: p, APIKey: "secret"})
	req := httptest.NewRequest(http.MethodGet, "/v1/capacity?model=gpt-4", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want %d body=%s", rec.Code, http.StatusOK, rec.Body)
	}
	var got struct {
		Model                  string `json:"model"`
		ModelAvailableAccounts int    `json:"model_available_accounts"`
		AvailableAccounts      int    `json:"available_accounts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got.Model != "gpt-4" || got.ModelAvailableAccounts != 2 || got.AvailableAccounts != 4 {
		t.Fatalf("capacity=%+v want model=gpt-4 model_available_accounts=2 available_accounts=4", got)
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("capacity must not be stored in shared caches")
	}
}

// TestCapacityHonorsHiddenModels hidden 模型报零容量，别让客户端按容量选到必拒模型。
func TestCapacityHonorsHiddenModels(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1"})
	h := NewHandler(Config{Pool: p, APIKey: "secret", ModelsHidden: []string{"cn:hidden"}})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/capacity?model=cn:hidden", nil)
	req.Header.Set("Authorization", "Bearer secret")
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
	}
	var got struct {
		ModelAvailableAccounts int `json:"model_available_accounts"`
		AvailableAccounts      int `json:"available_accounts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.ModelAvailableAccounts != 0 || got.AvailableAccounts != 1 {
		t.Fatalf("hidden capacity=%+v want 0/1", got)
	}
}
