package panel

import (
	"net/http"
	"strconv"
	"time"
)

// balanceHistory 返回最近 N 天余额趋势采样（默认 30 天）。
func (p *Panel) balanceHistory(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Balance == nil {
		writeErr(w, http.StatusNotImplemented, "balance history not wired")
		return
	}
	days := 30
	if v := r.URL.Query().Get("days"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 365 {
			days = n
		}
	}
	series, updated := p.cfg.Balance.Snapshot(time.Now().AddDate(0, 0, -days))
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":         true,
		"series":     series,
		"updated_at": updated,
	})
}
