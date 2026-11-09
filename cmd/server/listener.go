package main

import (
	"context"
	"net"
	"net/http"
	"sync"
	"time"
)

// serverRunner 持有当前 HTTP server/listener，支持 listen 与 read_timeout 热切换。
// 切换时只关闭旧 listener（不再接新连接），已建立的连接由旧 server 继续服务到结束。
type serverRunner struct {
	mu          sync.Mutex
	srv         *http.Server
	ln          net.Listener
	handler     http.Handler
	addr        string
	readTimeout time.Duration
}

func newHTTPServer(addr string, handler http.Handler, readTimeout time.Duration) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 30 * time.Second,
		ReadTimeout:       readTimeout,
		IdleTimeout:       120 * time.Second,
	}
}

// Start 首次启动监听。
func (r *serverRunner) Start(handler http.Handler, cfg *Config) error {
	r.mu.Lock()
	r.handler = handler
	r.mu.Unlock()
	return r.apply(cfg.Listen, cfg.ServerReadTimeoutDur)
}

// Reconfigure 在 listen/read_timeout 变化时进程内重建 listener 与 server。
func (r *serverRunner) Reconfigure(cfg *Config) error {
	r.mu.Lock()
	addr, timeout := r.addr, r.readTimeout
	r.mu.Unlock()
	if addr == cfg.Listen && timeout == cfg.ServerReadTimeoutDur {
		return nil
	}
	return r.apply(cfg.Listen, cfg.ServerReadTimeoutDur)
}

func (r *serverRunner) apply(addr string, readTimeout time.Duration) error {
	r.mu.Lock()
	oldLn := r.ln
	handler := r.handler
	oldAddr, oldTimeout := r.addr, r.readTimeout
	r.mu.Unlock()

	if oldLn != nil {
		_ = oldLn.Close()
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		// 新地址绑定失败：尝试恢复旧监听，避免配置保存把服务打没。
		if oldLn != nil {
			if l2, e2 := net.Listen("tcp", oldAddr); e2 == nil {
				srv2 := newHTTPServer(oldAddr, handler, oldTimeout)
				r.mu.Lock()
				r.srv, r.ln = srv2, l2
				r.mu.Unlock()
				go func() { _ = srv2.Serve(l2) }()
			}
		}
		return err
	}
	srv := newHTTPServer(addr, handler, readTimeout)
	r.mu.Lock()
	r.srv, r.ln, r.addr, r.readTimeout = srv, ln, addr, readTimeout
	r.mu.Unlock()
	go func() { _ = srv.Serve(ln) }()
	return nil
}

// Shutdown 优雅停止当前 server。
func (r *serverRunner) Shutdown(ctx context.Context) {
	r.mu.Lock()
	srv, ln := r.srv, r.ln
	r.srv, r.ln = nil, nil
	r.mu.Unlock()
	if ln != nil {
		_ = ln.Close()
	}
	if srv != nil {
		_ = srv.Shutdown(ctx)
	}
}
