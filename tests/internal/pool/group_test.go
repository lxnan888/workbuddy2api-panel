package pool

import (
	"path/filepath"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// 分组存 state.json，重启后保留；空组名归一化。
func TestAccountGroupPersistence(t *testing.T) {
	fp := filepath.Join(t.TempDir(), "state.json")
	p := New(fp)
	defer p.Close()
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	if !p.SetAccountGroup("u1", "  A组  ") {
		t.Fatal("set group")
	}
	if n := p.SetAccountsGroup([]string{"u2", "missing"}, "B"); n != 1 {
		t.Fatalf("batch updated=%d want 1", n)
	}
	p.Flush()

	q := New(fp)
	defer q.Close()
	q.SyncToDir([]*auth.Auth{{UID: "u1"}, {UID: "u2"}})
	s1, _ := q.Status("u1")
	s2, _ := q.Status("u2")
	if s1.Group != "A组" || s2.Group != "B" {
		t.Fatalf("persisted groups: u1=%q u2=%q", s1.Group, s2.Group)
	}
	if groups := q.AccountGroups(); len(groups) != 2 || groups[0] != "A组" || groups[1] != "B" {
		t.Fatalf("groups=%v", groups)
	}
}

// 重命名分组：整组标签一起改；空新名非法。
func TestRenameAccountGroup(t *testing.T) {
	p := New("")
	defer p.Close()
	for _, uid := range []string{"u1", "u2", "u3"} {
		p.Add(&auth.Auth{UID: uid})
	}
	p.SetAccountsGroup([]string{"u1", "u2"}, "old")
	p.SetAccountGroup("u3", "other")
	if n, ok := p.RenameAccountGroup("old", "new"); !ok || n != 2 {
		t.Fatalf("rename n=%d ok=%v", n, ok)
	}
	if _, ok := p.RenameAccountGroup("new", "  "); ok {
		t.Fatal("empty new name should be rejected")
	}
	s1, _ := p.Status("u1")
	s3, _ := p.Status("u3")
	if s1.Group != "new" || s3.Group != "other" {
		t.Fatalf("after rename: u1=%q u3=%q", s1.Group, s3.Group)
	}
}
