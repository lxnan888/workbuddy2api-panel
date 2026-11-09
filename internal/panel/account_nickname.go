package panel

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

// accountNickname 修改账号昵称并原子写回 auth 文件。
// 空串合法（清空后前端显示“未命名”）；最长 64 字节。
func (p *Panel) accountNickname(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	a := p.cfg.Pool.AuthByUID(uid)
	if a == nil {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	var body struct {
		Nickname string `json:"nickname"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid nickname body")
		return
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		writeErr(w, http.StatusBadRequest, "nickname body must contain one JSON value")
		return
	}
	name := strings.TrimSpace(body.Nickname)
	if len([]byte(name)) > 64 {
		writeErr(w, http.StatusBadRequest, "nickname must not exceed 64 bytes")
		return
	}
	if err := p.cfg.Pool.UpdateNickname(uid, name); err != nil {
		writeErr(w, http.StatusInternalServerError, "save nickname: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "nickname": name})
}
