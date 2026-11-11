// import_test.go 覆盖「导入 JSON」对三种形态的自动识别：
// 插件 OAuth 单对象 / 对象数组 / cockpit 数组，以及导入后的落盘与入池。
package panel

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

const nestedSingle = `{
  "account": {"enterpriseId": "", "nickname": "嵌套号", "uid": "u-nested"},
  "auth": {
    "accessToken": "at-u-nested",
    "domain": "www.codebuddy.cn",
    "expiresAt": 1796115027,
    "realm": "cn",
    "refreshToken": "rt-u-nested"
  }
}`

// TestParseImportFileShapes 三种形态都能解析出正确的 realm / 有效期单位。
func TestParseImportFileShapes(t *testing.T) {
	t.Run("单对象嵌套", func(t *testing.T) {
		entries, total, errs, err := parseImportFile([]byte(nestedSingle))
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if total != 1 || len(errs) != 0 || len(entries) != 1 {
			t.Fatalf("total=%d errs=%v entries=%d", total, errs, len(entries))
		}
		e := entries[0]
		if e.auth.UID != "u-nested" || e.auth.Nickname != "嵌套号" || e.realm != "cn" {
			t.Fatalf("entry=%+v realm=%s", e.auth, e.realm)
		}
		if e.auth.ExpiresAt != 1796115027 {
			t.Fatalf("嵌套形 expiresAt 应保持秒: %d", e.auth.ExpiresAt)
		}
	})

	t.Run("扁平 auth 对象", func(t *testing.T) {
		raw := `{"accessToken":"at-flat","refreshToken":"rt-flat","expiresAt":1796115027,"uid":"u-flat","realm":"cn"}`
		entries, total, errs, err := parseImportFile([]byte(raw))
		if err != nil || total != 1 || len(errs) != 0 || len(entries) != 1 {
			t.Fatalf("err=%v total=%d errs=%v entries=%d", err, total, errs, len(entries))
		}
		if entries[0].auth.UID != "u-flat" || entries[0].realm != "cn" {
			t.Fatalf("entry=%+v", entries[0])
		}
	})

	t.Run("嵌套对象数组", func(t *testing.T) {
		second := strings.ReplaceAll(nestedSingle, "u-nested", "u-nested2")
		raw := "[" + nestedSingle + "," + second + "]"
		entries, total, errs, err := parseImportFile([]byte(raw))
		if err != nil || total != 2 || len(errs) != 0 || len(entries) != 2 {
			t.Fatalf("err=%v total=%d errs=%v entries=%d", err, total, errs, len(entries))
		}
		if entries[0].auth.UID != "u-nested" || entries[1].auth.UID != "u-nested2" {
			t.Fatalf("entries=%+v", entries)
		}
	})

	t.Run("cockpit 数组", func(t *testing.T) {
		raw := `[{"id":"x","uid":"u-cockpit","access_token":"at-c","refresh_token":"rt-c",` +
			`"expires_at":1893456000000,"domain":"www.workbuddy.ai","nickname":"cockpit 号"}]`
		entries, total, errs, err := parseImportFile([]byte(raw))
		if err != nil || total != 1 || len(errs) != 0 || len(entries) != 1 {
			t.Fatalf("err=%v total=%d errs=%v entries=%d", err, total, errs, len(entries))
		}
		e := entries[0]
		if e.realm != "global" {
			t.Fatalf("workbuddy.ai 应判 global，得到 %s", e.realm)
		}
		if e.auth.ExpiresAt != 1893456000 {
			t.Fatalf("cockpit expires_at 毫秒应转秒，得到 %d", e.auth.ExpiresAt)
		}
	})

	t.Run("global 域嵌套", func(t *testing.T) {
		raw := strings.ReplaceAll(nestedSingle, "www.codebuddy.cn", "www.workbuddy.ai")
		raw = strings.ReplaceAll(raw, `"realm": "cn"`, `"realm": ""`)
		entries, _, errs, err := parseImportFile([]byte(raw))
		if err != nil || len(errs) != 0 || len(entries) != 1 {
			t.Fatalf("err=%v errs=%v entries=%d", err, errs, len(entries))
		}
		if entries[0].realm != "global" {
			t.Fatalf("workbuddy.ai 应判 global，得到 %s", entries[0].realm)
		}
	})

	t.Run("非法顶层结构", func(t *testing.T) {
		if _, _, _, err := parseImportFile([]byte(`"hello"`)); err == nil {
			t.Fatal("字符串顶层应报错")
		}
		if _, _, _, err := parseImportFile([]byte(`[]`)); err == nil {
			t.Fatal("空数组应报错")
		}
	})

	t.Run("单条缺 refreshToken 记为跳过", func(t *testing.T) {
		raw := `{"auth":{"accessToken":"at","refreshToken":"","expiresAt":1,"realm":"cn"},"account":{"uid":"u1"}}`
		entries, total, errs, err := parseImportFile([]byte(raw))
		if err != nil || total != 1 || len(entries) != 0 || len(errs) != 1 {
			t.Fatalf("err=%v total=%d errors=%v entries=%d", err, total, errs, len(entries))
		}
	})
}

// importRaw 走真实的 multipart 导入接口。
func (e *upgEnv) importRaw(name string, raw []byte) (int, string) {
	e.t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", name)
	if err != nil {
		e.t.Fatal(err)
	}
	if _, err := fw.Write(raw); err != nil {
		e.t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		e.t.Fatal(err)
	}
	req := httptest.NewRequest("POST", "/panel/api/import/cockpit", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer test-key")
	rr := httptest.NewRecorder()
	e.panel.ServeHTTP(rr, req)
	return rr.Code, rr.Body.String()
}

// TestImportNestedSingleObject 单对象嵌套导入：落盘为 auths/ 嵌套形、入池、realm 正确。
func TestImportNestedSingleObject(t *testing.T) {
	e := upgNewEnv(t, upgSeed{uid: "u0", nickname: "旧号", credits: 100})
	e.fake.register("u-nested", 321)

	code, body := e.importRaw("auth.json", []byte(nestedSingle))
	if code != http.StatusOK {
		t.Fatalf("导入 -> %d %s", code, body)
	}
	var resp struct {
		Imported int `json:"imported"`
		Skipped  int `json:"skipped"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("响应非 JSON: %v %s", err, body)
	}
	if resp.Imported != 1 || resp.Skipped != 0 {
		t.Fatalf("imported=%d skipped=%d body=%s", resp.Imported, resp.Skipped, body)
	}

	path := filepath.Join(e.authDir, "workbuddy-u-nested.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("auth 文件未落盘: %v", err)
	}
	a, err := auth.Parse(raw)
	if err != nil {
		t.Fatalf("落盘文件不可解析: %v", err)
	}
	if a.UID != "u-nested" || a.Nickname != "嵌套号" || a.Realm() != "cn" {
		t.Fatalf("落盘内容不符: uid=%s nickname=%s realm=%s", a.UID, a.Nickname, a.Realm())
	}
	if e.pool.AuthByUID("u-nested") == nil {
		t.Fatal("导入账号未入池")
	}
	if st, ok := e.pool.Status("u-nested"); !ok || st.Credits != 321 {
		t.Fatalf("导入后余额应刷新为 321，得到 ok=%v status=%+v", ok, st)
	}
}

// TestImportNestedArray 嵌套对象数组一次导入两个账号。
func TestImportNestedArray(t *testing.T) {
	e := upgNewEnv(t)
	e.fake.register("u-a", 10)
	e.fake.register("u-b", 20)
	a := strings.ReplaceAll(nestedSingle, "u-nested", "u-a")
	b := strings.ReplaceAll(nestedSingle, "u-nested", "u-b")
	code, body := e.importRaw("auths.json", []byte("["+a+","+b+"]"))
	if code != http.StatusOK || !strings.Contains(body, `"imported":2`) {
		t.Fatalf("导入 -> %d %s", code, body)
	}
	for _, uid := range []string{"u-a", "u-b"} {
		if _, err := os.Stat(filepath.Join(e.authDir, "workbuddy-"+uid+".json")); err != nil {
			t.Fatalf("%s 未落盘: %v", uid, err)
		}
		if e.pool.AuthByUID(uid) == nil {
			t.Fatalf("%s 未入池", uid)
		}
	}
}

// TestImportNestedGlobalRealm global 域导入落盘 realm=global。
func TestImportNestedGlobalRealm(t *testing.T) {
	e := upgNewEnv(t)
	e.fake.register("u-global", 5)
	raw := strings.ReplaceAll(nestedSingle, "u-nested", "u-global")
	raw = strings.ReplaceAll(raw, "www.codebuddy.cn", "www.workbuddy.ai")
	raw = strings.ReplaceAll(raw, `"realm": "cn"`, `"realm": "global"`)
	code, body := e.importRaw("auth.json", []byte(raw))
	if code != http.StatusOK || !strings.Contains(body, `"imported":1`) {
		t.Fatalf("导入 -> %d %s", code, body)
	}
	file, err := os.ReadFile(filepath.Join(e.authDir, "workbuddy-u-global.json"))
	if err != nil {
		t.Fatal(err)
	}
	a, err := auth.Parse(file)
	if err != nil {
		t.Fatal(err)
	}
	if a.RealmStored() != "global" {
		t.Fatalf("realm 应为 global，得到 %q", a.RealmStored())
	}
	if time.Now().Unix() > a.ExpiresAt {
		t.Fatalf("expiresAt 解析异常: %d", a.ExpiresAt)
	}
}
