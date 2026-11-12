package pool

import (
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// 零积分硬保护：已确认归零的号不再打收费/未知倍率模型，避免 402 → 硬冷却
// 到次日 04:00（用户看到的「降级/暂停」）。
func TestNoCreditBlockedForPaidAndUnknownModels(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.SetCostExploreInterval(0)
	p.SetRandomSource(func(n int64) int64 { return 0 })
	p.Add(&auth.Auth{UID: "empty"})
	p.Add(&auth.Auth{UID: "rich"})
	p.SetCredits("empty", 0, 0) // SetCredits 标记「已观测」
	p.SetCredits("rich", 500, 0)

	for _, model := range []string{"paid-model", "unknown-model"} {
		// 目录未注入倍率（未知）+ 实测收费两种形态都不该选中 empty。
		a := p.PickExcludingForRealm(nil, model, "")
		if a == nil || a.UID != "rich" {
			t.Fatalf("model=%s pick=%+v want rich（零积分号应跳过）", model, a)
		}
	}

	p.NoteModelCost("empty", "paid-model", 0, 1000) // 实测免费
	a := p.PickExcludingForRealm(nil, "free-model", "")
	if a == nil {
		t.Fatal("免费模型应仍有候选")
	}
}

// 目录明确免费（倍率 0）时零积分号照常可选；目录收费时跳过。
func TestNoCreditHonorsCatalogFreeRate(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.SetCostExploreInterval(0)
	p.SetRandomSource(func(n int64) int64 { return 0 })
	p.Add(&auth.Auth{UID: "empty"})
	p.SetCredits("empty", 0, 0)
	p.SetModelRateOf(func(realm, model string) string {
		if model == "free" {
			return "0.00"
		}
		return "1.62"
	})

	if a := p.PickExcludingForRealm(nil, "free", ""); a == nil || a.UID != "empty" {
		t.Fatalf("免费模型应放行零积分号，got %+v", a)
	}
	if a := p.PickExcludingForRealm(nil, "paid", ""); a != nil {
		t.Fatalf("收费模型应跳过零积分号，got %+v", a)
	}
}

// 新号/存量 state 未观测过余额时 credits=0 只是缺省值，不能当确认归零拦掉。
func TestNoCreditUnknownBalanceStaysSelectable(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.SetCostExploreInterval(0)
	p.SetRandomSource(func(n int64) int64 { return 0 })
	p.Add(&auth.Auth{UID: "fresh"}) // 从未 SetCredits

	a := p.PickExcludingForRealm(nil, "any-model", "")
	if a == nil || a.UID != "fresh" {
		t.Fatalf("余额未观测的新号应照常可选，got %+v", a)
	}
}

// 零积分号签到回血后自动恢复；优先级高也不能越过零积分硬保护。
func TestNoCreditRecoversAndCannotBeOverriddenByPriority(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.SetCostExploreInterval(0)
	p.SetRandomSource(func(n int64) int64 { return 0 })
	p.Add(&auth.Auth{UID: "empty"})
	p.Add(&auth.Auth{UID: "normal"})
	p.SetCredits("empty", 0, 0)
	p.SetCredits("normal", 200, 0)
	p.SetAccountPriority("empty", 999)

	if a := p.PickExcludingForRealm(nil, "paid-model", ""); a == nil || a.UID != "normal" {
		t.Fatalf("零积分硬保护优先于优先级，got %+v", a)
	}
	p.SetCredits("empty", 300, 0) // 签到回血
	if a := p.PickExcludingForRealm(nil, "paid-model", ""); a == nil || a.UID != "empty" {
		t.Fatalf("回血后高优先级号应恢复中选，got %+v", a)
	}
}

// 粘性命中零积分号也跳过（付费模型），避免钉在 402 上。
func TestNoCreditSkipsStickyForPaidModel(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.Add(&auth.Auth{UID: "empty"})
	p.SetCredits("empty", 0, 0)
	if a := p.PickByUIDForModel("empty", "paid-model"); a != nil {
		t.Fatalf("粘性零积分号应跳过付费模型，got %+v", a)
	}
	if a := p.PickByUID("empty"); a == nil {
		t.Fatal("无模型上下文的直取不受零积分保护影响")
	}
}
