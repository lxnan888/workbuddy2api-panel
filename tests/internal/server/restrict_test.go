// restrict_test.go 11140 风控连坐保护的两道闸：
//  1. 命中 11140 就地停手（同一请求不再挨个换号去打）；
//  2. 15 分钟窗内 2 个不同账号被限制 → 全池暂停派发（期间不打上游，503 + Retry-After）。
package server

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/livecfg"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

const restrictBody = `{"code":11140,"msg":"账号存在异常行为，已被限制使用","requestId":"aaaabbbbccccdddd"}`

func postChatRestricted(h *Handler) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)))
	return rec
}

// TestUpstreamRestrictionDetection 中英文 11140 限制文书识别；模型级限流与
// 11-128 通道校验不得被误判。
func TestUpstreamRestrictionDetection(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"中文风控文书", `{"code":11140,"msg":"账号存在异常行为，已被限制使用"}`, true},
		{"英文 request illegal", `{"code":11140,"msg":"request illegal"}`, true},
		{"英文 unusual activity", `{"code":11140,"msg":"Your account shows unusual activity and has been restricted."}`, true},
		{"字符串 code", `{"code":"11140","msg":"账号存在异常行为"}`, true},
		{"同 code 的模型限流", `{"code":11140,"msg":"The model provider is rate-limiting requests."}`, false},
		{"只有文案没有 11140", `{"code":50400,"msg":"账号存在异常行为"}`, false},
		{"11-128 通道校验", `{"code":"11-128","msg":"request illegal"}`, false},
		{"纯文本", `账号存在异常行为，已被限制使用`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := upstream.IsRestricted(tc.body); got != tc.want {
				t.Fatalf("IsRestricted(%s) = %v, want %v", tc.body, got, tc.want)
			}
		})
	}
}

// TestRestrictionGateNeedsTwoAccounts 单号反复 11140 归账号自己的禁用，不触发全池
// 暂停；同一 uid 重复上报不许把计数刷成「两个号」。
func TestRestrictionGateNeedsTwoAccounts(t *testing.T) {
	var g restrictionGate
	if g.note("a", time.Minute) {
		t.Fatal("只有 1 个账号被限制时不该暂停全池")
	}
	if g.note("a", time.Minute) {
		t.Fatal("同一个账号重复命中不该凑成第二个号")
	}
	if d := g.remaining(); d != 0 {
		t.Fatalf("remaining=%v want 0", d)
	}
	if !g.note("b", time.Minute) {
		t.Fatal("窗口内第 2 个不同账号被限制应触发全池暂停")
	}
	if d := g.remaining(); d <= 0 || d > time.Minute {
		t.Fatalf("remaining=%v want (0, 1m]", d)
	}
	g.reset()
	if d := g.remaining(); d != 0 {
		t.Fatalf("reset 后 remaining=%v want 0", d)
	}
}

// TestRestrictionGateHoldDisabled hold=0（upstream.restriction_hold_seconds: 0）时
// 只保留「命中即停手」，永不暂停全池。
func TestRestrictionGateDoesNotExtendActiveHold(t *testing.T) {
	until := time.Now().Add(5 * time.Minute)
	g := restrictionGate{until: until, hits: map[string]time.Time{"old": time.Now()}}
	if g.note("new", time.Hour) {
		t.Fatal("an active hold must not be re-triggered")
	}
	if !g.until.Equal(until) {
		t.Fatalf("active hold moved from %v to %v", until, g.until)
	}
	if len(g.hits) != 1 || g.hits["old"].IsZero() || !g.hits["new"].IsZero() {
		t.Fatalf("active hold should ignore new wave hits, hits=%v", g.hits)
	}
}

func TestRestrictionGateHoldDisabled(t *testing.T) {
	var g restrictionGate
	for _, uid := range []string{"a", "b", "c"} {
		if g.note(uid, 0) {
			t.Fatal("hold=0 时不该暂停")
		}
	}
	if d := g.remaining(); d != 0 {
		t.Fatalf("remaining=%v want 0", d)
	}
}

// TestChatStopsRotationOnRestrictedAccount 端到端：上游一直回 11140 文书时，
// 每个请求只打一次上游（不轮转），第二个被限制的号把全池推进暂停，之后的请求
// 直接失败、根本不打上游。
func TestChatStopsRotationOnRestrictedAccount(t *testing.T) {
	calls := 0
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		calls++
		return 403, restrictBody, false
	})
	p := testPoolWith(
		&auth.Auth{UID: "r1", AccessToken: "at-r1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "r2", AccessToken: "at-r2", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: up, RestrictionHold: 30 * time.Minute})

	// 第 1 个请求：1 个号被限制 → 就地停手。
	if rec := postChatRestricted(h); rec.Code == 0 {
		t.Fatal("no response")
	}
	if calls != 1 {
		t.Fatalf("calls=%d want 1（命中 11140 不许换号继续打）", calls)
	}
	if d := h.restrict.remaining(); d != 0 {
		t.Fatalf("只有 1 个号被限制时不该暂停全池, remaining=%v", d)
	}

	// 第 2 个请求：另一个号也被限制 → 触发全池暂停。
	postChatRestricted(h)
	if calls != 2 {
		t.Fatalf("calls=%d want 2（每个请求各打一次）", calls)
	}
	if d := h.restrict.remaining(); d <= 0 {
		t.Fatalf("窗口内第 2 个号被限制应触发全池暂停, remaining=%v", d)
	}

	// 第 3 个请求：暂停期内快速失败，不打上游。
	rec := postChatRestricted(h)
	if calls != 2 {
		t.Fatalf("暂停期内不该再打上游, calls=%d", calls)
	}
	if rec.Code != 503 {
		t.Fatalf("code=%d want 503, body=%s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "account_restricted_hold") {
		t.Errorf("应给出 account_restricted_hold 业务码: %s", body)
	}
	if ra := rec.Header().Get("Retry-After"); ra == "" {
		t.Error("暂停期内应带 Retry-After，客户端才知道等多久")
	}
}

// TestChatRestrictionHoldZeroKeepsStopping 配置 hold=0 时仍然「命中即停手」：
// 只是不再全池暂停。少掉的这一点是运维主动选的，不能顺手把停手也关掉。
func TestChatRestrictionHoldZeroKeepsStopping(t *testing.T) {
	calls := 0
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		calls++
		return 403, restrictBody, false
	})
	p := testPoolWith(
		&auth.Auth{UID: "z1", AccessToken: "at-z1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "z2", AccessToken: "at-z2", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: up, RestrictionHold: 0})

	for i := 0; i < 2; i++ {
		postChatRestricted(h)
	}
	if calls != 2 {
		t.Fatalf("calls=%d want 2（每个请求仍只打一次）", calls)
	}
	if d := h.restrict.remaining(); d != 0 {
		t.Fatalf("hold=0 时不该暂停全池, remaining=%v", d)
	}
}

// TestChatEnglishRestrictionStopsRotation 英文 unusual activity 与中文文书同处置：
// 该号被禁用，同一请求不换号。
func TestChatEnglishRestrictionStopsRotation(t *testing.T) {
	calls := 0
	body := `{"code":11140,"msg":"Your account shows unusual activity and has been restricted."}`
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		calls++
		return 403, body, false
	})
	p := testPoolWith(
		&auth.Auth{UID: "e1", AccessToken: "at-e1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "e2", AccessToken: "at-e2", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: up, RestrictionHold: 30 * time.Minute})

	postChatRestricted(h)
	if calls != 1 {
		t.Fatalf("calls=%d want 1（英文限制文书同样就地停手）", calls)
	}
}

// TestRestrictionContinueDefaultKeepsProtection 默认（零值/Live 未开启）保护开启：
// 命中 11140 就地停手，与既有测试同口径。
func TestRestrictionContinueDefaultKeepsProtection(t *testing.T) {
	calls := 0
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		calls++
		return 403, restrictBody, false
	})
	p := testPoolWith(
		&auth.Auth{UID: "d1", AccessToken: "at-d1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "d2", AccessToken: "at-d2", ExpiresAt: 9999999999},
	)
	live := livecfg.New(livecfg.Snapshot{RestrictionHold: 30 * time.Minute})
	h := NewHandler(Config{Pool: p, Upstream: up, Live: live})
	postChatRestricted(h)
	if calls != 1 {
		t.Fatalf("默认保护应停手: calls=%d want 1", calls)
	}
	if st, _ := p.Status("d1"); !st.Disabled {
		t.Fatal("命中 11140 的号应被禁用")
	}
}

// TestRestrictionContinueRotatesAndDisables 开关打开：命中号仍禁用，但本请求
// 继续轮换试下一个号；上游第二个号成功 → 200。
func TestRestrictionContinueRotatesAndDisables(t *testing.T) {
	calls := map[string]int{}
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls[authz]++
		if authz == "Bearer at-bad" {
			return 403, restrictBody, false
		}
		return 200, sseOK, true
	})
	p := testPoolWith(
		&auth.Auth{UID: "bad", AccessToken: "at-bad", ExpiresAt: 9999999999},
		&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999},
	)
	p.SetRandomSource(func(n int64) int64 { return 0 }) // 确定性：先选 bad
	live := livecfg.New(livecfg.Snapshot{RestrictionHold: 30 * time.Minute, RestrictionContinue: true})
	h := NewHandler(Config{Pool: p, Upstream: up, Live: live})

	rec := postChatRestricted(h)
	if rec.Code != 200 {
		t.Fatalf("继续模式应由下一个号成功: code=%d body=%s", rec.Code, rec.Body)
	}
	if calls["Bearer at-bad"] != 1 || calls["Bearer at-good"] != 1 {
		t.Fatalf("calls=%v want bad 1 次、good 1 次（继续轮换）", calls)
	}
	if st, _ := p.Status("bad"); !st.Disabled {
		t.Fatal("继续模式下命中 11140 的号仍应禁用")
	}
	if d := h.restrict.remaining(); d != 0 {
		t.Fatalf("继续模式不该触发全池暂停, remaining=%v", d)
	}
}

// TestRestrictionContinueDisablesHoldGate 开关打开时暂停门整体停用：即使已经
// 处于暂停窗口内（保护模式留下的 until），也不再快速失败，正常派发。
func TestRestrictionContinueDisablesHoldGate(t *testing.T) {
	calls := 0
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		calls++
		return 200, sseOK, true
	})
	p := testPoolWith(&auth.Auth{UID: "u", AccessToken: "at-u", ExpiresAt: 9999999999})
	live := livecfg.New(livecfg.Snapshot{RestrictionHold: 30 * time.Minute, RestrictionContinue: true})
	h := NewHandler(Config{Pool: p, Upstream: up, Live: live})
	// 人为置一个仍在生效的暂停窗口（模拟保护模式刚触发）。
	h.restrict.until = time.Now().Add(30 * time.Minute)

	rec := postChatRestricted(h)
	if rec.Code != 200 {
		t.Fatalf("继续模式下暂停门应停用: code=%d body=%s", rec.Code, rec.Body)
	}
	if calls != 1 {
		t.Fatalf("calls=%d want 1（正常派发）", calls)
	}
}

// TestRestrictionContinueHotReload 同一 Handler 通过 livecfg 热切换开关：
// 保护模式命中 11140 停手，热切换为继续模式后，再遇到一个新的 11140 号时
// 继续轮换到健康号成功，无需重建 Handler/重启。
func TestRestrictionContinueHotReload(t *testing.T) {
	calls := map[string]int{}
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls[authz]++
		if authz == "Bearer at-h1" || authz == "Bearer at-h4" {
			return 403, restrictBody, false
		}
		return 200, sseOK, true
	})
	p := testPoolWith(
		&auth.Auth{UID: "h1", AccessToken: "at-h1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "h2", AccessToken: "at-h2", ExpiresAt: 9999999999},
	)
	p.SetRandomSource(func(n int64) int64 { return 0 }) // 确定性：先选 h1
	live := livecfg.New(livecfg.Snapshot{RestrictionHold: 30 * time.Minute})
	h := NewHandler(Config{Pool: p, Upstream: up, Live: live})

	postChatRestricted(h) // 保护模式：h1 命中 → 停手 + 禁用
	if len(calls) != 1 || calls["Bearer at-h1"] != 1 {
		t.Fatalf("保护模式 calls=%v want 仅 h1 一次", calls)
	}
	if st, _ := p.Status("h1"); !st.Disabled {
		t.Fatal("h1 应被禁用")
	}

	// 热切换：加入一个「会 11140 的新坏号 h4」和健康号 h5；两者提权，
	// 保证下次请求先撞 h4，验证开关确实变成「继续」。
	live.Store(livecfg.Snapshot{RestrictionHold: 30 * time.Minute, RestrictionContinue: true})
	p.Add(&auth.Auth{UID: "h4", AccessToken: "at-h4", ExpiresAt: 9999999999})
	p.Add(&auth.Auth{UID: "h5", AccessToken: "at-h5", ExpiresAt: 9999999999})
	p.SetAccountPriority("h4", 10)
	p.SetAccountPriority("h5", 10)
	calls = map[string]int{}
	rec := postChatRestricted(h)
	if rec.Code != 200 {
		t.Fatalf("热切换后应继续轮换到健康号: code=%d body=%s", rec.Code, rec.Body)
	}
	if calls["Bearer at-h4"] != 1 || calls["Bearer at-h5"] != 1 {
		t.Fatalf("热切换后 calls=%v want h4 命中一次 + h5 承接一次", calls)
	}
	if st, _ := p.Status("h4"); !st.Disabled {
		t.Fatal("继续模式下 h4 仍应被禁用")
	}
}
