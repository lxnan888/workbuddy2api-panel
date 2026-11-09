// capacity_test.go 只读容量快照（/v1/capacity 的数据来源）。
package pool

import (
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

func TestCapacityForModelCountsHealthyAndModelCooled(t *testing.T) {
	p := New("")
	for _, uid := range []string{"available", "model-off", "paused", "cooling"} {
		p.Add(&auth.Auth{UID: uid})
	}
	p.BlockModelBackoff("model-off", "gpt-4", "11102 model not found")
	p.Pause("paused")
	p.Cooldown("cooling", CoolSoft, time.Hour, "test")

	model, all := p.CapacityForModel("gpt-4", "")
	if model != 1 || all != 2 {
		t.Fatalf("capacity=%d/%d want 1/2", model, all)
	}
	// 其他模型不受模型级冷却影响（model-off 仍可服务 other）。
	if model, all := p.CapacityForModel("other", ""); model != 2 || all != 2 {
		t.Fatalf("other capacity=%d/%d want 2/2", model, all)
	}
}

func TestCapacityForModelIgnoresInFlight(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetDefaultConcurrency(1)
	if !p.Acquire("u1") {
		t.Fatal("acquire failed")
	}
	t.Cleanup(func() { p.Release("u1") })

	if model, all := p.CapacityForModel("gpt-4", ""); model != 1 || all != 1 {
		t.Fatalf("capacity=%d/%d want 1/1（容量不按在途过滤）", model, all)
	}
}
