package main

import (
	"github.com/linguo2625469/workbuddy2api-panel/internal/livecfg"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/server"
)

// realmAwareAvailableForModel 构造会话粘性的两个账号列表闭包：
//
//   - available：校验已绑定账号是否仍可用。**不按优先级过滤**（粘性优先），
//     裸模型名且跨域回退开启时合并两域，避免绑定到另一域后被误判失效；
//   - alloc：新会话分配候选。按账号优先级收敛到最高优先级组，同优先级内
//     首选域优先；显式 cn:/global: 前缀只在该域内分配。
//
// 粘性分配的模型名可能带 realm 前缀（"global:gpt-5.4" / "cn:glm-5.2"）：必须按
// preferred_realm 解析裸名默认域，再交给分池选号域过滤——否则裸名取 cn 全集，
// global 号会被粘性分配给 CN 前缀请求（跨 realm 泄漏）。live 为 nil 或尚未赋值时
// 按零值快照（cn）处理；请求到达时 main 已完成 live 初始化。
func realmAwareAvailableForModel(p *pool.Pool, live func() livecfg.Snapshot) (available, alloc func(model string) []string) {
	resolve := func(model string) (realm, bare string, includeOther bool) {
		realm, bare, explicit := server.ResolveModelDetail(model, live().PreferredRealm)
		return realm, bare, !explicit && p.CrossDomainFallback()
	}
	return func(model string) []string {
			realm, bare, includeOther := resolve(model)
			return p.AvailableUIDsForModelRealms(bare, realm, includeOther)
		}, func(model string) []string {
			realm, bare, includeOther := resolve(model)
			return p.WeightedAvailableUIDsForModelPriority(bare, realm, includeOther)
		}
}
