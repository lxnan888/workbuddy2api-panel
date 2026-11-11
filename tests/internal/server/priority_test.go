package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/livecfg"
)

// 裸模型名跨域：高优先级账号在另一域时，优先用另一域的高优先级号。
func TestBareModelPriorityPicksHigherRealm(t *testing.T) {
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })
	calls := map[string]int{}
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls[authz]++
		return 200, sseOK, true
	})
	p := newRealmPool(t)
	p.SetCrossDomainFallback(true)
	p.SetAccountPriority("gl1", 5)
	h := NewHandler(Config{Pool: p, Upstream: up})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"deepseek-v4.1-flash-sg","messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if calls["Bearer at-gl"] != 1 || calls["Bearer at-cn"] != 0 {
		t.Fatalf("calls=%v want global 1 次、cn 0 次（global 优先级更高）", calls)
	}
}

// 同优先级：首选域 cn 优先，不能因为 global 号恰好权重高就跨域。
func TestBareModelEqualPriorityPrefersPreferredRealm(t *testing.T) {
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })
	calls := map[string]int{}
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls[authz]++
		return 200, sseOK, true
	})
	p := newRealmPool(t)
	p.SetCrossDomainFallback(true)
	h := NewHandler(Config{Pool: p, Upstream: up})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"deepseek-v4.1-flash-sg","messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if calls["Bearer at-cn"] != 1 || calls["Bearer at-gl"] != 0 {
		t.Fatalf("calls=%v want cn 1 次、global 0 次（同优先级首选域优先）", calls)
	}
}

// 显式 cn: 前缀锁域：即使 global 号优先级更高也不跨域。
func TestExplicitRealmIgnoresCrossRealmPriority(t *testing.T) {
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })
	calls := map[string]int{}
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls[authz]++
		return 200, sseOK, true
	})
	p := newRealmPool(t)
	p.SetCrossDomainFallback(true)
	p.SetAccountPriority("gl1", 5)
	h := NewHandler(Config{Pool: p, Upstream: up})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"cn:deepseek-v4.1-flash-sg","messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if calls["Bearer at-cn"] != 1 || calls["Bearer at-gl"] != 0 {
		t.Fatalf("calls=%v want cn 1 次、global 0 次（显式前缀锁域）", calls)
	}
}

// preferred_realm=global 时，裸名同优先级应优先 global；但 cn 号优先级更高时仍跨域选 cn。
func TestBareModelPriorityHonorsPreferredRealm(t *testing.T) {
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })
	run := func(priorityCN int, wantToken string) {
		t.Helper()
		calls := map[string]int{}
		up := newFakeUpstream(t, func(authz string) (int, string, bool) {
			calls[authz]++
			return 200, sseOK, true
		})
		p := newRealmPool(t)
		p.SetCrossDomainFallback(true)
		if priorityCN > 0 {
			p.SetAccountPriority("cn1", priorityCN)
		}
		h := NewHandler(Config{
			Pool:     p,
			Upstream: up,
			Live:     livecfg.New(livecfg.Snapshot{PreferredRealm: "global"}),
		})
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
			strings.NewReader(`{"model":"deepseek-v4.1-flash-sg","messages":[{"role":"user","content":"hi"}]}`)))
		if rec.Code != http.StatusOK {
			t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
		}
		if calls[wantToken] != 1 {
			t.Fatalf("calls=%v want %s 1 次", calls, wantToken)
		}
	}
	// 同优先级：首选域 global 优先。
	run(0, "Bearer at-gl")
	// cn 号优先级更高：跨域选 cn。
	run(5, "Bearer at-cn")
}
