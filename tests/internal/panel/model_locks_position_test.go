package panel

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestModelLocksPositionHelper 用最小 DOM 桩验证 applyModelLocksPosition：
// after_expiry 把 #mlBox 插到 #expBox 之后；其余值插到 #mlBottomAnchor 之后。
// 从 app.js 源码里抽出该函数（避免复制实现漂移），在同一 vm 脚本内执行。
func TestModelLocksPositionHelper(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; JS helper test skipped")
	}
	harness := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('function applyModelLocksPosition(');
if (start < 0) { console.log('FAIL: function not found'); throw new Error('function not found'); }
const end = src.indexOf('\n}', start);
const fnSrc = src.slice(start, end + 2);

// 最小 DOM：一个共同的 parent，children 顺序即页面顺序。
function makeNode(id) {
  return { id, parentNode: null, get nextSibling() {
    const p = this.parentNode; if (!p) return null;
    const i = p.children.indexOf(this); return i >= 0 ? (p.children[i+1] || null) : null;
  } };
}
const parent = { children: [],
  insertBefore(node, ref) {
    const i = this.children.indexOf(node); if (i >= 0) this.children.splice(i, 1);
    const j = ref ? this.children.indexOf(ref) : -1;
    if (j >= 0) this.children.splice(j, 0, node); else this.children.push(node);
    node.parentNode = this;
  } };
const exp = makeNode('expBox'), anchor = makeNode('mlBottomAnchor'),
      comp = makeNode('compBox'), acc = makeNode('accTableBox'), ml = makeNode('mlBox');
[exp, anchor, comp, acc, ml].forEach(n => parent.insertBefore(n, null));
const nodes = { expBox: exp, mlBottomAnchor: anchor, mlBox: ml };
const $ = id => nodes[id] || null;
const sandbox = { $, console };
vm.createContext(sandbox);
vm.runInContext(fnSrc + '\nthis.applyModelLocksPosition = applyModelLocksPosition;', sandbox);
const ids = () => parent.children.map(n => n.id).join(',');
const initial = ids();
if (initial !== 'expBox,mlBottomAnchor,compBox,accTableBox,mlBox') {
  console.log('FAIL initial order ' + initial); throw new Error('bad initial order');
}
// 默认/非法值：回到锚点后（即账号池表格下方原位置）。
sandbox.applyModelLocksPosition('bottom');
if (ids() !== 'expBox,mlBottomAnchor,mlBox,compBox,accTableBox') {
  console.log('FAIL bottom: ' + ids()); throw new Error('bottom position wrong');
}
sandbox.applyModelLocksPosition('bogus');
if (ids() !== 'expBox,mlBottomAnchor,mlBox,compBox,accTableBox') {
  console.log('FAIL bogus: ' + ids()); throw new Error('bogus should fallback to bottom');
}
// 可选值：紧跟积分到期提醒卡片。
sandbox.applyModelLocksPosition('after_expiry');
if (ids() !== 'expBox,mlBox,mlBottomAnchor,compBox,accTableBox') {
  console.log('FAIL after_expiry: ' + ids()); throw new Error('after_expiry position wrong');
}
console.log('POSITION OK');
`
	hf, err := os.CreateTemp(t.TempDir(), "position-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := hf.WriteString(harness); err != nil {
		t.Fatal(err)
	}
	_ = hf.Close()
	out, err := exec.Command(node, hf.Name(), "app.js").CombinedOutput()
	if err != nil || !strings.Contains(string(out), "POSITION OK") {
		t.Fatalf("model locks position helper: %v\n%s", err, out)
	}
}
