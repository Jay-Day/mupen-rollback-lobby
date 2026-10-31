package moderation

import (
	"path/filepath"
	"testing"
)

func TestAdminFailuresShareOneBudgetPerIP(t *testing.T) {
	store := &AdminStore{admins: []Admin{{Name: "mod", Salt: "s", Hash: HashPassword("s", "right")}}}
	m := NewManager(NewFilter(nil), store, filepath.Join(t.TempDir(), "bans.json"))

	if name, ok, limited := m.AuthenticateAdminFrom("1.1.1.1", "right"); !ok || limited || name != "mod" {
		t.Fatalf("right password = %q %v %v", name, ok, limited)
	}
	for i := 0; i < AdminFailLimit; i++ {
		if _, ok, limited := m.AuthenticateAdminFrom("1.1.1.1", "wrong"); ok || limited {
			t.Fatalf("guess %d: ok=%v limited=%v", i, ok, limited)
		}
	}
	// the budget is spent: even the right password isn't checked now
	if _, ok, limited := m.AuthenticateAdminFrom("1.1.1.1", "right"); ok || !limited {
		t.Fatalf("after %d failures: ok=%v limited=%v", AdminFailLimit, ok, limited)
	}
	// other addresses keep their own budget
	if _, ok, limited := m.AuthenticateAdminFrom("2.2.2.2", "right"); !ok || limited {
		t.Fatalf("other ip: ok=%v limited=%v", ok, limited)
	}
}
