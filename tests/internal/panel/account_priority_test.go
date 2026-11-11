package panel

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func putAccountPriority(e *upgEnv, uid string, priority int) (int, string) {
	e.t.Helper()
	body := fmt.Sprintf(`{"priority":%d}`, priority)
	req := httptest.NewRequest("PUT", "/panel/api/accounts/"+uid+"/priority", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer test-key")
	rr := httptest.NewRecorder()
	e.panel.ServeHTTP(rr, req)
	return rr.Code, rr.Body.String()
}

func TestAccountPriorityAPI(t *testing.T) {
	e := upgNewEnv(t, upgSeed{uid: "u1", credits: 100})

	code, body := putAccountPriority(e, "u1", 7)
	if code != http.StatusOK {
		t.Fatalf("set priority -> %d %s", code, body)
	}
	if !strings.Contains(body, `"priority":7`) {
		t.Fatalf("response missing priority: %s", body)
	}
	if got, ok := e.pool.AccountPriority("u1"); !ok || got != 7 {
		t.Fatalf("pool priority=%d ok=%v want 7", got, ok)
	}

	// 落盘：state.json 的 accounts.u1.priority 必须是 7（重启后仍生效）。
	e.pool.Flush()
	raw, err := os.ReadFile(e.stateFile)
	if err != nil {
		t.Fatal(err)
	}
	var sf struct {
		Accounts map[string]struct {
			Priority int `json:"priority"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(raw, &sf); err != nil {
		t.Fatal(err)
	}
	if sf.Accounts["u1"].Priority != 7 {
		t.Fatalf("state.json priority=%d want 7", sf.Accounts["u1"].Priority)
	}

	// 越界与非法账号。
	if code, _ := putAccountPriority(e, "u1", 10000); code != http.StatusBadRequest {
		t.Fatalf("priority 10000 -> %d want 400", code)
	}
	if code, _ := putAccountPriority(e, "u1", -1); code != http.StatusBadRequest {
		t.Fatalf("priority -1 -> %d want 400", code)
	}
	if code, _ := putAccountPriority(e, "missing", 1); code != http.StatusNotFound {
		t.Fatalf("missing account -> %d want 404", code)
	}

	// 改回 0 = 默认，接口应返回 0。
	if code, body := putAccountPriority(e, "u1", 0); code != http.StatusOK || !strings.Contains(body, `"priority":0`) {
		t.Fatalf("reset priority -> %d %s", code, body)
	}
}
