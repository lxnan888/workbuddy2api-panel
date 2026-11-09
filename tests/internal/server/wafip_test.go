package server

import (
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/proxypool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

func TestWAFGateSeparatesExits(t *testing.T) {
	oldWindow := wafIPWindow
	wafIPWindow = 10 * time.Second
	defer func() { wafIPWindow = oldWindow }()

	var g wafIPGate
	if g.noteWaf("proxy:a", "u1") {
		t.Fatal("one account must not activate an exit")
	}
	if g.noteWaf("proxy:b", "u2") {
		t.Fatal("a different exit must keep its own window")
	}
	if !g.noteWaf("proxy:a", "u2") {
		t.Fatal("two accounts on one exit must activate that exit")
	}
	if !g.active("proxy:a") {
		t.Fatal("activated exit should be active")
	}
	if g.active("proxy:b") {
		t.Fatal("other exit must remain unblocked")
	}
}

func TestWAFExitKeyFallbacks(t *testing.T) {
	if got := wafExitKey(nil); got != "direct" {
		t.Fatalf("nil error key=%q", got)
	}
	if got := wafExitKey(&upstream.Error{}); got != "direct" {
		t.Fatalf("missing route key=%q", got)
	}
	if got := wafExitKey(&upstream.Error{HasRoute: true, Route: proxypool.Route{Global: true, ProxyID: "global"}}); got != "global" {
		t.Fatalf("global key=%q", got)
	}
	if got := wafExitKey(&upstream.Error{HasRoute: true, Route: proxypool.Route{Proxied: true, ProxyID: "node-1"}}); got != "proxy:node-1" {
		t.Fatalf("proxy key=%q", got)
	}
}
