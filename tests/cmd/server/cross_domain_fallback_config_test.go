// cross_domain_fallback_config_test.go pool.cross_domain_fallback 配置测试：
// 本 fork 默认 true（裸名可跨域跟随账号池）/ 文件可显式覆盖为 false。
package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestCrossDomainFallbackDefault 键缺席 → 默认 true（本 fork 的取舍：客户端只写一个
// 模型名即可跟随账号池；需要严格锁域时显式关闭）。上游基线是 false，此处有意不同。
func TestCrossDomainFallbackDefault(t *testing.T) {
	c := Default()
	if err := c.normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if !c.Pool.CrossDomainFallback {
		t.Error("cross_domain_fallback 缺省必须为 true（本 fork 默认开启）")
	}
}

// TestCrossDomainFallbackExplicitOff 显式 false 必须能关掉默认值（逃生门）。
func TestCrossDomainFallbackExplicitOff(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	if err := os.WriteFile(fp, []byte(`{"pool":{"cross_domain_fallback":false}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.Pool.CrossDomainFallback {
		t.Error("显式 cross_domain_fallback=false 未生效")
	}
}

// TestCrossDomainFallbackParsedFromFile 显式配置覆盖默认。
func TestCrossDomainFallbackParsedFromFile(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	if err := os.WriteFile(fp, []byte(`{"pool":{"cross_domain_fallback":true}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if !c.Pool.CrossDomainFallback {
		t.Error("cross_domain_fallback=true 未生效")
	}
}
