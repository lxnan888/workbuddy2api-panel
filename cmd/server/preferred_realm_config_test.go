// preferred_realm_config_test.go pool.preferred_realm / models.bare_ids 配置测试。
package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestPreferredRealmDefault 键缺席 → 国内优先（cn），这是本 fork 的默认真实路由域。
func TestPreferredRealmDefault(t *testing.T) {
	c := Default()
	if err := c.normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if c.Pool.PreferredRealm != "cn" {
		t.Errorf("preferred_realm 缺省 = %q，必须为 cn", c.Pool.PreferredRealm)
	}
}

// TestPreferredRealmNormalize 非法值/大小写/空串一律回落 cn，global 原样保留。
func TestPreferredRealmNormalize(t *testing.T) {
	cases := map[string]string{
		"":         "cn",
		"cn":       "cn",
		"global":   "global",
		"GLOBAL":   "global",
		" Global ": "global",
		"intl":     "cn",
	}
	for in, want := range cases {
		c := Default()
		c.Pool.PreferredRealm = in
		if err := c.normalize(); err != nil {
			t.Fatalf("normalize(%q): %v", in, err)
		}
		if c.Pool.PreferredRealm != want {
			t.Errorf("preferred_realm %q → %q，want %q", in, c.Pool.PreferredRealm, want)
		}
	}
}

// TestBareIDsDefaultOff / 文件覆盖为 true。
func TestBareIDsConfig(t *testing.T) {
	c := Default()
	if err := c.normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if c.Models.BareIDs {
		t.Error("models.bare_ids 缺省必须为 false（保持带域前缀的现状）")
	}

	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	if err := os.WriteFile(fp, []byte(`{"models":{"bare_ids":true},"pool":{"preferred_realm":"global"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Models.BareIDs {
		t.Error("models.bare_ids=true 未生效")
	}
	if got.Pool.PreferredRealm != "global" {
		t.Errorf("preferred_realm = %q，want global", got.Pool.PreferredRealm)
	}
}
