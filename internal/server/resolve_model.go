package server

import "strings"

// resolveModelDetail 解析模型名协议（PLAN D6），并区分「realm 是否由调用方显式写出」：
//
//	分布式前缀： "[realm:]model"
//
// 取第一个 ":"，前段恰为 "cn"/"global" 才剥离；否则视为裸名，realm=cn、bare=原串。
// 大小写敏感（前缀必须是精确的小写枚举）。bare 即出站/选号/账本使用的裸模型名。
//
// 第三个返回值 realmExplicit 供跨域回退（issue #135）判定：只有裸名（false）才允许
// 在首选域选不到号时改用另一域；显式 cn:/global: 前缀永不被回退改写。
func resolveModelDetail(model string) (realm, bare string, realmExplicit bool) {
	return resolveModelDetailDefault(model, "cn")
}

// resolveModelDetailDefault 同 resolveModelDetail，但裸名的默认域由调用方指定
// （config pool.preferred_realm："" 或非法值按 "cn" 处理）。
// 显式前缀分支完全不受 defaultRealm 影响——写死哪个域就用哪个域，这是跨域回退的
// 前提（realmExplicit=true 时调用方不会改写域）。
func resolveModelDetailDefault(model, defaultRealm string) (realm, bare string, realmExplicit bool) {
	if defaultRealm != "global" {
		defaultRealm = "cn"
	}
	idx := strings.IndexByte(model, ':')
	if idx < 0 {
		return defaultRealm, model, false
	}
	prefix := model[:idx]
	if prefix != "cn" && prefix != "global" {
		return defaultRealm, model, false
	}
	return prefix, model[idx+1:], true
}

// resolveModel 是 resolveModelDetail 的两返回值形态（既有调用方语义零改动）。
func resolveModel(model string) (realm, bare string) {
	realm, bare, _ = resolveModelDetail(model)
	return realm, bare
}

// ResolveModel 是 resolveModel 的导出面（跨包调用）。
func ResolveModel(model string) (realm, bare string) { return resolveModel(model) }

// otherRealm 返回另一个选号域（"cn" ↔ "global"），供跨域回退使用。
func otherRealm(realm string) string {
	if realm == "global" {
		return "cn"
	}
	return "global"
}
