package panel

import (
	"context"
	"net/http"
	"time"
)

// updateStatus 返回在线更新状态。
func (p *Panel) updateStatus(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Updater == nil {
		writeErr(w, http.StatusNotImplemented, "updater not wired")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "status": p.cfg.Updater.Status()})
}

// updateCheck 手动检查新版本。
func (p *Panel) updateCheck(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Updater == nil {
		writeErr(w, http.StatusNotImplemented, "updater not wired")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if err := p.cfg.Updater.Check(ctx); err != nil {
		writeErr(w, http.StatusBadGateway, "check update: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "status": p.cfg.Updater.Status()})
}

// updateInstall 下载并安装最新版本（成功后由 updater 自行重启）。
func (p *Panel) updateInstall(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Updater == nil {
		writeErr(w, http.StatusNotImplemented, "updater not wired")
		return
	}
	if err := p.cfg.Updater.Install(r.Context()); err != nil {
		writeErr(w, http.StatusBadGateway, "install update: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "status": p.cfg.Updater.Status()})
}
