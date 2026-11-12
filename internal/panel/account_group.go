package panel

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sort"
)

// groupBody 分组写入的公共请求体解析。
func groupBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid group body")
		return false
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		writeErr(w, http.StatusBadRequest, "group body must contain one JSON value")
		return false
	}
	return true
}

// accountGroup 设置单账号分组；group="" = 移出分组（未分组）。
func (p *Panel) accountGroup(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Group string `json:"group"`
	}
	if !groupBody(w, r, &body) {
		return
	}
	if !p.cfg.Pool.SetAccountGroup(r.PathValue("uid"), body.Group) {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "groups": p.cfg.Pool.AccountGroups()})
}

// accountsGroup 批量把多个账号移到同一分组（含移到空串=移出分组）。
func (p *Panel) accountsGroup(w http.ResponseWriter, r *http.Request) {
	var body struct {
		UIDs  []string `json:"uids"`
		Group string   `json:"group"`
	}
	if !groupBody(w, r, &body) {
		return
	}
	if len(body.UIDs) == 0 {
		writeErr(w, http.StatusBadRequest, "uids must not be empty")
		return
	}
	n := p.cfg.Pool.SetAccountsGroup(body.UIDs, body.Group)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "updated": n, "groups": p.cfg.Pool.AccountGroups()})
}

// groupRename 重命名分组：整组账号的 group 标签一起改，不改账号本身。
func (p *Panel) groupRename(w http.ResponseWriter, r *http.Request) {
	var body struct {
		From string `json:"from"`
		To   string `json:"to"`
	}
	if !groupBody(w, r, &body) {
		return
	}
	n, ok := p.cfg.Pool.RenameAccountGroup(body.From, body.To)
	if !ok {
		writeErr(w, http.StatusBadRequest, "new group name must not be empty")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "updated": n, "groups": p.cfg.Pool.AccountGroups()})
}

// groupDelete 删除分组 = 直接删除该组全部账号（出池 + 删除 auth 凭证文件 +
// 解绑代理），与逐个点「移除」同口径；不是把账号移回未分组。前端必须二次确认。
func (p *Panel) groupDelete(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Group string `json:"group"`
	}
	if !groupBody(w, r, &body) {
		return
	}
	name := body.Group
	if name == "" {
		writeErr(w, http.StatusBadRequest, "group must not be empty")
		return
	}
	var uids []string
	for _, st := range p.cfg.Pool.List() {
		if st.Group == name {
			uids = append(uids, st.UID)
		}
	}
	sort.Strings(uids)
	removed, fileErrors := 0, []string{}
	for _, uid := range uids {
		a := p.cfg.Pool.Remove(uid)
		if a == nil {
			continue
		}
		if p.cfg.ProxyPool != nil {
			if err := p.cfg.ProxyPool.Unbind(uid); err != nil {
				log.Printf("panel: group delete uid=%s（代理绑定解绑失败: %v）", uid, err)
			}
		}
		if a.FilePath != "" {
			if err := os.Remove(a.FilePath); err != nil && !os.IsNotExist(err) {
				fileErrors = append(fileErrors, fmt.Sprintf("uid=%s: %v", uid, err))
			}
		}
		removed++
	}
	log.Printf("panel: 删除分组 %q，移除账号 %d 个（凭证文件删除失败 %d）", name, removed, len(fileErrors))
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":          true,
		"group":       name,
		"removed":     removed,
		"file_errors": fileErrors,
		"groups":      p.cfg.Pool.AccountGroups(),
	})
}
