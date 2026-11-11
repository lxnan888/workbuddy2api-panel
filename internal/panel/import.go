package panel

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// cockpitAccount 映射 cockpit tools 导出格式的单个账号。
type cockpitAccount struct {
	ID             string `json:"id"`
	Email          string `json:"email"`
	UID            string `json:"uid"`
	Nickname       string `json:"nickname"`
	AccessToken    string `json:"access_token"`
	RefreshToken   string `json:"refresh_token"`
	TokenType      string `json:"token_type"`
	ExpiresAt      int64  `json:"expires_at"`
	Domain         string `json:"domain"`
	DosageNotify   string `json:"dosage_notify_code"`
	PaymentType    string `json:"payment_type"`
	Status         string `json:"status"`
	UsageUpdatedAt int64  `json:"usage_updated_at"`
	LastCheckin    int64  `json:"last_checkin_time"`
	CheckinStreak  int    `json:"checkin_streak"`
	CreatedAt      int64  `json:"created_at"`
	LastUsed       int64  `json:"last_used"`
}

// importEntry 是解析完成、尚未落盘的账号。
type importEntry struct {
	auth  *auth.Auth
	realm string // cn / global
}

// importCockpit 接收账号 JSON 文件并批量导入账号到池中。
//
//	POST /panel/api/import/cockpit
//	Content-Type: multipart/form-data
//	Body: file=<json>
//
// 自动识别三种形态：
//  1. 插件 OAuth 单对象 {"auth": {...}, "account": {...}}（也兼容扁平 auth 对象）
//  2. 上述对象的数组
//  3. cockpit tools 导出的账号数组（snake_case access_token/refresh_token）
//
// 返回 {ok, total, imported, skipped, errors}。
func (p *Panel) importCockpit(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		writeErr(w, http.StatusBadRequest, "parse form: "+err.Error())
		return
	}
	file, _, err := r.FormFile("file")
	if err != nil {
		writeErr(w, http.StatusBadRequest, "missing file field: "+err.Error())
		return
	}
	defer file.Close()

	raw, err := io.ReadAll(file)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "read file: "+err.Error())
		return
	}

	entries, total, parseErrs, err := parseImportFile(raw)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	imported := 0
	skipped := len(parseErrs)
	errs := append([]string(nil), parseErrs...)
	for _, e := range entries {
		if err := p.importOne(e); err != nil {
			skipped++
			errs = append(errs, fmt.Sprintf("uid=%s: %v", e.auth.UID, err))
			continue
		}
		imported++
	}

	log.Printf("panel: import finished total=%d imported=%d skipped=%d", total, imported, skipped)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"total":    total,
		"imported": imported,
		"skipped":  skipped,
		"errors":   errs,
	})
}

// parseImportFile 自动识别单对象 / 对象数组 / cockpit 数组。
// 返回成功解析的条目、文件内账号条目总数、逐条错误；顶层结构非法时返回 error（→ 400）。
func parseImportFile(raw []byte) ([]importEntry, int, []string, error) {
	trim := bytes.TrimSpace(raw)
	if len(trim) == 0 {
		return nil, 0, nil, fmt.Errorf("empty file")
	}
	var items []json.RawMessage
	switch trim[0] {
	case '[':
		if err := json.Unmarshal(trim, &items); err != nil {
			return nil, 0, nil, fmt.Errorf("invalid json: %w", err)
		}
		if len(items) == 0 {
			return nil, 0, nil, fmt.Errorf("empty accounts array")
		}
	case '{':
		items = []json.RawMessage{trim}
	default:
		return nil, 0, nil, fmt.Errorf("unsupported json: expected an object or an array")
	}

	var entries []importEntry
	var errs []string
	for i, item := range items {
		entry, err := parseImportItem(item)
		if err != nil {
			errs = append(errs, fmt.Sprintf("item %d: %v", i+1, err))
			continue
		}
		entries = append(entries, entry)
	}
	return entries, len(items), errs, nil
}

// parseImportItem 按对象键识别单条账号格式。
func parseImportItem(raw json.RawMessage) (importEntry, error) {
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil {
		return importEntry{}, fmt.Errorf("not a JSON object")
	}
	// 插件 OAuth 嵌套形 / 扁平形：交给 auth.Parse（解析规则与 auths/ 目录一致）。
	if _, ok := probe["auth"]; ok {
		return parseOAuthAuth(raw)
	}
	if _, ok := probe["accessToken"]; ok {
		return parseOAuthAuth(raw)
	}
	// cockpit tools 导出形：snake_case。
	if _, ok := probe["access_token"]; ok {
		return parseCockpitAccount(raw)
	}
	return importEntry{}, fmt.Errorf("unrecognized account format (need auth/accessToken or cockpit access_token)")
}

// parseOAuthAuth 解析插件 OAuth 形（嵌套/扁平）单条账号。
func parseOAuthAuth(raw []byte) (importEntry, error) {
	a, err := auth.Parse(raw)
	if err != nil {
		return importEntry{}, err
	}
	uid := strings.TrimSpace(a.UID)
	if uid == "" {
		return importEntry{}, fmt.Errorf("missing uid")
	}
	if !validImportUID(uid) {
		return importEntry{}, fmt.Errorf("invalid uid %q", uid)
	}
	a.UID = uid
	a.AccessToken = strings.TrimSpace(a.AccessToken)
	a.RefreshToken = strings.TrimSpace(a.RefreshToken)
	a.Domain = strings.TrimSpace(a.Domain)
	a.Nickname = strings.TrimSpace(a.Nickname)
	if a.RefreshToken == "" {
		return importEntry{}, fmt.Errorf("missing refreshToken (uid=%s)", uid)
	}
	realm := auth.ResolveRealm(a.RealmStored(), a.Domain)
	if realm != "cn" && realm != "global" {
		return importEntry{}, fmt.Errorf("invalid realm %q (uid=%s)", realm, uid)
	}
	return importEntry{auth: a, realm: realm}, nil
}

// parseCockpitAccount 解析 cockpit tools 导出形单条账号。
func parseCockpitAccount(raw []byte) (importEntry, error) {
	var acc cockpitAccount
	if err := json.Unmarshal(raw, &acc); err != nil {
		return importEntry{}, fmt.Errorf("invalid cockpit account: %w", err)
	}
	uid := strings.TrimSpace(acc.UID)
	at := strings.TrimSpace(acc.AccessToken)
	rt := strings.TrimSpace(acc.RefreshToken)
	if uid == "" || at == "" || rt == "" {
		return importEntry{}, fmt.Errorf("missing required fields (id=%s)", acc.ID)
	}
	if !validImportUID(uid) {
		return importEntry{}, fmt.Errorf("invalid uid (id=%s)", acc.ID)
	}

	// 按 domain 推断 realm：workbuddy.ai 家族 → global，否则 cn。
	realm := auth.ResolveRealm("", acc.Domain)

	// cockpit tools 的 expires_at 为毫秒时间戳，转为秒。
	expiresAt := acc.ExpiresAt / 1000
	if expiresAt <= 0 {
		expiresAt = time.Now().Add(365 * 24 * time.Hour).Unix()
	}

	nickname := strings.TrimSpace(acc.Nickname)
	if nickname == "" {
		nickname = strings.TrimSpace(acc.Email)
	}

	return importEntry{
		auth: &auth.Auth{
			AccessToken:  at,
			RefreshToken: rt,
			ExpiresAt:    expiresAt,
			Domain:       strings.TrimSpace(acc.Domain),
			UID:          uid,
			Nickname:     nickname,
		},
		realm: realm,
	}, nil
}

// importOne 落盘并接入号池，随后执行与 cockpit 导入一致的自动动作。
func (p *Panel) importOne(e importEntry) error {
	a := e.auth
	dir := p.AuthDir()
	a.FilePath = filepath.Join(dir, fmt.Sprintf("workbuddy-%s.json", a.UID))

	if e.realm == "global" {
		if _, err := auth.BackfillRealmFor(a, "global"); err != nil {
			return fmt.Errorf("set realm: %w", err)
		}
	} else {
		// cn 且未写 realm 时补 cn；已有标识保持（与 LoadDir 存量迁移同口径）。
		_, _ = a.BackfillRealm()
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir auth dir: %w", err)
	}
	if err := a.SaveAtomic(); err != nil {
		return fmt.Errorf("save auth: %w", err)
	}

	p.cfg.Pool.Add(a)
	p.cfg.Pool.Revive(a.UID)
	p.StartNewAccountTasks(a.UID)

	// 顺带签到/激活（幂等；失败仅记日志，不阻断导入）。
	if e.realm == "global" {
		if activated, err := p.cfg.Upstream.GlobalCompleteRegistration(a); err != nil {
			log.Printf("panel: import global 注册激活 uid=%s: %v", a.UID, err)
		} else if activated {
			log.Printf("panel: import global 注册激活 uid=%s 完成", a.UID)
		}
		if claimed, err := p.cfg.Upstream.ClaimTrial(a); err != nil {
			log.Printf("panel: import global trial uid=%s: %v", a.UID, err)
		} else if claimed {
			log.Printf("panel: import global trial uid=%s 已领", a.UID)
		}
	} else {
		if err := p.cfg.Upstream.DailyCheckin(a); err != nil {
			log.Printf("panel: import checkin uid=%s: %v", a.UID, err)
		}
	}
	if rm, tt, err := p.cfg.Upstream.UserResource(a); err == nil {
		p.cfg.Pool.ReenableIfCredits(a.UID, rm, tt)
	}
	return nil
}

// validImportUID 校验导入 uid 是否可用于拼文件名（同 login.go validUID 口径）。
func validImportUID(uid string) bool {
	if uid == "" || len(uid) > 64 {
		return false
	}
	for _, c := range uid {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_':
		default:
			return false
		}
	}
	return true
}
