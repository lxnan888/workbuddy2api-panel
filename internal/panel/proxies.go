package panel

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/linguo2625469/workbuddy2api-panel/internal/proxypool"
)

func (p *Panel) proxySnapshot() any {
	if p.cfg.ProxyPool == nil {
		return nil
	}
	return p.cfg.ProxyPool.Snapshot()
}

func (p *Panel) proxyRoutes() {
	p.mux.HandleFunc("GET /panel/api/proxies", p.withAuth(p.proxies))
	p.mux.HandleFunc("POST /panel/api/proxies", p.withAuth(p.proxyImport))
	p.mux.HandleFunc("POST /panel/api/proxies/test", p.withAuth(p.proxyTest))
	p.mux.HandleFunc("POST /panel/api/proxies/check", p.withAuth(p.proxyCheck))
	p.mux.HandleFunc("PUT /panel/api/proxies/global", p.withAuth(p.proxyGlobal))
	p.mux.HandleFunc("POST /panel/api/proxies/global/check", p.withAuth(p.proxyGlobalCheck))
	p.mux.HandleFunc("PATCH /panel/api/proxies/{id}", p.withAuth(p.proxyUpdate))
	p.mux.HandleFunc("DELETE /panel/api/proxies/{id}", p.withAuth(p.proxyDelete))
	p.mux.HandleFunc("POST /panel/api/proxies/{id}/check", p.withAuth(p.proxyCheck))
	p.mux.HandleFunc("PUT /panel/api/proxies/health", p.withAuth(p.proxyHealth))
	p.mux.HandleFunc("PUT /panel/api/proxies/random_bind_avoid", p.withAuth(p.proxyRandomBindAvoid))
	p.mux.HandleFunc("PUT /panel/api/accounts/{uid}/proxy", p.withAuth(p.accountProxy))
	p.mux.HandleFunc("PUT /panel/api/proxies/realm/{realm}", p.withAuth(p.realmProxy))
}

func (p *Panel) proxyReady(w http.ResponseWriter) bool {
	if p.cfg.ProxyPool == nil {
		writeErr(w, http.StatusNotImplemented, "proxy pool not available")
		return false
	}
	return true
}

func proxyBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid proxy request body")
		return false
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		writeErr(w, http.StatusBadRequest, "proxy request body must contain one JSON value")
		return false
	}
	return true
}

func proxyError(w http.ResponseWriter, err error) {
	status := http.StatusBadRequest
	if errors.Is(err, proxypool.ErrNotFound) {
		status = http.StatusNotFound
	} else if errors.Is(err, proxypool.ErrBound) {
		status = http.StatusConflict
	}
	writeErr(w, status, err.Error())
}

func (p *Panel) proxies(w http.ResponseWriter, r *http.Request) {
	if p.proxyReady(w) {
		writeJSON(w, http.StatusOK, p.proxySnapshot())
	}
}

func (p *Panel) proxyImport(w http.ResponseWriter, r *http.Request) {
	if !p.proxyReady(w) {
		return
	}
	var body struct {
		Lines string `json:"lines"`
	}
	if !proxyBody(w, r, &body) {
		return
	}
	results, err := p.cfg.ProxyPool.Import(body.Lines)
	if err != nil {
		proxyError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "results": results})
}

func (p *Panel) proxyTest(w http.ResponseWriter, r *http.Request) {
	if !p.proxyReady(w) {
		return
	}
	var body struct {
		URL string `json:"url"`
	}
	if !proxyBody(w, r, &body) {
		return
	}
	result, err := p.cfg.ProxyPool.TestURL(r.Context(), body.URL)
	if err != nil {
		proxyError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (p *Panel) proxyUpdate(w http.ResponseWriter, r *http.Request) {
	if !p.proxyReady(w) {
		return
	}
	var body proxypool.Input
	if !proxyBody(w, r, &body) {
		return
	}
	if err := p.cfg.ProxyPool.Update(r.PathValue("id"), body); err != nil {
		proxyError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (p *Panel) proxyDelete(w http.ResponseWriter, r *http.Request) {
	if !p.proxyReady(w) {
		return
	}
	if err := p.cfg.ProxyPool.Delete(r.PathValue("id")); err != nil {
		proxyError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (p *Panel) proxyCheck(w http.ResponseWriter, r *http.Request) {
	if !p.proxyReady(w) {
		return
	}
	if err := p.cfg.ProxyPool.RequestCheck(r.PathValue("id")); err != nil {
		proxyError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"ok": true, "queued": true})
}

func (p *Panel) proxyHealth(w http.ResponseWriter, r *http.Request) {
	if !p.proxyReady(w) {
		return
	}
	var body proxypool.Settings
	if !proxyBody(w, r, &body) {
		return
	}
	if err := p.cfg.ProxyPool.Configure(body); err != nil {
		proxyError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// proxyRandomBindAvoid 保存「随机出口避开账号绑定代理」模式（fallback/strict）。
// 写入 proxy-pool.json，下一次选号立即生效，无需重启。
func (p *Panel) proxyRandomBindAvoid(w http.ResponseWriter, r *http.Request) {
	if !p.proxyReady(w) {
		return
	}
	var body struct {
		Mode string `json:"mode"`
	}
	if !proxyBody(w, r, &body) {
		return
	}
	if err := p.cfg.ProxyPool.SetRandomBindAvoid(body.Mode); err != nil {
		proxyError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "mode": p.cfg.ProxyPool.RandomBindAvoid()})
}

// proxyGlobal 保存容器级总代理（存 proxy-pool.json，不写 config.json）。
func (p *Panel) proxyGlobal(w http.ResponseWriter, r *http.Request) {
	if !p.proxyReady(w) {
		return
	}
	var body proxypool.GlobalConfig
	if !proxyBody(w, r, &body) {
		return
	}
	if err := p.cfg.ProxyPool.ConfigureGlobal(body.URL, body.Enabled); err != nil {
		proxyError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (p *Panel) proxyGlobalCheck(w http.ResponseWriter, r *http.Request) {
	if !p.proxyReady(w) {
		return
	}
	if err := p.cfg.ProxyPool.RequestGlobalCheck(); err != nil {
		proxyError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"ok": true, "queued": true})
}

// realmProxy 设置 realm 级默认出口：账号未显式绑定时取它。
func (p *Panel) realmProxy(w http.ResponseWriter, r *http.Request) {
	if !p.proxyReady(w) {
		return
	}
	p.proxyAccountMu.Lock()
	defer p.proxyAccountMu.Unlock()
	realm := r.PathValue("realm")
	if realm != "cn" && realm != "global" {
		writeErr(w, http.StatusBadRequest, "realm must be cn or global")
		return
	}
	var body proxypool.Binding
	if !proxyBody(w, r, &body) {
		return
	}
	if err := p.cfg.ProxyPool.BindRealm(realm, body); err != nil {
		proxyError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (p *Panel) accountProxy(w http.ResponseWriter, r *http.Request) {
	if !p.proxyReady(w) {
		return
	}
	p.proxyAccountMu.Lock()
	defer p.proxyAccountMu.Unlock()
	uid := r.PathValue("uid")
	if p.cfg.Pool.AuthByUID(uid) == nil {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	var body proxypool.Binding
	if !proxyBody(w, r, &body) {
		return
	}
	if err := p.cfg.ProxyPool.Bind(uid, body); err != nil {
		proxyError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
