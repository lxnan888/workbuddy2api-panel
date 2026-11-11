package pool

import (
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// priorityPool 两个账号：high 显式提权，low 用超高积分/快过期等诱饵验证优先级最高。
func priorityPool(t *testing.T) (*Pool, string, string) {
	t.Helper()
	withNoPickGap(t)
	p := New("")
	p.Add(&auth.Auth{UID: "low"})
	p.Add(&auth.Auth{UID: "high"})
	p.SetCredits("low", 1_000_000, 0)
	p.SetCredits("high", 1, 0)
	return p, "low", "high"
}

func mustPickUID(t *testing.T, p *Pool, want, why string) {
	t.Helper()
	got := p.Pick()
	if got == nil || got.UID != want {
		t.Fatalf("%s: pick=%+v want %s", why, got, want)
	}
}

// 优先级高于积分/权重：低积分高优先级号必须压过高积分低优先级号。
func TestPriorityBeatsCredits(t *testing.T) {
	p, _, high := priorityPool(t)
	p.SetAccountPriority(high, 10)
	p.SetRandomSource(func(n int64) int64 { return 0 })
	for i := 0; i < 20; i++ {
		mustPickUID(t, p, high, "priority must beat credits")
	}
}

// 同优先级仍走原加权随机：高积分账号在 r=0（最高权重）时应被选中。
func TestPriorityTieKeepsWeightedRandom(t *testing.T) {
	p, low, high := priorityPool(t)
	p.SetAccountPriority(low, 5)
	p.SetAccountPriority(high, 5)
	p.SetRandomSource(func(n int64) int64 { return 0 })
	mustPickUID(t, p, low, "same priority must keep weighted random")
}

// 优先级高于快过期偏好：低优先级号即使有快过期积分也不能插队。
func TestPriorityBeatsExpiringPreference(t *testing.T) {
	p, low, high := priorityPool(t)
	p.SetAccountPriority(high, 1)
	p.SetCreditsDetailed(low, 1000, 1000, 1000, time.Now().Add(time.Hour), 1000)
	p.SetRandomSource(func(n int64) int64 { return 0 })
	mustPickUID(t, p, high, "priority must beat expiring preference")
}

// 优先级高于成本分层：低优先级号即使实测免费，也不能压过高优先级收费号。
func TestPriorityBeatsCostTier(t *testing.T) {
	p, low, high := priorityPool(t)
	p.SetAccountPriority(high, 1)
	p.SetCredits(low, 1000, 0)
	p.SetCredits(high, 1000, 0)
	p.NoteModelCost(low, "m", 0, 1000)   // 实测免费（tier 0）
	p.NoteModelCost(high, "m", 10, 1000) // 实测收费（tier 2）
	p.SetRandomSource(func(n int64) int64 { return 0 })
	got := p.PickExcludingForModel(nil, "m")
	if got == nil || got.UID != high {
		t.Fatalf("priority must beat cost tier: pick=%+v want %s", got, high)
	}
}

// 高优先级号并发满/冷却时立即降级，不等待。
func TestPriorityFallsBackWhenHighUnavailable(t *testing.T) {
	p, low, high := priorityPool(t)
	p.SetAccountPriority(high, 10)
	p.SetDefaultConcurrency(1)
	if !p.Acquire(high) {
		t.Fatal("acquire high")
	}
	mustPickUID(t, p, low, "concurrency-full high must fall back immediately")
	p.Release(high)

	p.Cooldown(high, CoolSoft, time.Hour, "429")
	mustPickUID(t, p, low, "cooling high must fall back immediately")
}

// 兜底路径同样先按优先级：两个号都在冷却时优先选高优先级号，即使低优先级号更早恢复。
func TestPriorityFallbackChoosesHighFirst(t *testing.T) {
	p, low, high := priorityPool(t)
	p.SetAccountPriority(high, 10)
	p.Cooldown(low, CoolSoft, 10*time.Minute, "429")
	p.Cooldown(high, CoolSoft, time.Hour, "429")
	mustPickUID(t, p, high, "fallback must respect priority before earliest expiry")
}

// 跨域裸名：高优先级在另一域时跨域选它；同优先级时首选域优先。
func TestPriorityCrossRealm(t *testing.T) {
	p := realmPool(t)
	p.SetAccountPriority("g1", 5)
	if got := p.PickExcludingForModelRealms(nil, "", "cn"); got == nil || got.UID != "g1" {
		t.Fatalf("cross-realm priority: pick=%+v want g1", got)
	}
	// 显式 cn 前缀仍锁域，不受 global 高优先级影响。
	if got := p.PickExcludingForRealm(nil, "", "cn"); got == nil || realmOf(got) != "cn" {
		t.Fatalf("explicit cn must stay cn: pick=%+v", got)
	}
	// 全部 0：首选域 cn 优先。
	p.SetAccountPriority("g1", 0)
	for i := 0; i < 30; i++ {
		got := p.PickExcludingForModelRealms(nil, "", "cn")
		if got == nil || realmOf(got) != "cn" {
			t.Fatalf("equal priority must prefer cn: pick=%+v", got)
		}
	}
}

// 粘性优先：已绑定的低优先级账号不被高优先级账号抢占。
func TestPriorityDoesNotPreemptSticky(t *testing.T) {
	p, low, high := priorityPool(t)
	p.SetAccountPriority(high, 10)
	if got := p.PickByUIDForModel(low, ""); got == nil || got.UID != low {
		t.Fatalf("sticky must win over priority: pick=%+v want %s", got, low)
	}
	if got := p.PickByUID(low); got == nil || got.UID != low {
		t.Fatalf("sticky must win over priority: pick=%+v want %s", got, low)
	}
}

// 有序策略：优先级组内才走轮询；高优先级组被 tried 排除后才用低优先级。
func TestPriorityWithRoundRobin(t *testing.T) {
	p := New("")
	for _, uid := range []string{"a", "b", "c"} {
		p.Add(&auth.Auth{UID: uid})
		p.SetCredits(uid, 100, 0)
	}
	if err := p.SetScheduling(
		RegionStrategies{Domestic: RoundRobin, International: RoundRobin},
		RegionAccountOrder{Domestic: []string{"a", "b", "c"}},
	); err != nil {
		t.Fatal(err)
	}
	p.SetAccountPriority("a", 9)
	for i := 0; i < 5; i++ {
		if got := p.PickExcludingForRealm(nil, "", "cn"); got == nil || got.UID != "a" {
			t.Fatalf("round robin must stay in top priority group: pick=%+v", got)
		}
	}
	got := p.PickExcludingForRealm(map[string]bool{"a": true}, "", "cn")
	if got == nil || got.UID == "a" {
		t.Fatalf("tried high-priority account must fall back to next group: pick=%+v", got)
	}
}

// 优先级持久化：Set 后 Flush，重开池仍能读回。
func TestPriorityPersistence(t *testing.T) {
	stateFile := filepath.Join(t.TempDir(), "state.json")
	p := New(stateFile)
	p.Add(&auth.Auth{UID: "u1"})
	if !p.SetAccountPriority("u1", 7) {
		t.Fatal("set priority")
	}
	p.Flush()
	p.Close()

	p2 := New(stateFile)
	defer p2.Close()
	p2.SyncToDir([]*auth.Auth{{UID: "u1"}})
	if got, ok := p2.AccountPriority("u1"); !ok || got != 7 {
		t.Fatalf("priority not persisted: got=%d ok=%v", got, ok)
	}
	if st, ok := p2.Status("u1"); !ok || st.Priority != 7 {
		t.Fatalf("status priority=%d ok=%v", st.Priority, ok)
	}
}

// 新粘性会话分配只取最高优先级组；校验列表保留低优先级已绑定账号。
func TestPriorityStickyAllocation(t *testing.T) {
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })
	p := New("")
	p.Add(&auth.Auth{UID: "low"})
	p.Add(&auth.Auth{UID: "high"})
	p.Add(&auth.Auth{UID: "g1", Domain: "www.workbuddy.ai"})
	p.SetAccountPriority("high", 5)
	p.SetAccountPriority("g1", 6) // 另一域更高优先级：新分配应跨域选它
	alloc := p.WeightedAvailableUIDsForModelPriority("", "cn", true)
	if len(alloc) != 1 || alloc[0] != "g1" {
		t.Fatalf("alloc=%v want [g1] (highest priority across realms)", alloc)
	}
	// 校验列表不按优先级过滤：low 仍然有效（粘性优先）；裸名跨域时 global 绑定
	// 也应继续有效（不会被只查 cn 的旧逻辑误判失效）。
	valid := p.AvailableUIDsForModelRealms("", "cn", true)
	foundLow, foundGlobal := false, false
	for _, uid := range valid {
		switch uid {
		case "low":
			foundLow = true
		case "g1":
			foundGlobal = true
		}
	}
	if !foundLow || !foundGlobal {
		t.Fatalf("validation list must keep low-priority and cross-realm bindings: %v", valid)
	}
	// 显式 cn 前缀不跨域：即使 g1 优先级更高也不能分配。
	alloc = p.WeightedAvailableUIDsForModelPriority("", "cn", false)
	if len(alloc) != 1 || alloc[0] != "high" {
		t.Fatalf("explicit cn alloc=%v want [high]", alloc)
	}
}

// 并发读写下优先级不产生数据竞争（-race 时有效）。
func TestPriorityConcurrentSet(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(2)
		go func(n int) { defer wg.Done(); p.SetAccountPriority("u1", n) }(i)
		go func() { defer wg.Done(); _, _ = p.AccountPriority("u1") }()
	}
	wg.Wait()
}
