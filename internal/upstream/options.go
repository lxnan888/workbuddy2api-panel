package upstream

import (
	"net/http"
	"time"
)

// Options 出站请求的运行时配置快照（面板保存后可整体热替换）。
// 字段语义与 Client 上的同名字段一致；Options 是请求路径的唯一读取源。
type Options struct {
	UserAgent      string
	ClientVersion  string
	CliVersion     string
	ClientName     string
	DeviceToken    string
	DeviceTokenFile string
	PassthroughIP  bool

	ChatBaseCN       string
	BillingBaseCN    string
	WebBaseCN        string
	ChatBaseGlobal   string
	BillingBaseGlobal string
	GlobalEnabled    bool

	HeaderTimeout time.Duration
	IdleTimeout   time.Duration
	HTTPTimeout   time.Duration
}

// Options 返回当前运行时配置；未通过 SetOptions 注入时从 Client 字段兜底
// （测试/裸用路径零行为变化）。
func (c *Client) Options() Options {
	if c == nil {
		return Options{}
	}
	if p := c.opts.Load(); p != nil {
		return *p
	}
	o := Options{
		UserAgent:         c.UserAgent,
		ClientVersion:     c.ClientVersion,
		CliVersion:        c.CliVersion,
		ClientName:        c.ClientName,
		DeviceToken:       c.DeviceToken,
		DeviceTokenFile:   c.DeviceTokenFile,
		PassthroughIP:     c.PassthroughIP,
		ChatBaseCN:        c.ChatBaseCN,
		BillingBaseCN:     c.BillingBaseCN,
		WebBaseCN:         c.WebBaseCN,
		ChatBaseGlobal:    c.ChatBaseGlobal,
		BillingBaseGlobal: c.BillingBaseGlobal,
		GlobalEnabled:     c.GlobalEnabled,
		HeaderTimeout:     c.HeaderTimeout,
		IdleTimeout:       c.IdleTimeout,
	}
	if c.HTTP != nil {
		o.HTTPTimeout = c.HTTP.Timeout
	}
	return o
}

// SetOptions 热替换运行时配置，并按新超时重建 HTTP client（共享原 Transport）。
func (c *Client) SetOptions(o Options) {
	if c == nil {
		return
	}
	c.opts.Store(&o)
	c.clientMu.Lock()
	defer c.clientMu.Unlock()
	var tr http.RoundTripper
	if c.HTTP != nil {
		tr = c.HTTP.Transport
	} else if c.ChatHTTP != nil {
		tr = c.ChatHTTP.Transport
	}
	if tr != nil {
		c.HTTP = &http.Client{Timeout: o.HTTPTimeout, Transport: tr}
		c.ChatHTTP = &http.Client{Timeout: 0, Transport: tr}
	}
}

// httpClient 并发安全读取普通 HTTP client。
func (c *Client) httpClient() *http.Client {
	c.clientMu.RLock()
	defer c.clientMu.RUnlock()
	return c.HTTP
}

// clients 返回当前两个 client 的快照（供传输层重建时关闭空闲连接）。
func (c *Client) clients() []*http.Client {
	c.clientMu.RLock()
	defer c.clientMu.RUnlock()
	return []*http.Client{c.HTTP, c.ChatHTTP}
}
