// Package proxypool manages persistent account routes and independently probed proxies.
package proxypool

import (
	"context"
	cryptorand "crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/safefile"
)

var (
	ErrNotFound = errors.New("proxy not found")
	ErrBound    = errors.New("proxy is bound to accounts; remove bindings first")
)

// Error identifies a route failure without exposing proxy credentials.
type Error struct {
	ProxyID string
	Reason  string
	Blocked bool
	cause   error
}

func (e *Error) Error() string { return "proxy route: " + e.Reason }
func (e *Error) Unwrap() error { return e.cause }

func IsError(err error) bool {
	var e *Error
	return errors.As(err, &e)
}

func IsBlockedError(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.Blocked
}

type Settings struct {
	URL              string `json:"url"`
	IntervalSeconds  int    `json:"interval_seconds"`
	TimeoutSeconds   int    `json:"timeout_seconds"`
	FailureThreshold int    `json:"failure_threshold"`
	SuccessThreshold int    `json:"success_threshold"`
}

// GlobalConfig 容器级总代理：账号出站的兜底出口；有池代理时，
// 连接池代理本身也经它转发（本地 → 总代理 → 池代理 → 上游）。
type GlobalConfig struct {
	URL     string `json:"url"`
	Enabled bool   `json:"enabled"`
}

// GlobalView 总代理的脱敏快照（供面板展示）。
type GlobalView struct {
	URL          string    `json:"url"`
	Protocol     string    `json:"protocol"`
	PasswordSet  bool      `json:"password_set"`
	Enabled      bool      `json:"enabled"`
	Status       string    `json:"status"`
	Checking     bool      `json:"checking"`
	LatencyMS    int64     `json:"latency_ms"`
	CheckedAt    time.Time `json:"checked_at"`
	LastError    string    `json:"last_error"`
	BlockedUntil time.Time `json:"blocked_until,omitempty"`
}

// Route 一次出站选择的实际出口；Proxied=false 且 Global=false 表示直连。
type Route struct {
	ProxyID string
	Proxied bool
	Global  bool
}

func DefaultSettings() Settings {
	return Settings{"https://www.workbuddy.cn/", 60, 10, 3, 2}
}

func (s Settings) validate() error {
	u, err := url.Parse(s.URL)
	if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.Fragment != "" {
		return errors.New("health URL must be an HTTP(S) URL without credentials or fragment")
	}
	if s.IntervalSeconds < 1 || s.IntervalSeconds > 86400 || s.TimeoutSeconds < 1 || s.TimeoutSeconds > 300 ||
		s.FailureThreshold < 1 || s.FailureThreshold > 100 || s.SuccessThreshold < 1 || s.SuccessThreshold > 100 {
		return errors.New("health interval must be 1-86400s, timeout 1-300s, thresholds 1-100")
	}
	return nil
}

type Binding struct {
	Mode    string `json:"mode"`
	ProxyID string `json:"proxy_id,omitempty"`
}

// realmKey 把 realm 归一化成配置键；只有 cn / global 合法，其余返回空（= 不匹配）。
func realmKey(realm string) string {
	if realm == "cn" || realm == "global" {
		return realm
	}
	return ""
}

type node struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	URL     string `json:"url"`
	Enabled bool   `json:"enabled"`
}

type disk struct {
	Nodes    []node             `json:"proxies"`
	Bindings map[string]Binding `json:"bindings"`
	// Realms realm 级默认出口：cn/global → 绑定，账号没有显式绑定时取它。
	// cn/global 的上游域名与网络环境不同，运维通常要两个出口。
	Realms   map[string]Binding `json:"realms,omitempty"`
	Settings Settings           `json:"health"`
	// RandomBindAvoid 未绑定账号选「随机」时如何处理「已被账号级 fixed 绑定」的代理：
	//   "fallback"（默认/零值）：优先只用未绑定的健康代理，没有则退回全池健康代理随机；
	//   "strict"：绝不占用已绑定代理，没有空闲健康代理时走总代理/直连兜底。
	// 写入 proxy-pool.json，与 config.json 无关。
	RandomBindAvoid string `json:"random_bind_avoid,omitempty"`
	// Global 容器级总代理；不写 config.json，随代理池文件一起持久化。
	Global GlobalConfig `json:"global,omitempty"`
}

type health struct {
	status      string
	failures    int
	successes   int
	checkedAt   time.Time
	attemptedAt time.Time
	latencyMS   int64
	lastError   string
}

type View struct {
	ID           string    `json:"id"`
	Name         string    `json:"name"`
	URL          string    `json:"url"`
	Protocol     string    `json:"protocol"`
	PasswordSet  bool      `json:"password_set"`
	Enabled      bool      `json:"enabled"`
	Status       string    `json:"status"`
	Checking     bool      `json:"checking"`
	LatencyMS    int64     `json:"latency_ms"`
	CheckedAt    time.Time `json:"checked_at"`
	LastError    string    `json:"last_error"`
	Failures     int       `json:"failures"`
	Successes    int       `json:"successes"`
	BoundCount   int       `json:"bound_count"`
	BlockedUntil time.Time `json:"blocked_until,omitempty"`
}

type Snapshot struct {
	Proxies         []View             `json:"proxies"`
	Bindings        map[string]Binding `json:"bindings"`
	Realms          map[string]Binding `json:"realms"`
	Health          Settings           `json:"health"`
	RandomBindAvoid string             `json:"random_bind_avoid"`
	Global          GlobalView         `json:"global"`
}

type Input struct {
	Name     string `json:"name"`
	URL      string `json:"url"`
	Password string `json:"password"`
	Enabled  *bool  `json:"enabled"`
}

type ImportResult struct {
	Line   int    `json:"line"`
	Status string `json:"status"`
	ID     string `json:"id,omitempty"`
	Error  string `json:"error,omitempty"`
}

type Manager struct {
	mu         sync.Mutex
	path       string
	data       disk
	health     map[string]health
	generation map[string]uint64
	checking   map[string]bool
	requested  map[string]bool
	transports map[string]*http.Transport
	base       *http.Transport
	dialer     *net.Dialer
	tlsOver    TLSOverFunc // 经代理的 HTTPS 用它在隧道上握手（SetTLSOver）；nil = net/http 原生握手
	closed     bool
	wake       chan struct{}
	// Shared by periodic and manual probes, including concurrent HTTP handlers.
	slots chan struct{}

	// 总代理运行态。globalURL 为空 = 未配置；globalHealth 与池节点同口径。
	globalURL       *url.URL
	globalHealth    health
	globalChecking  bool
	globalRequest   bool
	globalGen       uint64
	globalTransport *http.Transport

	// 被动失败与 WAF 出口屏蔽：代理请求失败不再只等下一轮健康探测。
	passiveFails  map[string]int
	passiveAt     map[string]time.Time
	blocked       map[string]time.Time
	globalBlocked time.Time
}

func New(path string, base *http.Transport, dialer *net.Dialer) (*Manager, error) {
	if base == nil {
		base = http.DefaultTransport.(*http.Transport)
	}
	if dialer == nil {
		dialer = &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 15 * time.Second}
	}
	m := &Manager{
		path: path, base: base.Clone(), dialer: dialer,
		data:   disk{Nodes: []node{}, Bindings: map[string]Binding{}, Realms: map[string]Binding{}, Settings: DefaultSettings()},
		health: map[string]health{}, generation: map[string]uint64{}, checking: map[string]bool{}, requested: map[string]bool{},
		transports: map[string]*http.Transport{}, wake: make(chan struct{}, 1), slots: make(chan struct{}, 8),
		passiveFails: map[string]int{}, passiveAt: map[string]time.Time{}, blocked: map[string]time.Time{},
	}
	if path != "" {
		raw, err := safefile.Read(path, nil)
		if err != nil && !os.IsNotExist(err) {
			return nil, err
		}
		if err == nil {
			if err := json.Unmarshal(raw, &m.data); err != nil {
				return nil, fmt.Errorf("parse proxy pool: %w", err)
			}
		}
	}
	if err := m.data.Settings.validate(); err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for i := range m.data.Nodes {
		n := &m.data.Nodes[i]
		u, err := parseURL(n.URL)
		if err != nil || n.ID == "" || m.generation[n.ID] != 0 || seen[uString(u)] {
			return nil, errors.New("invalid or duplicate proxy in stored pool")
		}
		n.URL = u.String()
		seen[n.URL] = true
		m.generation[n.ID] = 1
	}
	if m.data.Bindings == nil {
		m.data.Bindings = map[string]Binding{}
	}
	if m.data.Realms == nil {
		m.data.Realms = map[string]Binding{}
	}
	for _, b := range m.data.Bindings {
		if err := m.validateBindingLocked(b); err != nil {
			return nil, err
		}
	}
	for realm, b := range m.data.Realms {
		if realmKey(realm) == "" {
			return nil, errors.New("invalid realm binding in stored pool")
		}
		if err := m.validateBindingLocked(b); err != nil {
			return nil, err
		}
	}
	if m.data.Global.URL != "" || m.data.Global.Enabled {
		if strings.TrimSpace(m.data.Global.URL) == "" {
			return nil, errors.New("global proxy is enabled but URL is empty")
		}
		u, err := parseProxyInput(m.data.Global.URL)
		if err != nil {
			return nil, fmt.Errorf("global proxy: %w", err)
		}
		m.globalURL = u
		m.data.Global.URL = u.String()
		m.globalGen = 1
	}
	return m, nil
}

func uString(u *url.URL) string {
	if u == nil {
		return ""
	}
	return u.String()
}

// parseProxyInput 解析一行代理输入。支持：
//
//	http://[user:pass@]host:port
//	https://[user:pass@]host:port
//	socks5://[user:pass@]host:port
//	socks5h://[user:pass@]host:port
//	host:port:user:pass
//	user:pass@host:port
//	host:port@user:pass
//	host:port
//
// 无 scheme 一律按 HTTP 代理处理。密码里的冒号在 host:port:user:pass 与
// user:pass@host:port 中可保留（分别用 SplitN 4 / 首个冒号切分）；密码里
// 同时含 @ 的极端情况建议使用 URL 形式。
func parseProxyInput(raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, errors.New("empty proxy")
	}
	if strings.Contains(raw, "://") {
		u, err := url.Parse(raw)
		if err != nil || u == nil {
			return nil, errors.New("invalid proxy URL")
		}
		return normalizeProxyURL(u)
	}

	// 无 scheme 的 @ 形式：按“更像地址”的一侧判 host。
	if at := strings.LastIndex(raw, "@"); at > 0 && at < len(raw)-1 {
		left, right := raw[:at], raw[at+1:]
		lh, lp, lok := hostPortParts(left)
		rh, rp, rok := hostPortParts(right)
		lCred := validUserPass(left)
		rCred := validUserPass(right)
		switch {
		case lok && rCred && (looksLikeHost(lh) || !rok || !looksLikeHost(rh)):
			return buildProxyURL(lh, lp, right)
		case rok && lCred:
			return buildProxyURL(rh, rp, left)
		case lok && rCred:
			return buildProxyURL(lh, lp, right)
		default:
			return nil, errors.New("proxy must be user:pass@host:port or host:port@user:pass")
		}
	}

	// IPv6 字面量用方括号：先剥出 host，再按剩余段解析 port[:user:pass]。
	if strings.HasPrefix(raw, "[") {
		if end := strings.Index(raw, "]"); end > 0 {
			host := raw[1:end]
			rest := strings.TrimPrefix(raw[end+1:], ":")
			parts := strings.SplitN(rest, ":", 3)
			switch len(parts) {
			case 1:
				return buildProxyURL(host, parts[0], "")
			case 3:
				return buildProxyURL(host, parts[0], parts[1]+":"+parts[2])
			}
		}
	}

	parts := strings.SplitN(raw, ":", 4)
	switch len(parts) {
	case 2:
		return buildProxyURL(parts[0], parts[1], "")
	case 4:
		return buildProxyURL(parts[0], parts[1], parts[2]+":"+parts[3])
	default:
		return nil, errors.New("proxy must be host:port, host:port:user:pass, user:pass@host:port or host:port@user:pass")
	}
}

// parseURL 解析并规范化已存储/已输入的代理 URL。
func parseURL(raw string) (*url.URL, error) { return parseProxyInput(raw) }

func normalizeProxyURL(u *url.URL) (*url.URL, error) {
	u.Scheme = strings.ToLower(strings.TrimSpace(u.Scheme))
	defaultPort := map[string]string{"http": "80", "https": "443", "socks5": "1080", "socks5h": "1080"}[u.Scheme]
	if defaultPort == "" || u.Hostname() == "" || u.Opaque != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" ||
		(u.Path != "" && u.Path != "/") || (u.User != nil && u.User.Username() == "") {
		return nil, errors.New("proxy requires http/https/socks5/socks5h, host and optional credentials; paths and queries are unsupported")
	}
	port := u.Port()
	if port == "" {
		port = defaultPort
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return nil, errors.New("proxy port must be 1-65535")
	}
	u.Host = net.JoinHostPort(strings.ToLower(u.Hostname()), strconv.Itoa(n))
	u.Path, u.RawPath = "", ""
	return u, nil
}

// hostPortParts 解析 host:port（支持 [IPv6]:port）。
func hostPortParts(s string) (host, port string, ok bool) {
	host, port, err := net.SplitHostPort(strings.TrimSpace(s))
	if err != nil {
		return "", "", false
	}
	host = strings.Trim(host, "[]")
	if host == "" {
		return "", "", false
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return "", "", false
	}
	return host, strconv.Itoa(n), true
}

// validUserPass 判断是否为 user:pass（用户名非空；密码可为空）。
func validUserPass(s string) bool {
	i := strings.IndexByte(s, ':')
	return i > 0
}

// looksLikeHost 用域名/IP 的常见形态消歧 @ 两侧。
func looksLikeHost(host string) bool {
	host = strings.ToLower(strings.TrimSpace(host))
	return host == "localhost" || strings.Contains(host, ".") || strings.Contains(host, ":") || net.ParseIP(host) != nil
}

// buildProxyURL 组装默认 http 的代理 URL。
func buildProxyURL(host, port, creds string) (*url.URL, error) {
	host = strings.TrimSpace(host)
	port = strings.TrimSpace(port)
	if host == "" {
		return nil, errors.New("proxy host is empty")
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return nil, errors.New("proxy port must be 1-65535")
	}
	u := &url.URL{Scheme: "http", Host: net.JoinHostPort(strings.ToLower(strings.Trim(host, "[]")), strconv.Itoa(n))}
	if creds != "" {
		i := strings.IndexByte(creds, ':')
		if i <= 0 {
			return nil, errors.New("proxy credentials must be user:pass")
		}
		u.User = url.UserPassword(creds[:i], creds[i+1:])
	}
	return normalizeProxyURL(u)
}

func (m *Manager) copyLocked() disk {
	d := m.data
	d.Nodes = append([]node{}, d.Nodes...)
	d.Bindings = make(map[string]Binding, len(m.data.Bindings))
	for k, v := range m.data.Bindings {
		d.Bindings[k] = v
	}
	return d
}

func (m *Manager) commitLocked(d disk) error {
	if m.closed {
		return errors.New("proxy pool is closed")
	}
	if m.path != "" {
		raw, err := json.MarshalIndent(d, "", "  ")
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(m.path), 0o700); err != nil {
			return err
		}
		if err := safefile.WriteKeep(m.path, raw, 0o600); err != nil {
			return err
		}
	}
	m.data = d
	m.signal()
	return nil
}

func (m *Manager) signal() {
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

func (m *Manager) Snapshot() Snapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	d := m.copyLocked()
	mode := d.RandomBindAvoid
	if mode != "strict" {
		mode = "fallback" // 零值/非法值 = 默认
	}
	s := Snapshot{Proxies: []View{}, Bindings: d.Bindings, Realms: d.Realms, Health: d.Settings, RandomBindAvoid: mode}
	counts := map[string]int{}
	for _, b := range d.Bindings {
		if b.Mode == "fixed" {
			counts[b.ProxyID]++
		}
	}
	for _, n := range d.Nodes {
		h := m.health[n.ID]
		status := h.status
		if status == "" {
			status = "pending"
		}
		if !n.Enabled {
			status = "disabled"
		}
		u, _ := parseURL(n.URL)
		passwordSet := false
		if u.User != nil {
			_, passwordSet = u.User.Password()
			u.User = url.User(u.User.Username())
		}
		s.Proxies = append(s.Proxies, View{
			ID: n.ID, Name: n.Name, URL: u.String(), Protocol: u.Scheme, PasswordSet: passwordSet,
			Enabled: n.Enabled, Status: status, Checking: m.checking[n.ID], LatencyMS: h.latencyMS,
			CheckedAt: h.checkedAt, LastError: h.lastError, Failures: h.failures, Successes: h.successes,
			BoundCount: counts[n.ID], BlockedUntil: m.blocked[n.ID],
		})
	}
	s.Global = m.globalViewLocked()
	return s
}

// globalViewLocked 生成总代理脱敏快照；调用方需持 m.mu。
func (m *Manager) globalViewLocked() GlobalView {
	v := GlobalView{Enabled: m.data.Global.Enabled, BlockedUntil: m.globalBlocked}
	if m.globalURL == nil {
		v.Status = "disabled"
		return v
	}
	u := *m.globalURL
	if u.User != nil {
		_, v.PasswordSet = u.User.Password()
		u.User = url.User(u.User.Username())
	}
	v.URL, v.Protocol = u.String(), u.Scheme
	h := m.globalHealth
	v.Status = h.status
	if v.Status == "" {
		v.Status = "pending"
	}
	if !v.Enabled {
		v.Status = "disabled"
	}
	v.Checking, v.LatencyMS, v.CheckedAt, v.LastError = m.globalChecking, h.latencyMS, h.checkedAt, h.lastError
	return v
}

func (m *Manager) Import(text string) ([]ImportResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	d := m.copyLocked()
	seen := map[string]string{}
	for _, n := range d.Nodes {
		seen[n.URL] = n.ID
	}
	results := []ImportResult{}
	var added []string
	for i, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		r := ImportResult{Line: i + 1, Status: "invalid"}
		u, err := parseProxyInput(line)
		if err != nil {
			r.Error = err.Error()
		} else if id := seen[u.String()]; id != "" {
			r.Status, r.ID = "duplicate", id
		} else {
			var b [16]byte
			if _, err := cryptorand.Read(b[:]); err != nil {
				return nil, err
			}
			id := hex.EncodeToString(b[:])
			d.Nodes = append(d.Nodes, node{ID: id, Name: u.Host, URL: u.String(), Enabled: true})
			seen[u.String()] = id
			added = append(added, id)
			r.Status, r.ID = "added", id
		}
		results = append(results, r)
	}
	if len(added) > 0 {
		if err := m.commitLocked(d); err != nil {
			return nil, err
		}
		for _, id := range added {
			m.generation[id] = 1
		}
	}
	return results, nil
}

func (m *Manager) Update(id string, in Input) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	d := m.copyLocked()
	for i, n := range d.Nodes {
		if n.ID != id {
			continue
		}
		u, _ := parseURL(n.URL)
		if in.URL != "" {
			var err error
			u, err = parseProxyInput(in.URL)
			if err != nil {
				return err
			}
			if u.User != nil {
				old, _ := parseURL(n.URL)
				_, supplied := u.User.Password()
				if !supplied && old.User != nil {
					if password, ok := old.User.Password(); ok {
						u.User = url.UserPassword(u.User.Username(), password)
					}
				}
			}
		}
		if in.Password != "" {
			if u.User == nil {
				return errors.New("password requires a username in the proxy URL")
			}
			u.User = url.UserPassword(u.User.Username(), in.Password)
		}
		for _, other := range d.Nodes {
			if other.ID != id && other.URL == u.String() {
				return errors.New("duplicate proxy URL")
			}
		}
		if in.Name != "" {
			if len(in.Name) > 128 {
				return errors.New("proxy name must not exceed 128 bytes")
			}
			d.Nodes[i].Name = strings.TrimSpace(in.Name)
		}
		d.Nodes[i].URL = u.String()
		if in.Enabled != nil {
			d.Nodes[i].Enabled = *in.Enabled
		}
		reset := d.Nodes[i].URL != n.URL || d.Nodes[i].Enabled != n.Enabled
		if err := m.commitLocked(d); err != nil {
			return err
		}
		if reset {
			m.resetLocked(id)
		}
		return nil
	}
	return ErrNotFound
}

func (m *Manager) resetLocked(id string) {
	m.generation[id]++
	delete(m.health, id)
	delete(m.blocked, id)
	delete(m.passiveFails, id)
	delete(m.passiveAt, id)
	if tr := m.transports[id]; tr != nil {
		tr.CloseIdleConnections()
		delete(m.transports, id)
	}
}

func (m *Manager) Delete(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, b := range m.data.Bindings {
		if b.Mode == "fixed" && b.ProxyID == id {
			return ErrBound
		}
	}
	for _, b := range m.data.Realms {
		if b.Mode == "fixed" && b.ProxyID == id {
			return ErrBound
		}
	}
	d := m.copyLocked()
	for i, n := range d.Nodes {
		if n.ID == id {
			d.Nodes = append(d.Nodes[:i], d.Nodes[i+1:]...)
			if err := m.commitLocked(d); err != nil {
				return err
			}
			m.resetLocked(id)
			return nil
		}
	}
	return ErrNotFound
}

func (m *Manager) validateBindingLocked(b Binding) error {
	switch b.Mode {
	case "random", "direct":
		if b.ProxyID != "" {
			return errors.New("proxy_id is only valid in fixed mode")
		}
	case "fixed":
		for _, n := range m.data.Nodes {
			if n.ID == b.ProxyID {
				return nil
			}
		}
		return ErrNotFound
	default:
		return errors.New("proxy mode must be random, direct or fixed")
	}
	return nil
}

func (m *Manager) Bind(uid string, b Binding) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.validateBindingLocked(b); err != nil {
		return err
	}
	d := m.copyLocked()
	if b.Mode == "random" {
		delete(d.Bindings, uid)
	} else {
		d.Bindings[uid] = b
	}
	return m.commitLocked(d)
}

func (m *Manager) Unbind(uid string) error { return m.Bind(uid, Binding{Mode: "random"}) }

// BindRealm 设置 realm 级默认出口（issue #39 P3）。random = 恢复「随机池」，
// 等价于删掉这条默认；direct = 该域直连。账号级绑定优先于它。
func (m *Manager) BindRealm(realm string, b Binding) error {
	if realmKey(realm) == "" {
		return errors.New("realm must be cn or global")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.validateBindingLocked(b); err != nil {
		return err
	}
	d := m.copyLocked()
	if d.Realms == nil {
		d.Realms = map[string]Binding{}
	}
	if b.Mode == "random" {
		delete(d.Realms, realmKey(realm))
	} else {
		d.Realms[realmKey(realm)] = b
	}
	return m.commitLocked(d)
}

// RequestCheck queues probes on the service lifecycle, independent of browser disconnects.
// An empty ID requests all nodes, including manually disabled nodes.
func (m *Manager) RequestCheck(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if id == "global" {
		if m.globalURL == nil {
			return ErrNotFound
		}
		m.globalRequest = true
		m.signal()
		return nil
	}
	found := id == ""
	for _, n := range m.data.Nodes {
		if id == "" || n.ID == id {
			m.requested[n.ID] = true
			found = true
		}
	}
	if id == "" && m.globalURL != nil {
		m.globalRequest = true
	}
	if !found {
		return ErrNotFound
	}
	m.signal()
	return nil
}

// SetRandomBindAvoid 设置「随机出口避开账号绑定代理」模式：
// "fallback"（默认）= 优先空闲代理、没有则回退全池；"strict" = 绝不占用绑定代理。
// 非法/空值按 fallback 处理。变更立即生效并持久化到 proxy-pool.json。
func (m *Manager) SetRandomBindAvoid(mode string) error {
	mode = strings.ToLower(strings.TrimSpace(mode))
	if mode == "" {
		mode = "fallback"
	}
	if mode != "fallback" && mode != "strict" {
		return errors.New("random_bind_avoid must be fallback or strict")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	d := m.copyLocked()
	if d.RandomBindAvoid == mode {
		return nil
	}
	d.RandomBindAvoid = mode
	return m.commitLocked(d)
}

// RandomBindAvoid 返回当前模式（归一化后的 fallback/strict）。
func (m *Manager) RandomBindAvoid() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.data.RandomBindAvoid == "strict" {
		return "strict"
	}
	return "fallback"
}

func (m *Manager) Configure(s Settings) error {
	if err := s.validate(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	d := m.copyLocked()
	if d.Settings == s {
		return nil
	}
	d.Settings = s
	if err := m.commitLocked(d); err != nil {
		return err
	}
	for _, n := range d.Nodes {
		m.resetLocked(n.ID)
	}
	m.globalGen++
	m.globalHealth = health{}
	m.globalBlocked = time.Time{}
	delete(m.passiveFails, "global")
	delete(m.passiveAt, "global")
	if m.globalTransport != nil {
		m.globalTransport.CloseIdleConnections()
		m.globalTransport = nil
	}
	return nil
}

// selection 一次出站的出口选择：池节点 / 总代理 / 直连。
type selection struct {
	node    node
	proxied bool
	global  bool
}

// exitBlockedLocked 报告池节点是否处于 WAF/被动失败屏蔽期。
func (m *Manager) exitBlockedLocked(id string, now time.Time) bool {
	until := m.blocked[id]
	return !until.IsZero() && now.Before(until)
}

// globalAvailableLocked 报告总代理是否可用（启用、已配置、未被屏蔽）。
func (m *Manager) globalAvailableLocked(now time.Time) bool {
	return m.data.Global.Enabled && m.globalURL != nil && (m.globalBlocked.IsZero() || !now.Before(m.globalBlocked))
}

func (m *Manager) globalBlockedLocked(now time.Time) bool {
	return m.data.Global.Enabled && m.globalURL != nil && !m.globalBlocked.IsZero() && now.Before(m.globalBlocked)
}

func (m *Manager) selectLocked(uid, realm string) (selection, error) {
	now := time.Now()
	if m.closed {
		return selection{}, &Error{Reason: "pool closed"}
	}
	b := m.data.Bindings[uid]
	// 账号绑定优先于 realm 默认；显式 direct 赢过两者。
	if _, bound := m.data.Bindings[uid]; !bound {
		if rb, ok := m.data.Realms[realmKey(realm)]; ok {
			b = rb
		}
	}
	if b.Mode == "direct" {
		if m.exitBlockedLocked("direct", now) {
			return selection{}, &Error{Reason: "direct exit is temporarily blocked", Blocked: true}
		}
		return selection{}, nil
	}

	if b.Mode == "fixed" {
		for _, n := range m.data.Nodes {
			if n.ID != b.ProxyID {
				continue
			}
			if m.exitBlockedLocked(n.ID, now) {
				return selection{}, &Error{ProxyID: n.ID, Reason: "fixed proxy is temporarily blocked", Blocked: true}
			}
			if !n.Enabled || m.health[n.ID].status != "healthy" {
				return selection{}, &Error{ProxyID: n.ID, Reason: "fixed proxy is unavailable"}
			}

			return selection{node: n, proxied: true}, nil
		}
		return selection{}, &Error{ProxyID: b.ProxyID, Reason: "fixed proxy not found"}
	}

	// random / 未绑定：优先从「健康 + 启用 + 未被账号级 fixed 绑定」的代理里等概率
	// 随机（蓄水池抽样）；这样 6 个号各绑专属出口时，第 7 个号不会大概率撞别人出口。
	// 一条空闲代理都没有时：
	//   - fallback（默认）：退回全池健康代理随机，保证有网可用；
	//   - strict：绝不占用已绑定代理，直接走下面的总代理/直连兜底。
	// 「已绑定」只统计账号级 Bindings 的 fixed；realm 默认 fixed 是给未绑定账号用的，
	// 不算专属，仍可随机命中。Bind/Unbind 改的是同一份 data，下一次请求立即生效。
	boundIDs := map[string]bool{}
	for _, binding := range m.data.Bindings {
		if binding.Mode == "fixed" && binding.ProxyID != "" {
			boundIDs[binding.ProxyID] = true
		}
	}
	healthyOf := func(avoidBound bool) (node, int) {
		var selected node
		count := 0
		for _, n := range m.data.Nodes {
			if !n.Enabled || m.health[n.ID].status != "healthy" || m.exitBlockedLocked(n.ID, now) {
				continue
			}
			if avoidBound && boundIDs[n.ID] {
				continue
			}
			count++
			if rand.IntN(count) == 0 {
				selected = n
			}
		}
		return selected, count
	}
	selected, count := healthyOf(true)
	if count > 0 {
		return selection{node: selected, proxied: true}, nil
	}
	if m.data.RandomBindAvoid != "strict" {
		selected, count = healthyOf(false)
		if count > 0 {
			return selection{node: selected, proxied: true}, nil
		}
	}
	if m.globalAvailableLocked(now) {
		return selection{global: true}, nil
	}
	if m.data.Global.Enabled && m.globalURL != nil {
		// 总代理被 WAF 屏蔽：不要静默直连暴露本机 IP。
		return selection{}, &Error{ProxyID: "global", Reason: "global proxy is temporarily blocked", Blocked: m.globalBlockedLocked(now)}
	}
	if m.exitBlockedLocked("direct", now) {
		return selection{}, &Error{Reason: "direct exit is temporarily blocked", Blocked: true}
	}
	return selection{}, nil
}

// Do 选择一次出口后执行请求；重定向与响应体保持同一条路由。
func (m *Manager) Do(client *http.Client, req *http.Request, uid, realm string) (*http.Response, error) {
	resp, _, err := m.DoRoute(client, req, uid, realm)
	return resp, err
}

// DoRoute 同 Do，并返回本次实际使用的出口，供 WAF 按出口归因。
func (m *Manager) DoRoute(client *http.Client, req *http.Request, uid, realm string) (*http.Response, Route, error) {
	m.mu.Lock()
	sel, err := m.selectLocked(uid, realm)
	var tr *http.Transport
	if err == nil {
		switch {
		case sel.proxied:
			tr, err = m.transportLocked(sel.node)
		case sel.global:
			tr, err = m.globalTransportLocked()
		}
	}
	m.mu.Unlock()
	if err != nil {
		if req.Body != nil {
			_ = req.Body.Close()
		}
		return nil, Route{}, err
	}
	route := Route{Proxied: sel.proxied, Global: sel.global}
	if sel.proxied {
		route.ProxyID = sel.node.ID
	} else if sel.global {
		route.ProxyID = "global"
	}
	if tr == nil {
		resp, err := client.Do(req)
		return resp, route, err
	}
	selected := *client
	selected.Transport = tr
	resp, err := selected.Do(req)
	if err != nil {
		reason := "proxy request failed"
		if errors.Is(err, context.Canceled) {
			reason = "request canceled"
		} else if errors.Is(err, context.DeadlineExceeded) {
			reason = "request timed out"
		} else if errors.Is(err, errProxyAuth) {
			reason = "proxy authentication failed"
		}
		if !errors.Is(err, context.Canceled) {
			m.noteRouteFailure(route)
			tr.CloseIdleConnections()
		}
		return nil, route, &Error{ProxyID: route.ProxyID, Reason: reason, cause: err}
	}
	if resp.StatusCode == http.StatusProxyAuthRequired {
		_ = resp.Body.Close()
		m.noteRouteFailure(route)
		return nil, route, &Error{ProxyID: route.ProxyID, Reason: "proxy authentication failed"}
	}
	m.noteRouteSuccess(route)
	resp.Body = &proxyBody{ReadCloser: resp.Body, id: route.ProxyID, transport: tr}
	return resp, route, nil
}

// noteRouteFailure 记录被动失败；达到阈值后临时剔除该出口，避免健康探测前持续撞坏节点。
func (m *Manager) noteRouteFailure(route Route) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	if route.Global {
		m.noteGlobalFailureLocked(now)
		return
	}
	if !route.Proxied || route.ProxyID == "" {
		return
	}
	if last, ok := m.passiveAt[route.ProxyID]; !ok || now.Sub(last) > 2*time.Duration(m.data.Settings.IntervalSeconds)*time.Second {
		m.passiveFails[route.ProxyID] = 0
	}
	m.passiveAt[route.ProxyID] = now
	m.passiveFails[route.ProxyID]++
	if m.passiveFails[route.ProxyID] >= m.data.Settings.FailureThreshold {
		h := m.health[route.ProxyID]
		h.status, h.failures, h.lastError, h.checkedAt = "unhealthy", m.passiveFails[route.ProxyID], "proxy request failed", now
		m.health[route.ProxyID] = h
		m.passiveFails[route.ProxyID] = 0
		if tr := m.transports[route.ProxyID]; tr != nil {
			tr.CloseIdleConnections()
		}
	}
}

// noteRouteSuccess 清空被动失败计数；健康状态仍由主动探测决定。
func (m *Manager) noteRouteSuccess(route Route) {
	if !route.Proxied && !route.Global {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if route.Global {
		m.passiveFails["global"] = 0
		return
	}
	delete(m.passiveFails, route.ProxyID)
	delete(m.passiveAt, route.ProxyID)
}

func (m *Manager) noteGlobalFailureLocked(now time.Time) {
	if last, ok := m.passiveAt["global"]; !ok || now.Sub(last) > 2*time.Duration(m.data.Settings.IntervalSeconds)*time.Second {
		m.passiveFails["global"] = 0
	}
	m.passiveAt["global"] = now
	m.passiveFails["global"]++
	if m.passiveFails["global"] >= m.data.Settings.FailureThreshold {
		m.globalHealth.status, m.globalHealth.failures, m.globalHealth.lastError, m.globalHealth.checkedAt = "unhealthy", m.passiveFails["global"], "global proxy request failed", now
		m.passiveFails["global"] = 0
		if m.globalTransport != nil {
			m.globalTransport.CloseIdleConnections()
		}
	}
}

// BlockExit 在 WAF 403 等出口级风控命中后临时屏蔽出口（proxyID 或 "global"）。
func (m *Manager) BlockExit(id string, d time.Duration) {
	if id == "" || d <= 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	until := time.Now().Add(d)
	if id == "global" {
		m.globalBlocked = until
	} else {
		m.blocked[id] = until
	}
	m.signal()
}

// ClearBlock 立即解除出口屏蔽（健康探测成功或人工解冻）。
func (m *Manager) ClearBlock(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if id == "global" {
		m.globalBlocked = time.Time{}
	} else {
		delete(m.blocked, id)
	}
}

// ConfigureGlobal 保存容器级总代理；url 为空且 enabled=false 表示清除。
func (m *Manager) ConfigureGlobal(raw string, enabled bool) error {
	raw = strings.TrimSpace(raw)
	var u *url.URL
	if raw != "" {
		var err error
		u, err = parseProxyInput(raw)
		if err != nil {
			return err
		}
	}
	if enabled && u == nil {
		return errors.New("global proxy is enabled but URL is empty")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	// 面板快照会隐藏密码。用户把该快照原样保存时，沿用同一代理地址和用户名的旧密码；
	// 若输入不带用户名，则按显式清除认证处理。
	if u != nil && u.User != nil {
		if _, supplied := u.User.Password(); !supplied && m.globalURL != nil && m.globalURL.User != nil &&
			u.Scheme == m.globalURL.Scheme && u.Host == m.globalURL.Host && u.User.Username() == m.globalURL.User.Username() {
			if oldPassword, ok := m.globalURL.User.Password(); ok {
				u.User = url.UserPassword(u.User.Username(), oldPassword)
			}
		}
	}
	d := m.copyLocked()
	if u != nil {
		d.Global = GlobalConfig{URL: u.String(), Enabled: enabled}
	} else {
		d.Global = GlobalConfig{}
	}
	if err := m.commitLocked(d); err != nil {
		return err
	}
	m.globalURL = u
	m.globalGen++
	m.globalHealth = health{}
	m.globalBlocked = time.Time{}
	delete(m.passiveFails, "global")
	delete(m.passiveAt, "global")
	if m.globalTransport != nil {
		m.globalTransport.CloseIdleConnections()
		m.globalTransport = nil
	}
	return nil
}

// RequestGlobalCheck 排队一次总代理检测。
func (m *Manager) RequestGlobalCheck() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.globalURL == nil {
		return ErrNotFound
	}
	m.globalRequest = true
	m.signal()
	return nil
}

func (m *Manager) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = true
	for _, tr := range m.transports {
		tr.CloseIdleConnections()
	}
	if m.globalTransport != nil {
		m.globalTransport.CloseIdleConnections()
	}
}
