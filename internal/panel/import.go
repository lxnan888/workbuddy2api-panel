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
	"sync"
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
	Group          string `json:"group"`
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
	group string // 分组标签（弹窗默认组可被 JSON 的 group 字段覆盖）
	file  string // 来源文件名（错误定位）
}

// importJob 一批导入任务的状态（进程内单例，前端轮询读快照）。
type importJob struct {
	mu       sync.Mutex
	ID       string   `json:"job_id"`
	Running  bool     `json:"running"`
	Done     bool     `json:"done"`
	Total    int      `json:"total"`
	Finished int      `json:"finished"` // 已处理条目（含跳过），进度分母口径
	Imported int      `json:"imported"`
	Skipped  int      `json:"skipped"`
	Current  string   `json:"current,omitempty"` // 当前处理的文件 / uid
	Errors   []string `json:"errors,omitempty"`
	Started  int64    `json:"started_at"`
	Ended    int64    `json:"ended_at,omitempty"`
}

// importSnapshot 返回任务状态的一致快照。
func (j *importJob) snapshot() map[string]any {
	j.mu.Lock()
	defer j.mu.Unlock()
	return map[string]any{
		"job_id":     j.ID,
		"running":    j.Running,
		"done":       j.Done,
		"total":      j.Total,
		"finished":   j.Finished,
		"imported":   j.Imported,
		"skipped":    j.Skipped,
		"current":    j.Current,
		"errors":     append([]string(nil), j.Errors...),
		"started_at": j.Started,
		"ended_at":   j.Ended,
	}
}

// importState 面板级导入任务状态：全局只允许一个进行中的任务。
type importState struct {
	mu      sync.Mutex
	current *importJob
}

var panelImport = importState{}

// importCockpit 接收多个账号 JSON 文件，启动后台导入任务并立即返回 202。
//
//	POST /panel/api/import/cockpit
//	Content-Type: multipart/form-data
//	Body: file=<json>（可重复多个）、group=<目标分组>
//
// 返回 {ok, job_id, total}；进度经 GET /panel/api/import/status 轮询。
// 未完成前再次提交返回 409（全局单任务）。按 UID 去重：批内重复 + 池里已存在
// 都跳过，不覆盖凭证、不解冻、不重跑签到/激活/查余额。
func (p *Panel) importCockpit(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(64 << 20); err != nil {
		writeErr(w, http.StatusBadRequest, "parse form: "+err.Error())
		return
	}
	defaultGroup := strings.TrimSpace(r.FormValue("group"))

	// 先把全部文件读进内存（文件内容小），再一次性建任务，避免「建了任务才发现解析失败」。
	var entries []importEntry
	var errs []string
	total := 0
	for _, fh := range r.MultipartForm.File["file"] {
		f, err := fh.Open()
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", fh.Filename, err))
			continue
		}
		raw, readErr := io.ReadAll(f)
		_ = f.Close()
		if readErr != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", fh.Filename, readErr))
			continue
		}
		fileEntries, fileTotal, parseErrs, perr := parseImportFile(raw)
		if perr != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", fh.Filename, perr))
			continue
		}
		total += fileTotal
		for _, pe := range parseErrs {
			errs = append(errs, fh.Filename+"："+pe)
		}
		for _, e := range fileEntries {
			e.file = fh.Filename
			if e.group == "" {
				e.group = defaultGroup
			}
			entries = append(entries, e)
		}
	}
	if total == 0 {
		writeErr(w, http.StatusBadRequest, "no valid account entries in files")
		return
	}

	// 单任务锁：CAS 建任务，未完成前拒绝新任务。
	job := &importJob{
		ID:      fmt.Sprintf("imp-%d", time.Now().UnixNano()),
		Running: true,
		Total:   total,
		Errors:  append([]string(nil), errs...),
		Started: time.Now().Unix(),
	}
	panelImport.mu.Lock()
	if panelImport.current != nil && panelImport.current.Running {
		panelImport.mu.Unlock()
		writeErr(w, http.StatusConflict, "已有导入任务进行中，请等待完成后再导入")
		return
	}
	panelImport.current = job
	panelImport.mu.Unlock()

	go p.runImportJob(job, entries)
	log.Printf("panel: import job %s started total=%d entries=%d group=%q", job.ID, total, len(entries), defaultGroup)
	writeJSON(w, http.StatusAccepted, map[string]any{
		"ok":     true,
		"job_id": job.ID,
		"total":  total,
	})
}

// importStatus 返回当前（或最近一次）导入任务的进度快照。
// 无任务时返回 {ok:true, running:false, job_id:""}，前端据此清空进度条。
func (p *Panel) importStatus(w http.ResponseWriter, r *http.Request) {
	panelImport.mu.Lock()
	job := panelImport.current
	panelImport.mu.Unlock()
	if job == nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "running": false, "job_id": ""})
		return
	}
	snap := job.snapshot()
	snap["ok"] = true
	writeJSON(w, http.StatusOK, snap)
}

// runImportJob 串行处理全部条目：每处理一条推进度（含跳过），并写回分组。
func (p *Panel) runImportJob(job *importJob, entries []importEntry) {
	defer func() {
		job.mu.Lock()
		job.Running = false
		job.Done = true
		job.Ended = time.Now().Unix()
		job.Current = ""
		job.mu.Unlock()
	}()

	seen := make(map[string]bool, len(entries))
	for _, e := range entries {
		uid := e.auth.UID
		job.mu.Lock()
		job.Current = e.file + " / " + uid
		job.mu.Unlock()

		// 按 UID 去重：批内重复 / 池里已存在都跳过。
		if seen[uid] || p.cfg.Pool.AuthByUID(uid) != nil {
			job.mu.Lock()
			job.Finished++
			job.Skipped++
			job.Errors = append(job.Errors, fmt.Sprintf("uid=%s：重复（已存在或本批重复），跳过", uid))
			job.mu.Unlock()
			continue
		}
		seen[uid] = true

		if err := p.importOne(e); err != nil {
			job.mu.Lock()
			job.Finished++
			job.Skipped++
			job.Errors = append(job.Errors, fmt.Sprintf("uid=%s：%v", uid, err))
			job.mu.Unlock()
			continue
		}
		if e.group != "" {
			p.cfg.Pool.SetAccountGroup(uid, e.group)
		}
		job.mu.Lock()
		job.Finished++
		job.Imported++
		job.mu.Unlock()
	}
	log.Printf("panel: import job %s finished total=%d imported=%d skipped=%d", job.ID, job.Total, job.Imported, job.Skipped)
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
	e := importEntry{auth: a, realm: realm}
	// 顶层 group 字段（弹窗默认组可被它覆盖）。
	var extra struct {
		Group string `json:"group"`
	}
	if json.Unmarshal(raw, &extra) == nil {
		e.group = strings.TrimSpace(extra.Group)
	}
	return e, nil
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
		group: strings.TrimSpace(acc.Group),
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
