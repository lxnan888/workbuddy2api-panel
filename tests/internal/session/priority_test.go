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
