package server

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/livecfg"
)

// TestBareIDsRejectsPrefixedModel 裸 ID 模式下对外命名空间只有裸名：
// 带 cn:/global: 前缀的请求按「模型不存在」拒绝，且不消耗上游调用。
func TestBareIDsRejectsPrefixedModel(t *testing.T) {
	var calls int
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		calls++
		return 200, sseOK, true
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	live := livecfg.New(livecfg.Snapshot{BareModelIDs: true, PreferredRealm: "cn"})
	h := NewHandler(Config{Pool: p, Upstream: up, Live: live})

	for _, model := range []string{"cn:glm-5.2", "global:glm-5.2"} {
		calls = 0
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/v1/chat/completions",
			strings.NewReader(`{"model":"`+model+`","messages":[{"role":"user","content":"hi"}]}`))
		h.ServeHTTP(rec, req)
		if rec.Code != 400 {
			t.Errorf("%s: code=%d，want 400", model, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "model_unavailable") {
			t.Errorf("%s: body=%s，want model_unavailable", model, rec.Body.String())
		}
		if calls != 0 {
			t.Errorf("%s: 不应打上游，calls=%d", model, calls)
		}
	}
}

// TestBareIDsAcceptsBareModel 同一开关下裸模型名照常放行。
func TestBareIDsAcceptsBareModel(t *testing.T) {
	var calls int
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		calls++
		return 200, sseOK, true
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	live := livecfg.New(livecfg.Snapshot{BareModelIDs: true, PreferredRealm: "cn"})
	h := NewHandler(Config{Pool: p, Upstream: up, Live: live})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`))
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s，want 200", rec.Code, rec.Body.String())
	}
	if calls != 1 {
		t.Errorf("calls=%d，want 1", calls)
	}
}
