package upstream

import (
	"encoding/json"
	"strings"
	"testing"
)

// extractCacheKey 从改写后的 body 里取出 prompt_cache_key 字段值。
func extractCacheKey(t *testing.T, body []byte) string {
	t.Helper()
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	v, _ := obj["prompt_cache_key"].(string)
	return v
}

// TestInjectPromptCacheKey_PreservesExisting 覆盖任务用例 1：
// 入站 body 已带 prompt_cache_key → 原值保留，不被网关覆盖。
func TestInjectPromptCacheKey_PreservesExisting(t *testing.T) {
	// Arrange
	uid := "user-abc-123"
	conv := "conv-xyz"
	in := `{"model":"glm-5.2","messages":[],"prompt_cache_key":"client-set-key"}`

	// Act
	out := InjectPromptCacheKey([]byte(in), uid, conv)

	// Assert
	if got := extractCacheKey(t, out); got != "client-set-key" {
		t.Fatalf("existing prompt_cache_key overwritten: got=%q want client-set-key", got)
	}
}

// isUUID4 校验 36 字符 UUID v4 形态（版本位 4、变体位 89ab）。
func isUUID4(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if s[i] != '-' {
				return false
			}
			continue
		}
		if !((s[i] >= '0' && s[i] <= '9') || (s[i] >= 'a' && s[i] <= 'f')) {
			return false
		}
	}
	return s[14] == '4' && strings.ContainsRune("89ab", rune(s[19]))
}

// TestInjectPromptCacheKey_UsesConversationID 覆盖任务用例 2：
// body 无 prompt_cache_key 但有 conversation_id → 用它做会话哈希源。
// 格式为 UUID v4 形态（不含项目缩写/uid 明文）。
func TestInjectPromptCacheKey_UsesConversationID(t *testing.T) {
	uid := "user-abc-123"
	in := `{"model":"glm-5.2","messages":[],"conversation_id":"conv-42"}`

	out := InjectPromptCacheKey([]byte(in), uid, "")
	got := extractCacheKey(t, out)
	if !isUUID4(got) {
		t.Fatalf("expected uuid4 cache key, got=%q", got)
	}
	if strings.Contains(got, "user-abc") || strings.HasPrefix(got, "wb2a-") {
		t.Fatalf("cache key must not carry uid/project fingerprint: %q", got)
	}
	// 同 conversation_id 两次出站 → 同 key（稳定）。
	out2 := InjectPromptCacheKey([]byte(in), uid, "")
	if got2 := extractCacheKey(t, out2); got2 != got {
		t.Fatalf("non-deterministic key for same conversation_id: %q vs %q", got, got2)
	}
	// 不同 conversation_id → 不同 key（哈希段区分）。
	inB := `{"model":"glm-5.2","messages":[],"conversation_id":"conv-99"}`
	gotB := extractCacheKey(t, InjectPromptCacheKey([]byte(inB), uid, ""))
	if gotB == got {
		t.Fatalf("different conversation_id produced same key: %q", got)
	}
}

// TestInjectPromptCacheKey_GeneratesStableKey 覆盖任务用例 3：
// body 两者都无 → 网关生成稳定键，两次同输入同账号得到同 key。
func TestInjectPromptCacheKey_GeneratesStableKey(t *testing.T) {
	uid := "user-abc-123"
	conv := "conv-stable"
	in := `{"model":"glm-5.2","messages":[]}`

	a := InjectPromptCacheKey([]byte(in), uid, conv)
	b := InjectPromptCacheKey([]byte(in), uid, conv)
	ka, kb := extractCacheKey(t, a), extractCacheKey(t, b)
	if ka != kb {
		t.Fatalf("non-deterministic key for same input+uid: a=%q b=%q", ka, kb)
	}
}

// TestInjectPromptCacheKey_AccountIsolation 覆盖任务用例 4：
// 不同账号 → key 不同（隔离验证）。
func TestInjectPromptCacheKey_AccountIsolation(t *testing.T) {
	conv := "conv-shared"
	in := `{"model":"glm-5.2","messages":[]}`

	ka := extractCacheKey(t, InjectPromptCacheKey([]byte(in), "user-aaa-111", conv))
	kb := extractCacheKey(t, InjectPromptCacheKey([]byte(in), "user-bbb-222", conv))
	if ka == kb {
		t.Fatalf("cross-account key collision: both=%q", ka)
	}
	if !isUUID4(ka) || !isUUID4(kb) {
		t.Fatalf("keys must be uuid4: a=%q b=%q", ka, kb)
	}
}

// TestInjectPromptCacheKey_Format 覆盖任务用例 5：
// cache key 格式校验（UUID v4，无 uid 明文）。
func TestInjectPromptCacheKey_Format(t *testing.T) {
	uid := "1234567890abcdef"
	in := `{"model":"glm-5.2","messages":[]}`
	out := InjectPromptCacheKey([]byte(in), uid, "conv-fmt")
	got := extractCacheKey(t, out)
	if !isUUID4(got) {
		t.Fatalf("expected uuid4 key, got=%q", got)
	}
	if strings.Contains(got, "12345678") {
		t.Fatalf("uid must not appear in key, got=%q", got)
	}
}

// TestInjectPromptCacheKey_EmptyConversation 覆盖任务用例 3 边界：
// 无 conversation_id 且无会话标识 → 仍生成稳定 UUID v4 键（不复用其他会话前缀）。
func TestInjectPromptCacheKey_EmptyConversation(t *testing.T) {
	uid := "user-abc-123"
	in := `{"model":"glm-5.2","messages":[]}`
	out := InjectPromptCacheKey([]byte(in), uid, "")
	got := extractCacheKey(t, out)
	if got == "" {
		t.Fatalf("expected non-empty key when no conversation, got empty")
	}
	if !isUUID4(got) {
		t.Fatalf("expected uuid4 key, got=%q", got)
	}
	if strings.Contains(got, "user-abc") {
		t.Fatalf("uid must not appear in key, got=%q", got)
	}
}

// TestPrepareBodyOptNoCacheKeyInjection 覆盖任务用例 6：
// PrepareBodyOpt（sanitize=false 的旧入口）行为不变——不注入 cache key。
// 向后兼容：仅传 body 不给 cacheKey 上下文时，不得引入新字段。
func TestPrepareBodyOptNoCacheKeyInjection(t *testing.T) {
	in := `{"model":"glm-5.2","messages":[]}`
	out := PrepareBodyOpt([]byte(in), false)
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, present := obj["prompt_cache_key"]; present {
		t.Fatalf("PrepareBodyOpt must NOT inject prompt_cache_key, but got: %v", obj["prompt_cache_key"])
	}
}
