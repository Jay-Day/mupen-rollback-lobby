package moderation

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func readBanFile(t *testing.T, path string) []Sanction {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var list []Sanction
	if err := json.Unmarshal(data, &list); err != nil {
		t.Fatal(err)
	}
	return list
}

// Bans are written off the lock, by one writer that folds a burst into a few
// writes; FlushBans leaves the file holding every ban made before it.
func TestBanFileWrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bans.json")
	m := NewManager(NewFilter(nil), &AdminStore{}, path)
	t.Cleanup(m.FlushBans)
	for i := 0; i < 2000; i++ {
		m.Ban(fmt.Sprintf("2001:db8::%x", i+1), AutoBanBy, "test", 0)
	}
	m.Ban("203.0.113.9", "Mod", "timed", time.Hour)
	m.Unban("2001:db8::1", "Mod")
	m.FlushBans()
	list := readBanFile(t, path)
	if len(list) != 2000 {
		t.Fatalf("file holds %d bans, want 2000", len(list))
	}
	// the file loads back as it was
	again := NewManager(NewFilter(nil), &AdminStore{}, path)
	if s, ok := again.IsBanned("203.0.113.9"); !ok || s.Until.IsZero() || s.By != "Mod" {
		t.Fatalf("reloaded timeout = %+v %v", s, ok)
	}
	if _, ok := again.IsBanned("2001:db8::1"); ok {
		t.Fatal("the unban didn't reach the file")
	}
	// nothing changed: no write at all
	before, _ := os.Stat(path)
	time.Sleep(20 * time.Millisecond)
	m.FlushBans()
	if after, _ := os.Stat(path); !after.ModTime().Equal(before.ModTime()) {
		t.Fatal("an unchanged list was written again")
	}
}

// While the file is being written, the lock that every connect and chat line
// takes stays free.
func TestBanWriteDoesntHoldTheLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bans.json")
	m := NewManager(NewFilter(nil), &AdminStore{}, path)
	t.Cleanup(m.FlushBans)
	for i := 0; i < 30000; i++ {
		m.bans[fmt.Sprintf("2001:db8:1::%x", i+1)] = Sanction{IP: "x", Reason: AutoBanReason, By: AutoBanBy, Set: time.Now()}
	}
	m.saveMu.Lock() // a write in progress
	start := time.Now()
	m.Ban("203.0.113.10", AutoBanBy, "test", 0) // returns without waiting for it
	_, banned := m.IsBanned("198.51.100.1")
	took := time.Since(start)
	m.saveMu.Unlock()
	if banned || took > 100*time.Millisecond {
		t.Fatalf("ban + IsBanned took %v during a write", took)
	}
	m.FlushBans()
	if n := len(readBanFile(t, path)); n != 30001 {
		t.Fatalf("file holds %d bans", n)
	}
}

// A ban file written before this version (no names, no ordering) loads.
func TestOldBanFileLoads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bans.json")
	old := `[
  {"ip": "203.0.113.200", "reason": "known ban evader", "by": "Juniper", "set": "2026-11-15T10:00:00Z", "until": "2099-01-01T00:00:00Z"},
  {"ip": "192.0.2.81", "reason": "spam", "by": "Wren", "set": "2026-11-16T10:00:00Z", "until": "0001-01-01T00:00:00Z"}
]`
	if err := os.WriteFile(path, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	m := NewManager(NewFilter(nil), &AdminStore{}, path)
	t.Cleanup(m.FlushBans)
	bans, _ := m.ListSanctions()
	if len(bans) != 2 {
		t.Fatalf("loaded %+v", bans)
	}
	if s, ok := m.IsBanned("192.0.2.81"); !ok || !s.Until.IsZero() {
		t.Fatalf("permanent ban = %+v %v", s, ok)
	}
}

func TestGuardPolicyFromSettings(t *testing.T) {
	g, m, _, banned := newTestGuard(t)
	g.SetPolicy(func() LoginPolicy { return LoginPolicy{MaxFailures: 3, BanDuration: 10 * time.Minute} })
	ip := "198.51.100.33"
	g.Failed(ip, "lobby")
	g.Failed(ip, "lobby")
	if r := g.Failed(ip, "lobby"); r != LoginBanned {
		t.Fatalf("third failure = %v", r)
	}
	s, ok := m.IsBanned(ip)
	if !ok || s.Reason != "3 failed moderator logins in a row" || s.Until.IsZero() || time.Until(s.Until) > 10*time.Minute {
		t.Fatalf("ban = %+v %v", s, ok)
	}
	if len(*banned) != 1 {
		t.Fatalf("hook = %v", *banned)
	}
	// a policy below 1 still needs one failure
	g.SetPolicy(func() LoginPolicy { return LoginPolicy{MaxFailures: 0} })
	if r := g.Failed("198.51.100.34", "lobby"); r != LoginBanned {
		t.Fatalf("MaxFailures 0 = %v", r)
	}
	if s, _ := m.IsBanned("198.51.100.34"); !s.Until.IsZero() || s.Reason != "a failed moderator login" {
		t.Fatalf("permanent one-failure ban = %+v", s)
	}
}
