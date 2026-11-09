// Package livecfg 运行期可变配置的并发安全持有者。
//
// 背景：进程启动时读入的配置是普通字段（读多写零），但管理面板允许在线改配置，
// 于是少量"可热生效"的字段需要有并发安全的读写点。此处用不可变快照 + atomic 指针：
// 读方 Load 拿到一致视图，写方 Store 整体替换，无锁无数据竞争。
//
// 只承载**读路径深、热改需求强**的少数字段；池参数/排程参数等各有既有 setter
// （pool.SetBreaker、scheduler.Reconfigure 等），不重复收编到这里。
package livecfg

import (
	"strings"
	"sync/atomic"
	"time"
)

// Snapshot 一次读取的不可变配置视图。
type Snapshot struct {
	APIKey               string        // 网关/面板共同鉴权密钥；空 = 不鉴权
	SoftCooldown         time.Duration // 429 软冷却基数（<=0 时调用方回退内置默认）
	SanitizeFingerprints bool          // 出站请求体指纹脱敏
	RecordClientInfo     bool          // 请求日志是否记录调用来源（客户端 IP / UA）
	HiddenModels         map[string]struct{} // 精确隐藏的模型 id（已归一化为 "cn:x"/"global:x" 全量形式）；nil = 不隐藏
	// BareModelIDs 模型列表是否只暴露裸模型名（models.bare_ids）。热改安全。
	BareModelIDs bool
	// PreferredRealm 裸模型名的首选域（pool.preferred_realm）："cn"/"global"。
	// 空串按 "cn" 解释（与 config.normalize 的兜底一致）。热改安全。
	PreferredRealm string
	// PromptMode / PromptText 提示词模式与文本（custom/append 使用）。
	PromptMode string
	PromptText string
	// MaxPayloadBytes 入站请求体上限；RestrictionHold 11140 连坐暂停时长。
	MaxPayloadBytes int64
	RestrictionHold time.Duration
	// GlobalEnabled global realm 路由开关。
	GlobalEnabled bool
	// EmbeddingsEnabled 实验特性：/v1/embeddings 总开关。
	EmbeddingsEnabled bool
	// WatermarkStripEnabled 实验特性：/v1/images/strip-watermark 总开关。
	WatermarkStripEnabled bool
	// AllowlistEnabled/AllowedModels models.allowed 白名单（空 = 不启用）。
	AllowlistEnabled bool
	AllowedModels    map[string]struct{}
	// RecordContext 是否把请求上下文记入请求事件日志。
	RecordContext bool
}

// Holder 原子持有当前快照。
type Holder struct {
	p atomic.Pointer[Snapshot]
}

// New 以初始快照构建。
func New(s Snapshot) *Holder {
	h := &Holder{}
	h.Store(s)
	return h
}

// Load 返回当前快照（Holder 为 nil 或从未 Store 时返回零值快照，调用方无需判空）。
func (h *Holder) Load() Snapshot {
	if h == nil {
		return Snapshot{}
	}
	if s := h.p.Load(); s != nil {
		return *s
	}
	return Snapshot{}
}

// Store 整体替换快照。
func (h *Holder) Store(s Snapshot) { h.p.Store(&s) }

// NormalizeHiddenModels 把用户配置的 hidden 条目归一化为精确 id 集合。//
// 条目带合法 "cn:"/"global:" 前缀 → 只隐藏该域；裸名 → 两域都隐藏。
// 大小写敏感（与 internal/server.resolveModel 的前缀规则对称：取第一个 ":",
// 前段恰为 "cn"/"global" 才剥离）。返回的 map 只读共享，调用方不得修改。
func NormalizeHiddenModels(hidden []string) map[string]struct{} {
	if len(hidden) == 0 {
		return nil
	}
	out := make(map[string]struct{}, len(hidden)*2)
	for _, m := range hidden {
		m = strings.TrimSpace(m)
		if m == "" {
			continue
		}
		if prefix, bare, ok := strings.Cut(m, ":"); ok && (prefix == "cn" || prefix == "global") {
			out[prefix+":"+bare] = struct{}{}
		} else {
			out["cn:"+m] = struct{}{}
			out["global:"+m] = struct{}{}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// NormalizeAllowedModels 同 NormalizeHiddenModels，用于 models.allowed 白名单。
func NormalizeAllowedModels(allowed []string) map[string]struct{} {
	return NormalizeHiddenModels(allowed)
}
