// wafip.go WAF IP 级拦截 fail-fast 状态机（任务书 waf-ip-failfast）。
//
// 背景（本项目-scan-absorb T-1 / BulidH 实测）：WAF 403 拦的是网关出口 IP 而非
// 账号——3 个账号 1 秒内全 403。既有 ErrWafBlock 账号级软冷却（wafCooldownBase
// 起的有界退避）在 IP 级拦截时不够：轮转会把一次客户端请求放大 MaxRotate 倍，
// 同一出口 IP 继续打上游只会加重风控。
//
// 判定：ErrWafBlock 基础上的短窗多号计数——wafIPWindow（60s 滑动窗）内
// ≥ wafIPThreshold 个**不同** UID 接连命中 WAF 403 → 判定 IP 级拦截，激活至
// now+wafIPWindow。单号反复 403（账号级偶发）永不触发：只数不同号。
// 激活期内新命中不续期（保守：不做主动探测，窗口自然解除）。
//
// 归属层评估：放 server（Handler 局部）而非 pool——IP 级状态唯一消费者是
// chatCompletions 轮转循环（是否继续轮转），pool 是账号级记账层，跨 UID 语义
// 不属于任何账号；server 已有进程级状态先例 degradeGate（mu+until 同风格）。
// 账号级软冷却照常记账（applyErrorPolicy 不变），IP 级状态只改变「是否继续
// 轮转」——协同不叠加。进程内状态、重启清零（窗口 60s，重建成本极低）。
package server

import (
	"log"
	"sync"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// wafIPWindow IP 级判定滑动窗 + 激活时长：窗内不同账号命中 WAF 403 达阈值即
// 判 IP 级拦截，激活同样长（到期自然解除）。var 仅供测试注入短窗（生产恒 60s，
// 任务书「如 60s 滑动窗」口径）。
var wafIPWindow = 60 * time.Second

// wafIPThreshold 判定阈值：窗内不同 UID 数达到该值激活。取 2——「多号」的最小
// 定义：单号反复 403 永不触发（账号级偶发归软冷却管），两个不同号在 60s 内接连
// 被拦（同一出口 IP）已是 IP 级证据（BulidH 实测 3 号 1s 全拦，阈值 2 更早止损，
// 少放大一次轮转）。
const wafIPThreshold = 2

// wafIPGate WAF IP 级拦截状态机（Handler 内嵌，零值可用）。
type wafIPGate struct {
	mu    sync.Mutex
	hits  map[string]map[string]time.Time // 出口键 → uid → 最近命中时刻
	until map[string]time.Time            // 出口键 → IP 级拦截截止时间
}

// noteWaf 记录某个实际出口上的某账号 WAF 403，返回该出口是否应 fail-fast。
// 出口键按 direct/global/proxy:<id> 隔离；同一代理出口上的多个账号命中才会
// 触发该出口的 IP 级状态，不会把其他代理或直连出口一起判死。
func (g *wafIPGate) noteWaf(exitKey, uid string) bool {
	if exitKey == "" {
		exitKey = "direct"
	}
	now := time.Now()
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.until != nil && now.Before(g.until[exitKey]) {
		return true
	}
	if g.hits == nil {
		g.hits = map[string]map[string]time.Time{}
	}
	if g.hits[exitKey] == nil {
		g.hits[exitKey] = map[string]time.Time{}
	}
	g.hits[exitKey][uid] = now
	for u, t := range g.hits[exitKey] {
		if now.Sub(t) > wafIPWindow {
			delete(g.hits[exitKey], u)
		}
	}
	if len(g.hits[exitKey]) >= wafIPThreshold {
		if g.until == nil {
			g.until = map[string]time.Time{}
		}
		g.until[exitKey] = now.Add(wafIPWindow)
		log.Printf("WARN: [server] waf exit block: exit=%s accounts=%d window=%s until=%s", exitKey, len(g.hits[exitKey]), wafIPWindow, g.until[exitKey].Format(time.RFC3339))
		g.hits[exitKey] = map[string]time.Time{}
		return true
	}
	return false
}

// active 报告某个出口是否已进入 fail-fast 状态。
func (g *wafIPGate) active(exitKey string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return !g.until[exitKey].IsZero() && time.Now().Before(g.until[exitKey])
}

func wafExitKey(err *upstream.Error) string {
	if err == nil || !err.HasRoute {
		return "direct"
	}
	if err.Route.Global {
		return "global"
	}
	if err.Route.Proxied && err.Route.ProxyID != "" {
		return "proxy:" + err.Route.ProxyID
	}
	return "direct"
}

func (h *Handler) blockWAFExit(err *upstream.Error) {
	if err == nil || !err.HasRoute || h.cfg.Upstream == nil || h.cfg.Upstream.ProxyPool == nil {
		return
	}
	switch {
	case !err.Route.Global && !err.Route.Proxied:
		h.cfg.Upstream.ProxyPool.BlockExit("direct", wafIPWindow)
	case err.Route.Global:
		h.cfg.Upstream.ProxyPool.BlockExit("global", wafIPWindow)
	case err.Route.Proxied && err.Route.ProxyID != "":
		h.cfg.Upstream.ProxyPool.BlockExit(err.Route.ProxyID, wafIPWindow)
	}
}
