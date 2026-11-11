// 分池选号域：按 realm（cn/global）过滤选号与可用集合。realm=="" 退化为现状。
package pool

import (
	"sort"
	"time"
)

// expiringVirtualSlots 快过期账号在新会话候选集中的虚拟实例权重。
// 3:1 是温和偏好，不是固定比例：账号组成变化会自然改变最终占比。
const expiringVirtualSlots = 3

// AvailableUIDsForRealm 同 AvailableUIDs，但仅返回 Realm()==realm 的账号。
// realm=="" 退化为 AvailableUIDs（现状语义）。
func (p *Pool) AvailableUIDsForRealm(realm string) []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	uids := make([]string, 0, len(p.byUID))
	for uid, e := range p.byUID {
		if realm != "" && e.a.Realm() != realm {
			continue
		}
		if !e.healthy(now) {
			continue
		}
		if p.concurrencyFull(e) {
			continue
		}
		uids = append(uids, uid)
	}
	sort.Strings(uids)
	return uids
}

// ServableForModelRealm 返回 realm 内此刻**真正能服务该模型**的账号数。
//
// 与 pick 的候选口径一致（healthyForModel 的 6004/11102 模型级冷却豁免、在途租约、
// 积分保底都计入），但**不含 pick 的全冷却兜底**——跨域回退（issue #135）要的正是
// 「这个域此刻有没有健康号」，兜底把冷却号捞回来的乐观语义会掩盖这一点。
//
// 只读：持 RLock 遍历，不改任何状态。realm=="" 时统计全池（与既有 realm 语义一致）。
func (p *Pool) ServableForModelRealm(model, realm string) int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	n := 0
	for _, e := range p.byUID {
		if realm != "" && e.a.Realm() != realm {
			continue
		}
		if !e.healthyForModel(now, model) {
			continue
		}
		if p.concurrencyFull(e) {
			continue
		}
		if p.floorBlockedForRealmModel(e, model, realm, now) {
			continue
		}
		n++
	}
	return n
}

// WeightedAvailableUIDsForModelRealm 返回带虚拟实例权重的可用账号列表。
//
// 普通账号出现 1 次；有效快过期账号出现 expiringVirtualSlots 次。调用方继续按
// 原有序列表哈希，即可让新会话对快过期账号形成温和偏好。重复项按 UID 排序后
// 展开，保证同一账号拓扑下不同进程得到一致列表。
//
// 该方法是现有 AvailableUIDsForModelRealm 的增量入口，不改变旧方法语义，也不
// 修改配置、状态或 Redis schema。prefer_expiring=false 时退化为逐账号一次。
func (p *Pool) WeightedAvailableUIDsForModelRealm(model, realm string) []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	uids := make([]string, 0, len(p.byUID))
	for uid, e := range p.byUID {
		if realm != "" && e.a.Realm() != realm {
			continue
		}
		if !e.healthyForModel(now, model) {
			continue
		}
		if p.concurrencyFull(e) {
			continue
		}
		uids = append(uids, uid)
	}
	sort.Strings(uids)

	out := make([]string, 0, len(uids))
	for _, uid := range uids {
		slots := 1
		if p.preferExpiring {
			if e := p.byUID[uid]; expiringNow(e, now) {
				slots = expiringVirtualSlots
			}
		}
		for i := 0; i < slots; i++ {
			out = append(out, uid)
		}
	}
	return out
}

// AvailableUIDsForModelRealm 同 AvailableUIDsForModel，但仅返回 Realm()==realm 的账号
// （6004 模型豁免照常生效）。realm=="" 退化为 AvailableUIDsForModel。
func (p *Pool) AvailableUIDsForModelRealm(model, realm string) []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	uids := make([]string, 0, len(p.byUID))
	for uid, e := range p.byUID {
		if realm != "" && e.a.Realm() != realm {
			continue
		}
		if !e.healthyForModel(now, model) {
			continue
		}
		if p.concurrencyFull(e) {
			continue
		}
		uids = append(uids, uid)
	}
	sort.Strings(uids)
	return uids
}

// AvailableUIDsForModelRealms 返回可服务该模型的账号，供会话粘性校验「已绑定账号
// 是否仍可用」。与 WeightedAvailableUIDsForModelPriority 的区别：本方法**不按优先级
// 过滤**——已绑定的低优先级账号继续有效（粘性优先）；includeOther=true 时合并两域，
// 让裸模型名绑定到另一域后不会被粘性校验误判为失效。
func (p *Pool) AvailableUIDsForModelRealms(model, preferredRealm string, includeOther bool) []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	uids := make([]string, 0, len(p.byUID))
	for uid, e := range p.byUID {
		if !realmAllowed(e, preferredRealm, includeOther) {
			continue
		}
		if !e.healthyForModel(now, model) {
			continue
		}
		if p.concurrencyFull(e) {
			continue
		}
		if p.floorBlockedForRealmModel(e, model, e.a.Realm(), now) {
			continue
		}
		uids = append(uids, uid)
	}
	sort.Strings(uids)
	return uids
}

// WeightedAvailableUIDsForModelPriority 返回新粘性会话的可分配账号列表：
// 只保留最高优先级组；同优先级内首选域优先，首选域没有可分配账号时才用另一域。
// 与 pick 的优先级口径一致；快过期账号仍按虚拟实例权重展开（同组内温和偏好）。
// includeOther=false 时只在 preferredRealm 内分配（显式 cn:/global: 前缀）。
func (p *Pool) WeightedAvailableUIDsForModelPriority(model, preferredRealm string, includeOther bool) []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	cands := make([]*entry, 0, len(p.byUID))
	for _, e := range p.byUID {
		if !realmAllowed(e, preferredRealm, includeOther) {
			continue
		}
		if !e.healthyForModel(now, model) {
			continue
		}
		if p.concurrencyFull(e) {
			continue
		}
		if p.floorBlockedForRealmModel(e, model, e.a.Realm(), now) {
			continue
		}
		cands = append(cands, e)
	}
	if len(cands) == 0 {
		return nil
	}
	cands = maxPriorityCandidates(cands)
	if includeOther && preferredRealm != "" {
		if pref := realmCandidates(cands, preferredRealm); len(pref) > 0 {
			cands = pref
		}
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].a.UID < cands[j].a.UID })
	out := make([]string, 0, len(cands))
	for _, e := range cands {
		slots := 1
		if p.preferExpiring && expiringNow(e, now) {
			slots = expiringVirtualSlots
		}
		for i := 0; i < slots; i++ {
			out = append(out, e.a.UID)
		}
	}
	return out
}

// realmAllowed 报告账号是否在本次选号允许的域内：preferredRealm 为空或
// includeOther=true 时不限域（跨域优先级），否则必须精确匹配首选域。
func realmAllowed(e *entry, preferredRealm string, includeOther bool) bool {
	if preferredRealm == "" || includeOther {
		return true
	}
	return e.a.Realm() == preferredRealm
}
