package server

import "testing"

// TestResolveModelDetailDefault 裸名走调用方给的首选域；显式前缀永远优先，
// 且 realmExplicit 必须为 true（跨域回退据此判定，写错会让显式锁域失效）。
func TestResolveModelDetailDefault(t *testing.T) {
	cases := []struct {
		model, def, wantRealm, wantBare string
		wantExplicit                    bool
	}{
		{"glm-5.2", "cn", "cn", "glm-5.2", false},
		{"glm-5.2", "global", "global", "glm-5.2", false},
		{"glm-5.2", "", "cn", "glm-5.2", false},      // 空首选域回落 cn
		{"glm-5.2", "bogus", "cn", "glm-5.2", false}, // 非法值回落 cn
		{"cn:glm-5.2", "global", "cn", "glm-5.2", true},
		{"global:glm-5.2", "cn", "global", "glm-5.2", true},
		{"a:b", "global", "global", "a:b", false}, // 非 realm 前缀视为裸名
	}
	for _, c := range cases {
		realm, bare, explicit := resolveModelDetailDefault(c.model, c.def)
		if realm != c.wantRealm || bare != c.wantBare || explicit != c.wantExplicit {
			t.Errorf("resolveModelDetailDefault(%q,%q) = (%q,%q,%v)，want (%q,%q,%v)",
				c.model, c.def, realm, bare, explicit, c.wantRealm, c.wantBare, c.wantExplicit)
		}
	}
}

// TestResolveModelDetailLegacy 旧入口（无首选域参数）必须仍是 cn，零回归。
func TestResolveModelDetailLegacy(t *testing.T) {
	realm, bare, explicit := resolveModelDetail("glm-5.2")
	if realm != "cn" || bare != "glm-5.2" || explicit {
		t.Errorf("legacy resolveModelDetail = (%q,%q,%v)", realm, bare, explicit)
	}
}
