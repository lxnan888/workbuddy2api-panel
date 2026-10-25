package upstream

import "testing"

// TestNormalizeReasoningEffortNoPerModelDowngrade 2026-10-25 实测后收敛：
// 上游只校验枚举值、不按模型限制档位，因此 max 必须原样出站（此前会被
// supportedEfforts 削成 xhigh/high）。
func TestNormalizeReasoningEffortNoPerModelDowngrade(t *testing.T) {
	for _, eff := range []string{"minimal", "low", "medium", "high", "xhigh", "max"} {
		obj := map[string]any{"model": "glm-5.2", "reasoning_effort": eff}
		normalizeReasoningEffort(obj)
		if got := obj["reasoning_effort"]; got != eff {
			t.Errorf("%s 被改写为 %v，不应按模型目录降级", eff, got)
		}
	}
}

// TestNormalizeReasoningEffortInvalid 非法值（拼错 / off）换成安全默认档，
// 避免上游 400（国内 11133 / 国际 11-128）。
func TestNormalizeReasoningEffortInvalid(t *testing.T) {
	for _, bad := range []string{"off", "bogus123", " MAX2 ", ""} {
		obj := map[string]any{"model": "deepseek-v4.1-flash", "reasoning_effort": bad}
		normalizeReasoningEffort(obj)
		if got := obj["reasoning_effort"]; got != defaultEffortOnInvalid {
			t.Errorf("%q → %v，want %s", bad, got, defaultEffortOnInvalid)
		}
	}
}

// TestNormalizeReasoningEffortCamelAndCase camelCase 字段与大小写归一。
func TestNormalizeReasoningEffortCamelAndCase(t *testing.T) {
	obj := map[string]any{"model": "glm-5.2", "reasoningEffort": "MAX"}
	normalizeReasoningEffort(obj)
	if got := obj["reasoningEffort"]; got != "max" {
		t.Errorf("camelCase 大写档位 → %v，want max", got)
	}
	// 未携带字段：不注入新键。
	obj2 := map[string]any{"model": "glm-5.2"}
	normalizeReasoningEffort(obj2)
	if _, ok := obj2["reasoning_effort"]; ok {
		t.Error("未携带 reasoning_effort 时不应注入")
	}
}

// TestEffortListingMeasuredSet 目录只声明窄档位时，列表仍按实测全集输出。
func TestEffortListingMeasuredSet(t *testing.T) {
	got, def := EffortListing("cn", "glm-5.2", nil, "")
	if len(got) != len(MeasuredEfforts) {
		t.Fatalf("efforts=%v，want 实测全集 %v", got, MeasuredEfforts)
	}
	for i := range MeasuredEfforts {
		if got[i] != MeasuredEfforts[i] {
			t.Fatalf("efforts=%v，want %v", got, MeasuredEfforts)
		}
	}
	if def != "high" {
		t.Errorf("default=%q，want high", def)
	}

	// 远端默认档优先。
	if _, def := EffortListing("global", "deepseek-v4.1-flash", []string{"high"}, "max"); def != "max" {
		t.Errorf("远端 default 未生效：%q", def)
	}

	// 无任何推理证据 → 不输出（避免宣称不支持的档位）。
	if e, d := EffortListing("cn", "no-such-model", nil, ""); e != nil || d != "" {
		t.Errorf("未知模型应返回 nil，got %v/%q", e, d)
	}
}
