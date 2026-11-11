package session

import (
	"testing"
	"time"
)

// 已绑定的低优先级账号继续有效（粘性优先）；只有解绑/失效后的新分配才用
// AllocForModel 的最高优先级列表。
func TestResolveForModelKeepsStickyOverPriority(t *testing.T) {
	r := New(Config{
		TTL:               time.Minute,
		AvailableForModel: func(string) []string { return []string{"low"} },
		AllocForModel:     func(string) []string { return []string{"high"} },
	})
	r.Bind("conv-1", "low")

	uid, ok := r.ResolveForModel("conv-1", "m")
	if !ok || uid != "low" {
		t.Fatalf("sticky binding must win over priority: uid=%q ok=%v", uid, ok)
	}

	r.Unbind("conv-1")
	uid, ok = r.ResolveForModel("conv-1", "m")
	if !ok || uid != "high" {
		t.Fatalf("new binding must use priority alloc list: uid=%q ok=%v", uid, ok)
	}
}

// AllocForModel 未注入时回落 AvailableForModel（旧行为零回归）。
func TestResolveForModelAllocFallback(t *testing.T) {
	r := New(Config{
		TTL:               time.Minute,
		AvailableForModel: func(string) []string { return []string{"only"} },
	})
	uid, ok := r.ResolveForModel("conv-2", "m")
	if !ok || uid != "only" {
		t.Fatalf("alloc fallback: uid=%q ok=%v", uid, ok)
	}
}

// 绑定号暂不可用：保留绑定、不续期 TTL；恢复后自动回到原绑定。
func TestResolveForModelPreservesUnavailableBinding(t *testing.T) {
	available := []string{"a"}
	r := New(Config{
		TTL:               time.Minute,
		AvailableForModel: func(string) []string { return available },
		AllocForModel:     func(string) []string { return []string{"b"} },
	})
	r.Bind("conv-3", "a")
	r.mu.RLock()
	before := r.entries["conv-3"].lastActive
	r.mu.RUnlock()

	available = []string{"b"} // a 冷却/并发满 → 不可用
	uid, ok, preserved := r.ResolveForModelState("conv-3", "m")
	if uid != "a" || ok || !preserved {
		t.Fatalf("preserved resolve: uid=%q ok=%v preserved=%v", uid, ok, preserved)
	}
	r.mu.RLock()
	after := r.entries["conv-3"].lastActive
	r.mu.RUnlock()
	if !after.Equal(before) {
		t.Fatal("preserved binding must not refresh TTL")
	}
	if r.Count() != 1 {
		t.Fatalf("binding should be kept, count=%d", r.Count())
	}

	available = []string{"a", "b"} // 原号恢复
	uid, ok, preserved = r.ResolveForModelState("conv-3", "m")
	if uid != "a" || !ok || preserved {
		t.Fatalf("recovered resolve: uid=%q ok=%v preserved=%v", uid, ok, preserved)
	}
}

// 绑定已过期：删除旧绑定并按新会话重新分配，不会保留故障绑定。
func TestResolveForModelExpiredBindingReallocates(t *testing.T) {
	r := New(Config{
		TTL:               time.Minute,
		AvailableForModel: func(string) []string { return []string{"b"} },
		AllocForModel:     func(string) []string { return []string{"b"} },
	})
	r.Bind("conv-4", "a")
	r.mu.Lock()
	r.entries["conv-4"] = entry{uid: "a", lastActive: time.Now().Add(-2 * time.Minute)}
	r.mu.Unlock()

	uid, ok, preserved := r.ResolveForModelState("conv-4", "m")
	if uid != "b" || !ok || preserved {
		t.Fatalf("expired binding should reallocate: uid=%q ok=%v preserved=%v", uid, ok, preserved)
	}
	if r.Count() != 1 {
		t.Fatalf("expired binding should be replaced, count=%d", r.Count())
	}
}

// 有序策略的 lookup 同样保留暂不可用绑定，不删绑。
func TestLookupForModelStatePreservesUnavailableBinding(t *testing.T) {
	available := []string{"a"}
	r := New(Config{
		TTL:               time.Minute,
		AvailableForModel: func(string) []string { return available },
	})
	r.Bind("conv-5", "a")
	available = []string{"b"}
	uid, ok, preserved := r.LookupForModelState("conv-5", "m")
	if uid != "a" || ok || !preserved {
		t.Fatalf("lookup preserved: uid=%q ok=%v preserved=%v", uid, ok, preserved)
	}
	if r.Count() != 1 {
		t.Fatalf("binding should be kept, count=%d", r.Count())
	}
	available = []string{"a"}
	uid, ok, preserved = r.LookupForModelState("conv-5", "m")
	if uid != "a" || !ok || preserved {
		t.Fatalf("lookup recovered: uid=%q ok=%v preserved=%v", uid, ok, preserved)
	}
}
