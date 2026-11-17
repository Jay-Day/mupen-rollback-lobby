package modsite

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jay-day/rmgk-lobby/internal/protocol"
)

// A moderator can't ban the address they're signed in from; banning one that
// another signed-in moderator uses needs confirm.
func TestBanTargetChecks(t *testing.T) {
	ts := newTestSite(t)
	cookie := ts.login(modIP)
	var reply struct {
		Error        string
		NeedsConfirm bool
	}
	if code := ts.api("POST", "/api/admin/sanctions/ban", cookie, map[string]any{"ip": modIP}, &reply); code != 400 ||
		!strings.Contains(reply.Error, "your own address") {
		t.Fatalf("own IP ban = %d %+v", code, reply)
	}
	// a timeout of it too, and a player's ban when the player shares it
	if code := ts.api("POST", "/api/admin/sanctions/ban", cookie, map[string]any{"ip": modIP, "duration": "1h"}, nil); code != 400 {
		t.Fatalf("own IP timeout = %d", code)
	}
	sibling := ts.connect("Sibling", modIP)
	if code := ts.api("POST", "/api/admin/users/"+itoa(sibling.id)+"/ban", cookie, map[string]any{}, &reply); code != 400 {
		t.Fatalf("ban of a player on the moderator's own IP = %d %+v", code, reply)
	}
	if bans, _ := ts.mod.ListSanctions(); len(bans) != 0 {
		t.Fatalf("banned anyway: %+v", bans)
	}
	// muting it is fine (it locks nobody out)
	if code := ts.api("POST", "/api/admin/sanctions/mute", cookie, map[string]any{"ip": modIP}, nil); code != 200 {
		t.Fatalf("own IP mute = %d", code)
	}

	// another moderator signed in to the site from 203.0.113.40
	const other = "203.0.113.40"
	ts.login(other)
	if code := ts.api("POST", "/api/admin/sanctions/ban", cookie, map[string]any{"ip": other}, &reply); code != 409 ||
		!reply.NeedsConfirm || !strings.Contains(reply.Error, "a moderator signed in to this site") {
		t.Fatalf("ban of another moderator's IP = %d %+v", code, reply)
	}
	if code := ts.api("POST", "/api/admin/sanctions/ban", cookie, map[string]any{"ip": other, "confirm": true}, nil); code != 200 {
		t.Fatalf("confirmed ban = %d", code)
	}
	// a lobby moderator's address
	const lobbyModIP = "203.0.113.41"
	lobbyMod := ts.connect("LobbyMod", lobbyModIP)
	lobbyMod.send(protocol.TypeAdminAuth, protocol.AdminAuthRequest{Password: modPassword})
	if _, ok := lobbyMod.await(protocol.TypeAdminAuthOK); !ok {
		t.Fatal("lobby moderator sign-in")
	}
	if code := ts.api("POST", "/api/admin/sanctions/ban", cookie, map[string]any{"ip": lobbyModIP, "duration": "10m"}, &reply); code != 409 ||
		!strings.Contains(reply.Error, "moderator LobbyMod in the lobby") {
		t.Fatalf("ban of a lobby moderator's IP = %d %+v", code, reply)
	}
	// a plain player's address needs nothing
	ts.connect("Player", "203.0.113.42")
	if code := ts.api("POST", "/api/admin/sanctions/ban", cookie, map[string]any{"ip": "203.0.113.42"}, nil); code != 200 {
		t.Fatalf("player IP ban = %d", code)
	}
}

func TestSanctionsArePaged(t *testing.T) {
	ts := newTestSite(t)
	cookie := ts.login(modIP)
	for i := 0; i < 250; i++ {
		ts.mod.Ban(fmt.Sprintf("2001:db8::%x", i+1), "auto", "test", 0)
		if i%50 == 0 {
			time.Sleep(2 * time.Millisecond) // distinct times
		}
	}
	ts.mod.Mute("192.0.2.5", "Wren", "caps", 0)
	var first struct {
		Bans, Mutes         []struct{ IP, Cursor string }
		MoreBans, MoreMutes bool
		BanCount, MuteCount int
	}
	if code := ts.api("GET", "/api/admin/sanctions", cookie, nil, &first); code != 200 || len(first.Bans) != sanctionsPage ||
		!first.MoreBans || first.BanCount != 250 || len(first.Mutes) != 1 || first.MoreMutes || first.MuteCount != 1 {
		t.Fatalf("first pages = %d %+v", code, first)
	}
	seen := map[string]bool{}
	for _, b := range first.Bans {
		seen[b.IP] = true
	}
	cursor := first.Bans[len(first.Bans)-1].Cursor
	for {
		var page struct {
			Items []struct{ IP, Cursor string }
			More  bool
			Total int
		}
		if code := ts.api("GET", "/api/admin/sanctions?kind=ban&limit=100&before="+cursor, cookie, nil, &page); code != 200 ||
			page.Total != 250 {
			t.Fatalf("next page = %d %+v", code, page)
		}
		for _, b := range page.Items {
			if seen[b.IP] {
				t.Fatalf("%s listed twice", b.IP)
			}
			seen[b.IP] = true
		}
		if !page.More {
			break
		}
		cursor = page.Items[len(page.Items)-1].Cursor
	}
	if len(seen) != 250 {
		t.Fatalf("paging listed %d of 250 bans", len(seen))
	}
	var one struct {
		Ban  *struct{ IP string }
		Mute *struct{ Reason string }
	}
	if code := ts.api("GET", "/api/admin/sanctions?ip=192.0.2.5", cookie, nil, &one); code != 200 || one.Ban != nil ||
		one.Mute == nil || one.Mute.Reason != "caps" {
		t.Fatalf("one address = %d %+v", code, one)
	}
	if code := ts.api("GET", "/api/admin/sanctions?kind=other", cookie, nil, nil); code != 400 {
		t.Fatalf("bad kind = %d", code)
	}
	if code := ts.api("GET", "/api/admin/sanctions?kind=ban&limit=501", cookie, nil, nil); code != 400 {
		t.Fatalf("big limit = %d", code)
	}
}

// Signed-out answers close the connection; a signed-in one keeps it.
func TestSignedOutConnectionsClose(t *testing.T) {
	ts := newTestSite(t)
	for _, q := range []req{
		{method: "GET", path: "/"},
		{method: "POST", path: "/api/admin/login", body: map[string]string{"password": "wrong"}},
		{method: "GET", path: "/nothing"},
	} {
		q.ip = "203.0.113.70"
		if w := ts.serve(q); w.Header().Get("Connection") != "close" {
			t.Errorf("%s %s signed out: Connection = %q", q.method, q.path, w.Header().Get("Connection"))
		}
	}
	cookie := ts.login(modIP)
	if w := ts.serve(req{method: "GET", path: "/", ip: modIP, cookie: cookie}); w.Header().Get("Connection") != "" {
		t.Fatal("the signed-in page closed its connection")
	}
	// a signed-in 404 closes it, and keeps the site's headers
	w := ts.serve(req{method: "GET", path: "/api/admin/nothing", ip: modIP, cookie: cookie})
	if w.Code != 404 || w.Header().Get("Connection") != "close" || w.Header().Get("X-Frame-Options") != "DENY" {
		t.Fatalf("signed-in 404 = %d %v", w.Code, w.Header())
	}
}

func TestAdmits(t *testing.T) {
	ts := newTestSite(t)
	ts.mod.Ban("203.0.113.80", "auto", "test", 0)
	ts.setConfig(`{"adminAllowedIPs": ["203.0.113.0/24"]}`)
	for ip, want := range map[string]bool{
		"127.0.0.1": true, "::1": true, "203.0.113.81": true,
		"203.0.113.80": false, // banned
		"198.51.100.1": false, // not listed
	} {
		if got := ts.s.Admits(ip); got != want {
			t.Errorf("Admits(%s) = %v, want %v", ip, got, want)
		}
	}
	ts.mod.Ban("127.0.0.1", "auto", "test", 0) // a loopback ban never locks the tunnel out
	if !ts.s.Admits("127.0.0.1") {
		t.Fatal("loopback refused")
	}
}
