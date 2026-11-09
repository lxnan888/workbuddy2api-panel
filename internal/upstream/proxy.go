package upstream

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"strconv"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/proxypool"
)

// gzipRequestBody 对 >=1KB 的 chat 请求体做 gzip（官方客户端形态）。
// 用 GetBody 取独立读副本，不改写调用方已构造的 Body。
func gzipRequestBody(req *http.Request) {
	if req == nil || req.Body == nil || req.GetBody == nil || req.Header.Get("Content-Encoding") != "" {
		return
	}
	src, err := req.GetBody()
	if err != nil {
		return
	}
	defer src.Close()
	data, err := io.ReadAll(io.LimitReader(src, 8<<20))
	if err != nil || len(data) < 1024 {
		return
	}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(data); err != nil {
		return
	}
	if err := zw.Close(); err != nil {
		return
	}
	req.Body = io.NopCloser(&buf)
	req.ContentLength = int64(buf.Len())
	req.Header.Set("Content-Encoding", "gzip")
	req.Header.Set("Content-Length", strconv.Itoa(buf.Len()))
}

// officialConnectionValue 给出该 path 上官方客户端实际发送的 Connection 值。
//
// 2026-10-30 双版本真机抓包（CN 5.9.3 / GL 5.9.4）：官方在每条 RPC 上都显式
// 写了 Connection —— 除 /v3/config 是 close 外，其余全是 keep-alive。
// HTTP/1.1 默认即复用，Go 因此从不发送这一行，但头集合少了它就是少一个字段；
// 风控按白名单查头时，"缺一个官方必发的头"和"多一个官方不发的头"一样显眼。
//
// 没抓到过的 path 返回空串，保持 Go 默认行为，不臆造。
func officialConnectionValue(path string) string {
	switch path {
	case "/v3/config":
		return "close"
	case "/v2/chat/completions", "/v2/chat/busy", "/v2/report":
		return "keep-alive"
	}
	return ""
}

// applyConnectionHeader 把官方那个显式的 Connection 头补回去。
//
// close 不能写进 Header：net/http 的 transferWriter 会按 req.Close 自己写一行
// Connection: close，手写会重复成两行。keep-alive 相反 —— Go 认为它是默认值所以
// 从不发出，必须手写才会出现在线上字节里。
func applyConnectionHeader(req *http.Request) {
	switch officialConnectionValue(req.URL.Path) {
	case "close":
		req.Close = true
	case "keep-alive":
		req.Header.Set("Connection", "keep-alive")
	}
}

// doRoute 是单一的、带账号语义的 HTTP 出站出口，并保留本次实际路由。
//
// 头名拼写在这里统一纠正（headcase.go），Connection 在这里补发（官方必发，
// Go 默认不发）。所有出站点都收敛到本函数，代理出口信息随上游错误信封返回，
// 供服务端按出口归因 WAF。
func (c *Client) doRoute(req *http.Request, a *auth.Auth, chat bool) (*http.Response, proxypool.Route, error) {
	req = normalizeHeaderSpelling(req)
	applyConnectionHeader(req)
	if chat {
		gzipRequestBody(req)
	}
	client := c.httpClient()
	if chat {
		client = c.chatHTTP()
	}
	if c.ProxyPool == nil || a == nil {
		resp, err := client.Do(req)
		return resp, proxypool.Route{}, err
	}
	return c.ProxyPool.DoRoute(client, req, a.UID, a.Realm())
}

// do 保留普通出站调用方的原有签名。
// do 保留普通出站调用方的原有签名。
func (c *Client) do(req *http.Request, a *auth.Auth, chat bool) (*http.Response, error) {
	resp, _, err := c.doRoute(req, a, chat)
	return resp, err
}
