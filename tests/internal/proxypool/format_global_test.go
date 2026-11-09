package proxypool

import (
	"context"
	"crypto/tls"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestParseProxyInputFormats(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		// 无 scheme：默认 http。
		{"proxy.example:8080:alice:s3cret", "http://alice:s3cret@proxy.example:8080"},
		{"alice:s3cret@proxy.example:8080", "http://alice:s3cret@proxy.example:8080"},
		{"proxy.example:8080@alice:s3cret", "http://alice:s3cret@proxy.example:8080"},
		{"proxy.example:8080", "http://proxy.example:8080"},
		// URL 形式：带/不带认证、四种协议。
		{"http://proxy.example:80", "http://proxy.example:80"},
		{"http://alice:s3cret@proxy.example:80", "http://alice:s3cret@proxy.example:80"},
		{"https://alice:s3cret@proxy.example:443", "https://alice:s3cret@proxy.example:443"},
		{"socks5://proxy.example:1080", "socks5://proxy.example:1080"},
		{"socks5://alice:s3cret@proxy.example:1080", "socks5://alice:s3cret@proxy.example:1080"},
		{"socks5h://alice:s3cret@proxy.example:1080", "socks5h://alice:s3cret@proxy.example:1080"},
		// 大小写与默认端口。
		{"HTTP://ALICE:S3CRET@PROXY.EXAMPLE", "http://ALICE:S3CRET@proxy.example:80"},
		// 密码含冒号。
		{"proxy.example:8080:alice:pa:ss", "http://alice:pa%3Ass@proxy.example:8080"},
		{"alice:pa:ss@proxy.example:8080", "http://alice:pa%3Ass@proxy.example:8080"},
		// IPv6。
		{"[::1]:8080:alice:s3cret", "http://alice:s3cret@[::1]:8080"},
	}
	for _, tc := range cases {
		u, err := parseProxyInput(tc.in)
		if err != nil {
			t.Errorf("%q: %v", tc.in, err)
			continue
		}
		if got := u.String(); got != tc.want {
			t.Errorf("%q => %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestParseProxyInputRejects(t *testing.T) {
	for _, in := range []string{
		"", "host", "host:0", "host:65536", "host:abc", "http://host/path",
		"http://host?x=1", "http://host#f", "ftp://host:21", "http://:pass@host:80",
		"user:pass@host", "host:8080:user", "a@b@c",
	} {
		if _, err := parseProxyInput(in); err == nil {
			t.Errorf("%q should be rejected", in)
		}
	}
}

func withAuth(raw string) string {
	return strings.Replace(raw, "://", "://user:secret@", 1)
}

func expectSeen(t *testing.T, seen <-chan string, want string) {
	t.Helper()
	select {
	case got := <-seen:
		if got != want {
			t.Fatalf("connect target=%q, want %q", got, want)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("proxy saw no CONNECT, want %q", want)
	}
}

func TestGlobalProxyChain(t *testing.T) {
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "target-ok")
	}))
	defer target.Close()

	globalSrv, globalSeen := connectProxy(t, false)
	poolSrv, poolSeen := connectProxy(t, false)
	globalURL, poolURL := withAuth(globalSrv.URL), withAuth(poolSrv.URL)

	base := http.DefaultTransport.(*http.Transport).Clone()
	base.Proxy = nil
	base.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} // 测试自签证书
	m, err := New("", base, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if err := m.ConfigureGlobal(globalURL, true); err != nil {
		t.Fatal(err)
	}
	id := addProxy(t, m, poolURL)
	m.mu.Lock()
	m.health[id] = health{status: "healthy"}
	m.mu.Unlock()

	req, _ := http.NewRequest(http.MethodGet, target.URL, nil)
	resp, route, err := m.DoRoute(&http.Client{Timeout: 5 * time.Second}, req, "u", "cn")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "target-ok" {
		t.Fatalf("body=%q", body)
	}
	if !route.Proxied || route.ProxyID != id || route.Global {
		t.Fatalf("route=%+v", route)
	}
	expectSeen(t, globalSeen, strings.TrimPrefix(poolSrv.URL, "http://"))
	expectSeen(t, poolSeen, strings.TrimPrefix(target.URL, "https://"))
}

func TestGlobalOnlyAndDirectOverride(t *testing.T) {
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	defer target.Close()
	globalSrv, globalSeen := connectProxy(t, false)

	base := http.DefaultTransport.(*http.Transport).Clone()
	base.Proxy = nil
	base.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	m, err := New("", base, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if err := m.ConfigureGlobal(withAuth(globalSrv.URL), true); err != nil {
		t.Fatal(err)
	}

	// 没有池代理：random 走总代理。
	client := &http.Client{Transport: base, Timeout: 5 * time.Second}
	req, _ := http.NewRequest(http.MethodGet, target.URL, nil)
	resp, route, err := m.DoRoute(client, req, "u", "cn")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if !route.Global || route.Proxied {
		t.Fatalf("route=%+v", route)
	}
	expectSeen(t, globalSeen, strings.TrimPrefix(target.URL, "https://"))

	// 显式 direct 必须绕过总代理。
	if err := m.Bind("direct-user", Binding{Mode: "direct"}); err != nil {
		t.Fatal(err)
	}
	req, _ = http.NewRequest(http.MethodGet, target.URL, nil)
	resp, route, err = m.DoRoute(client, req, "direct-user", "cn")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if route.Global || route.Proxied {
		t.Fatalf("direct route=%+v", route)
	}
	select {
	case got := <-globalSeen:
		t.Fatalf("direct should not hit global, saw %q", got)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestGlobalExitBlockDoesNotDisablePoolChain(t *testing.T) {
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "target-ok")
	}))
	defer target.Close()

	globalSrv, globalSeen := connectProxy(t, false)
	poolSrv, poolSeen := connectProxy(t, false)
	base := http.DefaultTransport.(*http.Transport).Clone()
	base.Proxy = nil
	base.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	m, err := New("", base, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if err := m.ConfigureGlobal(withAuth(globalSrv.URL), true); err != nil {
		t.Fatal(err)
	}
	id := addProxy(t, m, withAuth(poolSrv.URL))
	m.mu.Lock()
	m.health[id] = health{status: "healthy"}
	m.mu.Unlock()

	// 屏蔽总代理作为最终出口后，池代理链仍应经总代理连接池代理。
	m.BlockExit("global", time.Minute)
	req, _ := http.NewRequest(http.MethodGet, target.URL, nil)
	resp, route, err := m.DoRoute(&http.Client{Timeout: 5 * time.Second}, req, "pooled", "cn")
	if err != nil {
		t.Fatalf("pool route failed while global exit blocked: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if string(body) != "target-ok" || !route.Proxied || route.ProxyID != id || route.Global {
		t.Fatalf("pool route=%+v body=%q", route, body)
	}
	expectSeen(t, globalSeen, strings.TrimPrefix(poolSrv.URL, "http://"))
	expectSeen(t, poolSeen, strings.TrimPrefix(target.URL, "https://"))

	// 没有可用池代理时，总代理会成为最终出口，此时屏蔽必须生效。
	off := false
	if err := m.Update(id, Input{Enabled: &off}); err != nil {
		t.Fatal(err)
	}
	req, _ = http.NewRequest(http.MethodGet, target.URL, nil)
	if _, _, err := m.DoRoute(&http.Client{Timeout: time.Second}, req, "global-only", "cn"); !IsBlockedError(err) {
		t.Fatalf("blocked global fallback should fail closed as blocked, got %v", err)
	}

	// 显式直连出口的屏蔽同样必须 fail-fast，不能静默改走总代理。
	if err := m.Bind("direct-only", Binding{Mode: "direct"}); err != nil {
		t.Fatal(err)
	}
	m.BlockExit("direct", time.Minute)
	req, _ = http.NewRequest(http.MethodGet, target.URL, nil)
	if _, _, err := m.DoRoute(&http.Client{Timeout: time.Second}, req, "direct-only", "cn"); !IsBlockedError(err) {
		t.Fatalf("blocked direct route should fail closed as blocked, got %v", err)
	}
}

func TestFixedProxyUnavailableDoesNotFallback(t *testing.T) {
	m := testManager(t, "")
	if err := m.ConfigureGlobal("http://127.0.0.1:9", true); err != nil {
		t.Fatal(err)
	}
	id := addProxy(t, m, "http://127.0.0.1:8")
	if err := m.Bind("u", Binding{Mode: "fixed", ProxyID: id}); err != nil {
		t.Fatal(err)
	}
	if sel, err := m.selectLocked("u", "cn"); err == nil || sel.proxied || sel.global {
		t.Fatalf("fixed unavailable must error, sel=%+v err=%v", sel, err)
	}
}

func TestTestURLAndProbeGlobal(t *testing.T) {
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	defer target.Close()
	globalSrv, _ := connectProxy(t, false)
	inputSrv, _ := connectProxy(t, false)

	base := http.DefaultTransport.(*http.Transport).Clone()
	base.Proxy = nil
	base.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	m, err := New("", base, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	m.mu.Lock()
	m.data.Settings.URL = target.URL
	m.mu.Unlock()
	if err := m.ConfigureGlobal(withAuth(globalSrv.URL), true); err != nil {
		t.Fatal(err)
	}
	res, err := m.TestURL(context.Background(), withAuth(inputSrv.URL))
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK {
		t.Fatalf("test url result=%+v", res)
	}
	if err := m.ProbeGlobal(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := m.Snapshot().Global.Status; got != "healthy" {
		t.Fatalf("global status=%q", got)
	}
}
