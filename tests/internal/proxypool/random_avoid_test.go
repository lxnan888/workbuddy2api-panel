package proxypool

import (
	"path/filepath"
	"testing"
)

// 随机出口优先避开被账号级 fixed 绑定的代理；解绑后重新可随机命中。
func TestRandomAvoidsAccountBoundProxies(t *testing.T) {
	path := filepath.Join(t.TempDir(), "proxy-pool.json")
	m := testManager(t, path)
	a := addProxy(t, m, "http://127.0.0.1:1001")
	b := addProxy(t, m, "http://127.0.0.1:1002")
	c := addProxy(t, m, "http://127.0.0.1:1003")
	m.health[a] = health{status: "healthy"}
	m.health[b] = health{status: "healthy"}
	m.health[c] = health{status: "healthy"}
	// a、b 被两个账号专属绑定，只剩 c 是空闲代理。
	if err := m.Bind("u1", Binding{Mode: "fixed", ProxyID: a}); err != nil {
		t.Fatal(err)
	}
	if err := m.Bind("u2", Binding{Mode: "fixed", ProxyID: b}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 50; i++ {
		sel, err := m.selectLocked("random-account", "cn")
		if err != nil {
			t.Fatalf("select: %v", err)
		}
		if !sel.proxied || sel.node.ID != c {
			t.Fatalf("iter %d: random picked %q (want only unbound %q)", i, sel.node.ID, c)
		}
	}
	// 解绑 u1 后 a 也进入随机池；多次抽样应能覆盖 a 和 c。
	if err := m.Unbind("u1"); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		sel, err := m.selectLocked("random-account", "cn")
		if err != nil {
			t.Fatal(err)
		}
		seen[sel.node.ID] = true
	}
	if !seen[a] || !seen[c] || seen[b] {
		t.Fatalf("after unbind seen=%v want a+c but not b", seen)
	}
}

// 全部健康代理都被绑定时：fallback（默认）回退全池随机，strict 走兜底不撞绑定代理。
func TestRandomNoFreeProxyModes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "proxy-pool.json")
	m := testManager(t, path)
	a := addProxy(t, m, "http://127.0.0.1:2001")
	m.health[a] = health{status: "healthy"}
	if err := m.Bind("u1", Binding{Mode: "fixed", ProxyID: a}); err != nil {
		t.Fatal(err)
	}

	// 默认 fallback：仍选中 a（有网可用）。
	sel, err := m.selectLocked("random-account", "cn")
	if err != nil || !sel.proxied || sel.node.ID != a {
		t.Fatalf("fallback: sel=%+v err=%v want proxied a", sel, err)
	}

	// strict：绝不占用绑定代理，走直连兜底（未配置总代理时 proxied=false, global=false）。
	if err := m.SetRandomBindAvoid("strict"); err != nil {
		t.Fatal(err)
	}
	sel, err = m.selectLocked("random-account", "cn")
	if err != nil {
		t.Fatalf("strict should not error: %v", err)
	}
	if sel.proxied || sel.global {
		t.Fatalf("strict must not use bound proxy: sel=%+v", sel)
	}

	// 非法值拒绝，持久化可重启恢复。
	if err := m.SetRandomBindAvoid("bogus"); err == nil {
		t.Fatal("invalid mode should be rejected")
	}
	if got := m.RandomBindAvoid(); got != "strict" {
		t.Fatalf("mode=%q want strict", got)
	}
	m2 := testManager(t, path)
	if got := m2.RandomBindAvoid(); got != "strict" {
		t.Fatalf("persisted mode=%q want strict", got)
	}
}

// realm 默认 fixed 不算「账号专属」：未绑定账号仍可随机命中它。
func TestRandomIgnoresRealmFixedAsBound(t *testing.T) {
	path := filepath.Join(t.TempDir(), "proxy-pool.json")
	m := testManager(t, path)
	a := addProxy(t, m, "http://127.0.0.1:3001")
	m.health[a] = health{status: "healthy"}
	if err := m.BindRealm("cn", Binding{Mode: "fixed", ProxyID: a}); err != nil {
		t.Fatal(err)
	}
	sel, err := m.selectLocked("random-account", "cn")
	if err != nil || !sel.proxied || sel.node.ID != a {
		t.Fatalf("realm fixed should still be random-able: sel=%+v err=%v", sel, err)
	}
}
