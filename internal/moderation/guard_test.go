package moderation

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func testManager(t *testing.T) *Manager {
	t.Helper()
	dir := t.TempDir()
	adminsPath := filepath.Join(dir, "admins.json")
	raw, _ := json.Marshal([]Admin{{Name: "Mod", Salt: "s", Hash: HashPassword("s", "right-password")}})
	if err := os.WriteFile(adminsPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	admins, err := LoadAdmins(adminsPath)
	if err != nil {
		t.Fatal(err)
	}
	m := NewManager(NewFilter(nil), admins, filepath.Join(dir, "bans.json"))
	t.Cleanup(m.FlushBans) // the writer is done before the directory goes
	return m
}

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func newTestGuard(t *testing.T) (*LoginGuard, *Manager, *fakeClock, *[]string) {
	m := testManager(t)
	g := NewLoginGuard(m)
	clock := &fakeClock{t: time.Unix(1_800_000_000, 0)}
	g.now = clock.now
	var banned []string
	g.SetOnBan(func(b GuardBan) { banned = append(banned, b.IP+" "+b.Where) })
	return g, m, clock, &banned
}

func TestGuardBansAfterFiveInARow(t *testing.T) {
	g, m, _, banned := newTestGuard(t)
	ip := "203.0.113.9"
	for i := 1; i < MaxLoginFailures; i++ {
		if r := g.Failed(ip, "lobby"); r != LoginCounted {
			t.Fatalf("failure %d = %v", i, r)
		}
		if _, ok := m.IsBanned(ip); ok {
			t.Fatalf("banned after %d failures", i)
		}
	}
	if r := g.Failed(ip, "moderator site"); r != LoginBanned {
		t.Fatalf("fifth failure = %v", r)
	}
	s, ok := m.IsBanned(ip)
	if !ok || s.By != AutoBanBy || s.Reason != AutoBanReason || !s.Until.IsZero() {
		t.Fatalf("ban = %+v %v", s, ok)
	}
	if len(*banned) != 1 || (*banned)[0] != ip+" moderator site" {
		t.Fatalf("hook calls = %v", *banned)
	}
	// it shows with the other bans, and comes off like them
	bans, _ := m.ListSanctions()
	if len(bans) != 1 || bans[0].IP != ip {
		t.Fatalf("listed bans = %+v", bans)
	}
	if !m.Unban(ip, "Mod") {
		t.Fatal("unban")
	}
	if g.Tracked() != 0 {
		t.Fatalf("tracked after the ban = %d", g.Tracked())
	}
}

func TestGuardSuccessResetsTheCount(t *testing.T) {
	g, m, _, _ := newTestGuard(t)
	ip := "198.51.100.4"
	for round := 0; round < 3; round++ {
		for i := 1; i < MaxLoginFailures; i++ {
			g.Failed(ip, "lobby")
		}
		g.Succeeded(ip)
	}
	if _, ok := m.IsBanned(ip); ok {
		t.Fatal("banned although every run ended in a success")
	}
	// the count is per IP: another address' failures don't add up with these
	for i := 1; i < MaxLoginFailures; i++ {
		g.Failed(ip, "lobby")
		g.Failed("198.51.100.5", "lobby")
	}
	if _, ok := m.IsBanned(ip); ok {
		t.Fatal("banned by someone else's failures")
	}
}

func TestGuardNormalizesAddresses(t *testing.T) {
	g, m, _, _ := newTestGuard(t)
	for i := 0; i < MaxLoginFailures; i++ {
		ip := "203.0.113.20"
		if i%2 == 1 {
			ip = "::ffff:203.0.113.20" // the same address, IPv4-mapped
		}
		g.Failed(ip, "lobby")
	}
	if _, ok := m.IsBanned("203.0.113.20"); !ok {
		t.Fatal("an IPv4-mapped address counted apart")
	}
}

func TestGuardLocksLoopbackOutInsteadOfBanning(t *testing.T) {
	for _, ip := range []string{"127.0.0.1", "::1", "127.0.0.5"} {
		g, m, clock, banned := newTestGuard(t)
		for i := 1; i < MaxLoginFailures; i++ {
			g.Failed(ip, "moderator site")
		}
		if ok, _ := g.Allow(ip); !ok {
			t.Fatalf("%s locked before the fifth failure", ip)
		}
		if r := g.Failed(ip, "moderator site"); r != LoginLocked {
			t.Fatalf("%s fifth failure = %v", ip, r)
		}
		if _, ok := m.IsBanned(ip); ok || len(*banned) != 0 {
			t.Fatalf("%s was banned", ip)
		}
		ok, left := g.Allow(ip)
		if ok || left != LoopbackLockout {
			t.Fatalf("%s allowed during lockout (%v left)", ip, left)
		}
		// failures while locked out don't stretch it
		g.Failed(ip, "moderator site")
		clock.advance(LoopbackLockout - time.Second)
		if ok, _ := g.Allow(ip); ok {
			t.Fatalf("%s lockout ended early", ip)
		}
		clock.advance(2 * time.Second)
		if ok, _ := g.Allow(ip); !ok {
			t.Fatalf("%s still locked after %v", ip, LoopbackLockout)
		}
		// a fresh run of failures is needed for the next lockout
		if r := g.Failed(ip, "moderator site"); r != LoginCounted {
			t.Fatalf("%s first failure after the lockout = %v", ip, r)
		}
	}
}

func TestGuardForgetsAndStaysBounded(t *testing.T) {
	g, m, clock, _ := newTestGuard(t)
	for i := 1; i < MaxLoginFailures; i++ {
		g.Failed("192.0.2.1", "lobby")
	}
	clock.advance(guardForget + time.Minute)
	g.Failed("192.0.2.1", "lobby") // a day later, this starts a new count
	if _, ok := m.IsBanned("192.0.2.1"); ok {
		t.Fatal("failures a day apart banned")
	}
	for i := 0; i < guardMaxEntries+100; i++ {
		g.Failed(fmt.Sprintf("10.%d.%d.%d", i>>16&255, i>>8&255, i&255), "lobby")
	}
	if n := g.Tracked(); n > guardMaxEntries {
		t.Fatalf("tracking %d IPs, cap %d", n, guardMaxEntries)
	}
}

func TestNilGuard(t *testing.T) {
	var g *LoginGuard
	if ok, _ := g.Allow("203.0.113.1"); !ok {
		t.Fatal("nil guard refused")
	}
	if r := g.Failed("203.0.113.1", "lobby"); r != LoginCounted {
		t.Fatal("nil guard counted")
	}
	g.Succeeded("203.0.113.1")
	g.SetOnBan(nil)
}

func TestAdminKeys(t *testing.T) {
	m := testManager(t)
	name, key, ok := m.AuthenticateAdminKey("right-password")
	if !ok || name != "Mod" || key == "" || !m.AdminKeyValid(key) {
		t.Fatalf("auth = %q %q %v", name, key, ok)
	}
	if _, _, ok := m.AuthenticateAdminKey("wrong"); ok {
		t.Fatal("wrong password accepted")
	}
	// a new password for the same name is a different credential
	m.SetAdmins(&AdminStore{admins: []Admin{{Name: "Mod", Salt: "t", Hash: HashPassword("t", "new-password")}}})
	if m.AdminKeyValid(key) {
		t.Fatal("old key still valid after a password change")
	}
	m.SetAdmins(&AdminStore{})
	if m.AdminKeyValid("") {
		t.Fatal("empty key valid")
	}
}

func TestGuardTryCountsAndStopsParallelGuessing(t *testing.T) {
	g, m, _, banned := newTestGuard(t)
	const ip = "203.0.113.77"
	var mu sync.Mutex
	checked := 0
	var wg sync.WaitGroup
	results := make(chan LoginResult, 50)
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results <- g.Try(ip, "moderator site", func() bool {
				mu.Lock()
				checked++
				mu.Unlock()
				return false
			})
		}()
	}
	wg.Wait()
	close(results)
	counts := map[LoginResult]int{}
	for r := range results {
		counts[r]++
	}
	if checked != MaxLoginFailures || counts[LoginBanned] != 1 || counts[LoginCounted] != MaxLoginFailures-1 ||
		counts[LoginRefused] != 50-MaxLoginFailures {
		t.Fatalf("checked %d passwords, results %v", checked, counts)
	}
	if _, ok := m.IsBanned(ip); !ok || len(*banned) != 1 {
		t.Fatal("not banned once")
	}
	// a right password from a banned IP isn't even checked
	if r := g.Try(ip, "lobby", func() bool { t.Fatal("checked"); return true }); r != LoginRefused {
		t.Fatalf("banned try = %v", r)
	}
	// elsewhere, a right password is OK and resets the count
	g.Failed("203.0.113.78", "lobby")
	if r := g.Try("203.0.113.78", "lobby", func() bool { return true }); r != LoginOK || g.Tracked() != 0 {
		t.Fatalf("good try = %v, tracked %d", r, g.Tracked())
	}
	var nilGuard *LoginGuard
	if nilGuard.Try(ip, "lobby", func() bool { return true }) != LoginOK ||
		nilGuard.Try(ip, "lobby", func() bool { return false }) != LoginCounted {
		t.Fatal("nil guard Try")
	}
}
