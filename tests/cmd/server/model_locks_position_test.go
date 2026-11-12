package main

import "testing"

// 模型锁池卡片位置：默认 bottom（老配置缺字段零回归），只认 after_expiry，
// 其余非法值一律回落 bottom。
func TestModelLocksPositionConfig(t *testing.T) {
	cases := []struct {
		name, raw, want string
	}{
		{"缺字段默认原位置", `{}`, "bottom"},
		{"显式原位置", `{"panel":{"model_locks_position":"bottom"}}`, "bottom"},
		{"积分提醒下方", `{"panel":{"model_locks_position":"after_expiry"}}`, "after_expiry"},
		{"非法值回落", `{"panel":{"model_locks_position":"top"}}`, "bottom"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, err := ParseConfig([]byte(tc.raw))
			if err != nil {
				t.Fatalf("ParseConfig: %v", err)
			}
			if c.Panel.ModelLocksPosition != tc.want {
				t.Fatalf("got %q want %q", c.Panel.ModelLocksPosition, tc.want)
			}
		})
	}
}
