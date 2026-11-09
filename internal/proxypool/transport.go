package proxypool

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/net/proxy"
)

type dialFunc func(ctx context.Context, network, addr string) (net.Conn, error)

// globalProbeID 周期探测队列里的总代理占位 id（不会与池节点 id 冲突）。
const globalProbeID = "__global__"

// TLSOverFunc 在已建好的隧道上做出站 TLS 握手（由 upstream 注入，见 SetTLSOver）。
type TLSOverFunc func(ctx context.Context, raw net.Conn, addr string) (net.Conn, error)

var errProxyAuth = errors.New("proxy authentication failed")

// SetTLSOver 注入「在隧道上握手」的函数：经代理的 HTTPS 与直连用同一套 TLS 指纹、请求头顺序与流量计量。
// 不注入时由 net/http 在隧道上做原生握手（仍然经代理，只是指纹是 Go 的）。已建的连接按新口径重建。
func (m *Manager) SetTLSOver(f TLSOverFunc) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tlsOver = f
	for id, tr := range m.transports {
		tr.CloseIdleConnections()
		delete(m.transports, id)
	}
	if m.globalTransport != nil {
		m.globalTransport.CloseIdleConnections()
		m.globalTransport = nil
	}
}

func (m *Manager) transportLocked(n node) (*http.Transport, error) {
	if tr := m.transports[n.ID]; tr != nil {
		return tr, nil
	}
	u, err := parseURL(n.URL)
	if err != nil {
		return nil, err
	}
	tr, err := m.transportForURL(u, m.dialThroughGlobal)
	if err != nil {
		return nil, err
	}
	m.transports[n.ID] = tr
	return tr, nil
}

// globalTransportLocked 构造总代理自己的 Transport；连接总代理本身始终直连。
func (m *Manager) globalTransportLocked() (*http.Transport, error) {
	if m.globalTransport != nil {
		return m.globalTransport, nil
	}
	if m.globalURL == nil {
		return nil, errors.New("global proxy not configured")
	}
	tr, err := m.transportForURL(m.globalURL, m.dialer.DialContext)
	if err != nil {
		return nil, err
	}
	m.globalTransport = tr
	return tr, nil
}

// transportForURL 为单个代理构造 Transport。baseDial 负责“连到代理 host”：
// 池代理传 dialThroughGlobal（可链式），总代理传直连 dialer。
func (m *Manager) transportForURL(u *url.URL, baseDial dialFunc) (*http.Transport, error) {
	tr := m.base.Clone()
	tr.Proxy = nil
	tlsBase := m.base.TLSClientConfig
	over := m.tlsOver
	nativeTLS := func(ctx context.Context, raw net.Conn, addr string) (net.Conn, error) {
		host, _, err := net.SplitHostPort(addr)
		if err != nil {
			host = addr
		}
		cfg := &tls.Config{MinVersion: tls.VersionTLS12}
		if tlsBase != nil {
			cfg = tlsBase.Clone()
		}
		cfg.ServerName, cfg.NextProtos = host, nil
		tc := tls.Client(raw, cfg)
		if err := tc.HandshakeContext(ctx); err != nil {
			raw.Close()
			return nil, err
		}
		return tc, nil
	}
	handshake := nativeTLS
	if over != nil {
		handshake = over
	}
	if u.Scheme == "http" || u.Scheme == "https" {
		connect := m.connectTunnel(u, tlsBase, baseDial)
		// 明文 HTTP 目标走 absolute-form：连接池代理本身也必须经 baseDial（可链式总代理），
		// 否则这条路径会绕过总代理直连池代理。
		tr.DialContext = baseDial
		// HTTPS 目标不交给 net/http 的代理逻辑（它会用原生握手）；明文 HTTP 目标用标准的 absolute-form
		// 转发——不少商用代理只放行 CONNECT 到 443。
		tr.Proxy = func(r *http.Request) (*url.URL, error) {
			if r.URL.Scheme == "https" {
				return nil, nil
			}
			return u, nil
		}
		tr.DialTLSContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			if addr == u.Host {
				// https 代理承载明文 HTTP 目标：net/http 要的是到代理本身的 TLS，不是隧道。
				raw, err := baseDial(ctx, network, addr)
				if err != nil {
					return nil, err
				}
				return nativeTLS(ctx, raw, addr)
			}
			raw, err := connect(ctx, network, addr)
			if err != nil {
				return nil, err
			}
			return handshake(ctx, raw, addr)
		}
	} else {
		tunnel, err := m.socksTunnel(u, baseDial)
		if err != nil {
			return nil, err
		}
		tr.DialContext = tunnel
		tr.DialTLSContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			raw, err := tunnel(ctx, network, addr)
			if err != nil {
				return nil, err
			}
			return handshake(ctx, raw, addr) // 握手用原始主机名（socks5 在隧道里已就地解析成 IP）
		}
	}
	return tr, nil
}

// dialerAdapter 把 dialFunc 适配成 golang.org/x/net/proxy.Dialer（SOCKS5 建连用）。
type dialerAdapter struct{ fn dialFunc }

func (d dialerAdapter) Dial(network, addr string) (net.Conn, error) {
	return d.fn(context.Background(), network, addr)
}

// socksTunnel 构造经 SOCKS5/SOCKS5H 的拨号函数；baseDial 负责连到 SOCKS 代理 host。
func (m *Manager) socksTunnel(u *url.URL, baseDial dialFunc) (dialFunc, error) {
	var auth *proxy.Auth
	if u.User != nil {
		password, _ := u.User.Password()
		auth = &proxy.Auth{User: u.User.Username(), Password: password}
	}
	d, err := proxy.SOCKS5("tcp", u.Host, auth, dialerAdapter{fn: baseDial})
	if err != nil {
		return nil, errors.New("create SOCKS dialer failed")
	}
	dialer, ok := d.(proxy.ContextDialer)
	if !ok {
		return nil, errors.New("SOCKS dialer does not support context")
	}
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		if m.dialer.Timeout > 0 {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, m.dialer.Timeout)
			defer cancel()
		}
		if u.Scheme == "socks5" {
			host, port, err := net.SplitHostPort(address)
			if err != nil {
				return nil, err
			}
			if net.ParseIP(host) == nil {
				resolver := m.dialer.Resolver
				if resolver == nil {
					resolver = net.DefaultResolver
				}
				ips, err := resolver.LookupIPAddr(ctx, host)
				if err != nil {
					return nil, err
				}
				if len(ips) == 0 {
					return nil, errors.New("DNS returned no addresses")
				}
				address = net.JoinHostPort(ips[0].IP.String(), port)
			}
		}
		return dialer.DialContext(ctx, network, address)
	}, nil
}

// dialThroughGlobal 连到某个地址：总代理启用时先经总代理建隧道，否则直连。
// 用于池代理的 host 连接与健康探测，实现“本地 → 总代理 → 池代理 → 上游”链式。
func (m *Manager) dialThroughGlobal(ctx context.Context, network, addr string) (net.Conn, error) {
	m.mu.Lock()
	enabled := m.data.Global.Enabled
	var gu *url.URL
	if m.globalURL != nil {
		copy := *m.globalURL
		gu = &copy
	}
	dialer := m.dialer
	tlsBase := m.base.TLSClientConfig
	m.mu.Unlock()
	if !enabled || gu == nil {
		return dialer.DialContext(ctx, network, addr)
	}
	if gu.Scheme == "http" || gu.Scheme == "https" {
		connect := m.connectTunnel(gu, tlsBase, dialer.DialContext)
		return connect(ctx, network, addr)
	}
	tunnel, err := m.socksTunnel(gu, dialer.DialContext)
	if err != nil {
		return nil, err
	}
	return tunnel(ctx, network, addr)
}

// connectTunnel 经 HTTP / HTTPS 代理用 CONNECT 建到目标的 TCP 隧道（替代 Transport.Proxy，好在隧道上
// 用自己的 TLS 握手）。407 返回 errProxyAuth；握手阶段受 ctx 取消与截止时间约束。
func (m *Manager) connectTunnel(u *url.URL, tlsBase *tls.Config, baseDial dialFunc) dialFunc {
	auth := ""
	if u.User != nil {
		password, _ := u.User.Password()
		auth = base64.StdEncoding.EncodeToString([]byte(u.User.Username() + ":" + password))
	}
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		conn, err := baseDial(ctx, "tcp", u.Host)
		if err != nil {
			return nil, err
		}
		stop := context.AfterFunc(ctx, func() { conn.SetDeadline(time.Unix(1, 0)) })
		defer stop()
		// 建隧道最多等拨号超时：请求级截止时间可能长达整个回复（120s），只认它的话，
		// 接了 TCP 却不回 CONNECT 的代理能把请求挂满整段。
		var dl time.Time
		if m.dialer.Timeout > 0 {
			dl = time.Now().Add(m.dialer.Timeout)
		}
		if cd, ok := ctx.Deadline(); ok && (dl.IsZero() || cd.Before(dl)) {
			dl = cd
		}
		conn.SetDeadline(dl)
		if u.Scheme == "https" {
			cfg := &tls.Config{MinVersion: tls.VersionTLS12}
			if tlsBase != nil {
				cfg = tlsBase.Clone() // 与到代理本身的 TLS 同一份配置（证书校验口径一致）
			}
			cfg.ServerName, cfg.NextProtos = u.Hostname(), nil
			tc := tls.Client(conn, cfg)
			if err := tc.HandshakeContext(ctx); err != nil {
				conn.Close()
				return nil, err
			}
			conn = tc
		}
		req := "CONNECT " + addr + " HTTP/1.1\r\nHost: " + addr + "\r\n"
		if auth != "" {
			req += "Proxy-Authorization: Basic " + auth + "\r\n"
		}
		if _, err := io.WriteString(conn, req+"\r\n"); err != nil {
			conn.Close()
			return nil, err
		}
		br := bufio.NewReader(conn)
		resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
		if err != nil {
			conn.Close()
			return nil, err
		}
		resp.Body.Close()
		switch {
		case resp.StatusCode == http.StatusProxyAuthRequired:
			conn.Close()
			return nil, errProxyAuth
		case resp.StatusCode != http.StatusOK:
			conn.Close()
			return nil, fmt.Errorf("proxy CONNECT: %s", resp.Status)
		}
		if !stop() { // ctx 已取消：AfterFunc 已经或即将把截止时间设成过去
			conn.Close()
			return nil, ctx.Err()
		}
		conn.SetDeadline(time.Time{})
		if br.Buffered() > 0 {
			return &bufferedConn{Conn: conn, r: br}, nil
		}
		return conn, nil
	}
}

// bufferedConn 隧道建好时代理已多发了字节（读进了 bufio）：先吐完缓冲再读连接。
type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) { return c.r.Read(p) }

type proxyBody struct {
	io.ReadCloser
	id        string
	transport *http.Transport
	closed    atomic.Bool
}

// Read 读响应体时出了网络错误，认为经这个节点的连接可能都坏了，清掉节点的空闲连接。
// 调用方自己造成的错误不算：取消了请求，或 Close 之后又读（net/http 回「read on closed response
// body」，是未导出的错误值，所以靠自己记下是否已关闭）。误清一次就白白丢掉后面请求能复用的连接。
func (b *proxyBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err != nil && err != io.EOF {
		if b.closed.Load() || errors.Is(err, context.Canceled) {
			return n, err
		}
		b.transport.CloseIdleConnections()
		err = &Error{ProxyID: b.id, Reason: "proxy response interrupted", cause: err}
	}
	return n, err
}

func (b *proxyBody) Close() error {
	b.closed.Store(true)
	return b.ReadCloser.Close()
}

// Probe uses a separate total deadline and never carries account headers or cookies.
func (m *Manager) Probe(ctx context.Context, id string) error {
	select {
	case m.slots <- struct{}{}:
		defer func() { <-m.slots }()
	case <-ctx.Done():
		return ctx.Err()
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return errors.New("proxy pool is closed")
	}
	if m.checking[id] {
		m.mu.Unlock()
		return nil
	}
	var found *node
	for _, n := range m.data.Nodes {
		if n.ID == id {
			copy := n
			found = &copy
			break
		}
	}
	if found == nil {
		m.mu.Unlock()
		return ErrNotFound
	}
	tr, err := m.transportLocked(*found)
	if err != nil {
		m.mu.Unlock()
		return err
	}
	version, settings := m.generation[id], m.data.Settings
	m.checking[id] = true
	delete(m.requested, id)
	h := m.health[id]
	h.attemptedAt = time.Now()
	m.health[id] = h
	m.mu.Unlock()

	checkCtx, cancel := context.WithTimeout(ctx, time.Duration(settings.TimeoutSeconds)*time.Second)
	defer cancel()
	start := time.Now()
	req, _ := http.NewRequestWithContext(checkCtx, http.MethodGet, settings.URL, nil)
	client := &http.Client{Transport: tr}
	resp, checkErr := client.Do(req)
	message := ""
	if checkErr != nil {
		message = "proxy connection failed"
		if errors.Is(checkCtx.Err(), context.DeadlineExceeded) {
			message = "health check timed out"
		}
	} else {
		_ = resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 400 {
			message = "health endpoint returned HTTP " + strconv.Itoa(resp.StatusCode)
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.checking, id)
	if ctx.Err() != nil || m.closed || m.generation[id] != version {
		m.signal()
		return nil
	}
	h = m.health[id]
	h.checkedAt, h.latencyMS, h.lastError = time.Now(), time.Since(start).Milliseconds(), message
	if message == "" {
		h.failures = 0
		h.successes++
		if h.status != "unhealthy" || h.successes >= settings.SuccessThreshold {
			h.status = "healthy"
		}
		delete(m.passiveFails, id)

		delete(m.passiveAt, id)
	} else {
		h.successes = 0
		h.failures++
		if h.failures >= settings.FailureThreshold {
			h.status = "unhealthy"
			tr.CloseIdleConnections()
		}
	}
	m.health[id] = h
	return nil
}

// ProbeGlobal 用真实 GET 健康检查 URL 验证总代理全链路。
func (m *Manager) ProbeGlobal(ctx context.Context) error {
	select {
	case m.slots <- struct{}{}:
		defer func() { <-m.slots }()
	case <-ctx.Done():
		return ctx.Err()
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return errors.New("proxy pool is closed")
	}
	if m.globalURL == nil {
		m.mu.Unlock()
		return ErrNotFound
	}
	if m.globalChecking {
		m.mu.Unlock()
		return nil
	}
	tr, err := m.globalTransportLocked()
	if err != nil {
		m.mu.Unlock()
		return err
	}
	version, settings := m.globalGen, m.data.Settings
	m.globalChecking = true
	m.globalRequest = false
	h := m.globalHealth
	h.attemptedAt = time.Now()
	m.globalHealth = h
	m.mu.Unlock()

	checkCtx, cancel := context.WithTimeout(ctx, time.Duration(settings.TimeoutSeconds)*time.Second)
	defer cancel()
	start := time.Now()
	req, _ := http.NewRequestWithContext(checkCtx, http.MethodGet, settings.URL, nil)
	client := &http.Client{Transport: tr}
	resp, checkErr := client.Do(req)
	message := ""
	if checkErr != nil {
		message = "proxy connection failed"
		if errors.Is(checkCtx.Err(), context.DeadlineExceeded) {
			message = "health check timed out"
		}
	} else {
		_ = resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 400 {
			message = "health endpoint returned HTTP " + strconv.Itoa(resp.StatusCode)
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.globalChecking = false
	if ctx.Err() != nil || m.closed || m.globalGen != version {
		m.signal()
		return nil
	}
	h = m.globalHealth
	h.checkedAt, h.latencyMS, h.lastError = time.Now(), time.Since(start).Milliseconds(), message
	if message == "" {
		h.failures = 0
		h.successes++
		if h.status != "unhealthy" || h.successes >= settings.SuccessThreshold {
			h.status = "healthy"
		}
		delete(m.passiveFails, "global")

		delete(m.passiveAt, "global")
	} else {
		h.successes = 0
		h.failures++
		if h.failures >= settings.FailureThreshold {
			h.status = "unhealthy"
			tr.CloseIdleConnections()
		}
	}
	m.globalHealth = h
	return nil
}

// TestResult 一次代理测试结果（不落库）。
type TestResult struct {
	OK         bool   `json:"ok"`
	LatencyMS  int64  `json:"latency_ms"`
	StatusCode int    `json:"status_code"`
	Error      string `json:"error,omitempty"`
}

// TestURL 对任意未保存的代理输入做真实 GET；连接该代理本身仍按当前总代理链式。
func (m *Manager) TestURL(ctx context.Context, raw string) (TestResult, error) {
	u, err := parseProxyInput(raw)
	if err != nil {
		return TestResult{}, err
	}
	m.mu.Lock()
	settings := m.data.Settings
	baseDial := m.dialThroughGlobal
	m.mu.Unlock()
	tr, err := m.transportForURL(u, baseDial)
	if err != nil {
		return TestResult{}, err
	}
	defer tr.CloseIdleConnections()

	checkCtx, cancel := context.WithTimeout(ctx, time.Duration(settings.TimeoutSeconds)*time.Second)
	defer cancel()
	start := time.Now()
	req, _ := http.NewRequestWithContext(checkCtx, http.MethodGet, settings.URL, nil)
	client := &http.Client{Transport: tr}
	resp, testErr := client.Do(req)
	out := TestResult{LatencyMS: time.Since(start).Milliseconds()}
	if testErr != nil {
		out.Error = "proxy connection failed"
		if errors.Is(checkCtx.Err(), context.DeadlineExceeded) {
			out.Error = "health check timed out"
		}
		return out, nil
	}
	defer resp.Body.Close()
	out.StatusCode = resp.StatusCode
	if resp.StatusCode < 200 || resp.StatusCode >= 400 {
		out.Error = "health endpoint returned HTTP " + strconv.Itoa(resp.StatusCode)
		return out, nil
	}
	out.OK = true
	return out, nil
}

// CheckAll shares the global eight-probe bound with the periodic checker.
func (m *Manager) CheckAll(ctx context.Context) {
	s := m.Snapshot()
	jobs := make(chan string)
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for id := range jobs {
				if id == globalProbeID {
					_ = m.ProbeGlobal(ctx)
					continue
				}
				_ = m.Probe(ctx, id)
			}
		}()
	}
	for _, n := range s.Proxies {
		select {
		case jobs <- n.ID:
		case <-ctx.Done():
			close(jobs)
			wg.Wait()
			return
		}
	}
	if s.Global.Enabled && s.Global.URL != "" {
		select {
		case jobs <- globalProbeID:
		case <-ctx.Done():
			close(jobs)
			wg.Wait()
			return
		}
	}
	close(jobs)
	wg.Wait()
}

// Run is blocking and must finish before Close. Disabled-by-health nodes remain due.
func (m *Manager) Run(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	jobs := make(chan string)
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for id := range jobs {
				if id == globalProbeID {
					_ = m.ProbeGlobal(ctx)
					continue
				}
				_ = m.Probe(ctx, id)
			}
		}()
	}
	defer func() { close(jobs); wg.Wait() }()
	for {
		m.mu.Lock()
		var due []string
		for _, n := range m.data.Nodes {
			if !m.checking[n.ID] && (m.requested[n.ID] || (n.Enabled && time.Since(m.health[n.ID].attemptedAt) >= time.Duration(m.data.Settings.IntervalSeconds)*time.Second)) {
				due = append(due, n.ID)
			}
		}
		if m.globalURL != nil && m.data.Global.Enabled && !m.globalChecking &&
			(m.globalRequest || m.globalHealth.attemptedAt.IsZero() ||
				time.Since(m.globalHealth.attemptedAt) >= time.Duration(m.data.Settings.IntervalSeconds)*time.Second) {
			due = append(due, globalProbeID)
		}
		m.mu.Unlock()
		for _, id := range due {
			select {
			case jobs <- id:
			case <-ctx.Done():
				return
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-m.wake:
		}
	}
}
