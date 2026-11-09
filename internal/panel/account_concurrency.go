package panel

import (
	"encoding/json"
	"io"
	"net/http"
)

// accountConcurrency 设置单账号并发覆盖值。
// 0 = 按域默认（国内 3 / 国际 2）；1–64 = 显式上限。实时生效并落盘 state.json。
func (p *Panel) accountConcurrency(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	var body struct {
		Concurrency int `json:"concurrency"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid concurrency body")
		return
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		writeErr(w, http.StatusBadRequest, "concurrency body must contain one JSON value")
		return
	}
	if body.Concurrency < 0 || body.Concurrency > 64 {
		writeErr(w, http.StatusBadRequest, "concurrency must be 0-64")
		return
	}
	if !p.cfg.Pool.SetAccountConcurrency(uid, body.Concurrency) {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	current, limit, _ := p.cfg.Pool.AccountConcurrency(uid)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":          true,
		"concurrency": current,
		"limit":       limit,
	})
}
