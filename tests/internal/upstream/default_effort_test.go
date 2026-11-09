package upstream

import "testing"

// TestEnsureDefaultEffort 客户端没写 effort 时补默认档：
// glm-5.1 这类「只思考」模型不带 effort 会被上游拒（11133），带默认档才 200。
func TestEnsureDefaultEffort(t *testing.T) {
	cases := []struct {
		name    string
		body    map[string]any
		model   string
		want    string
		wantSet bool
	}{
		{"静态表单档兜底", map[string]any{"messages": []any{}}, "glm-5.1", "medium", true},
		{"远端单档优先", map[string]any{"messages": []any{}}, "glm-5.1", "low", true},
		{"多档模型不注入", map[string]any{"messages": []any{}}, "glm-5.2", "", false},
		{"已有 camel 不覆盖", map[string]any{"reasoningEffort": "max"}, "glm-5.1", "", false},
		{"已有 snake 不覆盖", map[string]any{"reasoning_effort": "max"}, "glm-5.1", "max", true},
		{"未知模型不注入", map[string]any{"messages": []any{}}, "no-such-model", "", false},
		{"显式关闭思考不注入", map[string]any{"thinking": map[string]any{"type": "disabled"}}, "glm-5.1", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			efforts := map[string][]string{}
			if c.name == "远端单档优先" {
				efforts["glm-5.1"] = []string{"low"}
			}
			ensureDefaultEffort(c.body, c.model, efforts)
			got, ok := c.body["reasoning_effort"].(string)
			if c.name == "已有 camel 不覆盖" {
				if _, snake := c.body["reasoning_effort"]; snake {
					t.Error("已有 camel 字段时不应再注入 snake 字段")
				}
				return
			}
			if ok != c.wantSet {
				t.Fatalf("reasoning_effort present=%v want %v (body=%v)", ok, c.wantSet, c.body)
			}
			if ok && got != c.want {
				t.Errorf("reasoning_effort=%q want %q", got, c.want)
			}
		})
	}
}
