package panel

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
)

// accountPriority 设置单账号优先级。
// 0 = 默认；1–9999 = 数字越大越优先。实时生效并落盘 state.json。
func (p *Panel) accountPriority(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	var body struct {
		Priority int `json:"priority"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid priority body")
		return
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		writeErr(w, http.StatusBadRequest, "priority body must contain one JSON value")
		return
	}
	if body.Priority < 0 || body.Priority > pool.MaxAccountPriority {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("priority must be 0-%d", pool.MaxAccountPriority))
		return
	}
	if !p.cfg.Pool.SetAccountPriority(uid, body.Priority) {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	current, _ := p.cfg.Pool.AccountPriority(uid)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"priority": current,
	})
}
