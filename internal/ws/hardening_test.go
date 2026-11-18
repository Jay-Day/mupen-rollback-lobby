package ws

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/jay-day/rmgk-lobby/internal/moderation"
	"github.com/jay-day/rmgk-lobby/internal/modlog"
	"github.com/jay-day/rmgk-lobby/internal/protocol"
)

// joinWithOrigin connects like a web page would: the browser adds Origin.
func (f *modFixture) joinWithOrigin(name, ip, origin string) *wsConn {
	f.t.Helper()
	ctx, cancel := context.WithTimeout(bg, 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, f.url, &websocket.DialOptions{HTTPHeader: http.Header{
		"X-Test-IP": {ip}, "Origin": {origin}}})
	if err != nil {
		f.t.Fatalf("dial: %v", err)
	}
	f.t.Cleanup(func() { conn.CloseNow() })
	c := &wsConn{t: f.t, conn: conn}
	before := f.hub.OnlineCount()
	c.send(protocol.TypeHello, protocol.HelloRequest{Username: name, ClientVersion: "v0.9.15"})
	if _, ok := c.await(protocol.TypeHelloOK); !ok {
		f.t.Fatalf("hello %s refused", name)
	}
	f.waitOnline(before + 1)
	return c
}

// A web page can open the lobby's socket from any visitor's browser. Its
// ADMIN_AUTH must neither count toward the login guard (or the page could get
// its visitors banned) nor ever sign in.
func TestWebPageCantTryModeratorPasswords(t *testing.T) {
	f := newModFixture(t)
	const visitor = "198.51.100.77"
	page := f.joinWithOrigin("Visitor1", visitor, "http://evil.example")
	for i := 0; i < 2*moderation.MaxLoginFailures; i++ {
		page.send(protocol.TypeAdminAuth, protocol.AdminAuthRequest{Password: "guess"})
		env, ok := page.await(protocol.TypeAdminAuthFail)
		if !ok {
			t.Fatalf("try %d: connection closed", i+1)
		}
		var n protocol.ModNoticePayload
		json.Unmarshal(env.Data, &n)
		if n.Text != "Incorrect password." {
			t.Fatalf("reply = %q, want the usual wrong-password answer", n.Text)
		}
	}
	// even the right password doesn't sign a web page in
	page.send(protocol.TypeAdminAuth, protocol.AdminAuthRequest{Password: testModPassword})
	if _, ok := page.await(protocol.TypeAdminAuthFail); !ok {
		t.Fatal("no ADMIN_AUTH_FAIL for the right password from a web page")
	}
	page.send(protocol.TypeModAction, protocol.ModActionRequest{Action: "list"})
	if text, ok := page.awaitNotice("not authorized"); !ok {
		t.Fatalf("MOD_ACTION from a web page = %q", text)
	}
	if _, banned := f.mod.IsBanned(visitor); banned {
		t.Fatal("a web page got its visitor's address banned")
	}
	if f.guard.Tracked() != 0 {
		t.Fatalf("the guard counted %d addresses' web-page tries", f.guard.Tracked())
	}
	// the desktop client (no Origin) from the same address still signs in
	desk, _ := f.join("Desktop", visitor)
	f.signInModerator(desk)
}

// The guard's ban follows lobby.json: adminLoginMaxFailures and
// adminLoginBanSeconds (here 2 failures, a 1 hour timeout).
func TestLoginGuardPolicy(t *testing.T) {
	f := newModFixture(t)
	f.guard.SetPolicy(func() moderation.LoginPolicy {
		return moderation.LoginPolicy{MaxFailures: 2, BanDuration: time.Hour}
	})
	const ip = "198.51.100.78"
	c, _ := f.join("Guesser", ip)
	c.send(protocol.TypeAdminAuth, protocol.AdminAuthRequest{Password: "a"})
	c.await(protocol.TypeAdminAuthFail)
	c.send(protocol.TypeAdminAuth, protocol.AdminAuthRequest{Password: "b"})
	if _, ok := c.awaitNotice("too many failed moderator logins"); !ok {
		t.Fatal("no ban notice after 2 failures")
	}
	s, banned := f.mod.IsBanned(ip)
	if !banned || s.Until.IsZero() || time.Until(s.Until) < 59*time.Minute || s.Reason != moderation.LoginBanReason(2) {
		t.Fatalf("ban = %+v %v", s, banned)
	}
	if audit := f.logs.lines("moderator action"); !hasLine(audit, "action=timeout", "Timed out "+ip+" until") {
		t.Fatalf("audit = %q", audit)
	}
}

// A moderator taken out of admins.json (or given a new password) loses the
// lobby's powers at their next action, and the site can then kick them.
func TestRemovedModeratorLosesPowers(t *testing.T) {
	f := newModFixture(t)
	mod, modOK := f.join("Moddy", "192.0.2.1")
	f.signInModerator(mod)
	victim, _ := f.join("Victim", "192.0.2.2")
	if _, err := f.hub.ModerateUser("Site", modOK.UserID, "kick", 0, ""); err != ErrModerator {
		t.Fatalf("site kick of a valid moderator: %v", err)
	}
	f.mod.SetAdmins(&moderation.AdminStore{}) // admins.json emptied
	mod.send(protocol.TypeModAction, protocol.ModActionRequest{Action: "kick", Target: "Victim"})
	if _, ok := mod.awaitNotice("no longer valid"); !ok {
		t.Fatal("removed moderator not told")
	}
	if _, ok := mod.awaitNotice("not authorized"); !ok {
		t.Fatal("removed moderator's kick wasn't refused")
	}
	victim.sync() // still connected
	if out, err := f.hub.ModerateUser("Site", modOK.UserID, "kick", 0, "not a moderator any more"); err != nil ||
		out.Target != "Moddy" {
		t.Fatalf("site kick of a removed moderator = %+v %v", out, err)
	}
	mod.awaitClosed()
}

// Blocked lines are sampled per address: the first few are logged, the rest
// counted, and the window's last one is logged with the count of the others.
func TestBlockedChatIsSampledPerIP(t *testing.T) {
	f := newModFixture(t)
	const ip = "198.51.100.90"
	a, _ := f.join("Muted1", ip)
	b, _ := f.join("Muted2", ip) // same address, another connection
	f.mod.Mute(ip, "Mod", "test", 0)
	for i := 0; i < 4; i++ {
		a.chat(channelLobby, "from a")
		b.chat(channelLobby, "from b")
	}
	a.sync()
	b.sync()
	blocked := func() []string {
		var out []string
		for _, line := range f.logs.lines("chat") {
			if strings.Contains(line, "ip="+ip) {
				out = append(out, line)
			}
		}
		return out
	}
	if rows := blocked(); len(rows) != blockedLogBurst {
		t.Fatalf("logged %d blocked lines, want %d: %q", len(rows), blockedLogBurst, rows)
	}
	// the window ends: its last line goes in, standing for the others
	f.hub.flushBlockedChat(time.Now().Add(blockedLogWindow + time.Second))
	rows := blocked()
	last := rows[len(rows)-1]
	if len(rows) != blockedLogBurst+1 || !strings.Contains(last, "blocked=muted") ||
		!strings.Contains(last, fmt.Sprintf("suppressed=%d", 8-blockedLogBurst-1)) {
		t.Fatalf("after the window = %q", rows)
	}
}

func TestBlockedChatLogUnit(t *testing.T) {
	b := newBlockedChatLog()
	t0 := time.Unix(1_800_000_000, 0)
	line := func(text string) modlog.ChatMessage {
		return modlog.ChatMessage{Text: text, Blocked: modlog.BlockedFilter}
	}
	for i := 0; i < blockedLogBurst; i++ {
		if _, ok := b.admit("203.0.113.1", line("x"), t0); !ok {
			t.Fatalf("line %d held back", i)
		}
	}
	for i := 0; i < 10; i++ {
		if _, ok := b.admit("203.0.113.1", line("y"), t0.Add(time.Second)); ok {
			t.Fatal("line over the burst logged")
		}
	}
	if out := b.flush(t0.Add(30 * time.Second)); len(out) != 0 {
		t.Fatalf("flushed before the window ended: %+v", out)
	}
	// a new window: the next line carries the count
	m, ok := b.admit("203.0.113.1", line("z"), t0.Add(blockedLogWindow+time.Second))
	if !ok || m.Suppressed != 10 {
		t.Fatalf("first line of the next window = %+v %v", m, ok)
	}
	if out := b.flush(t0.Add(3 * blockedLogWindow)); len(out) != 0 {
		t.Fatalf("nothing held back, but flushed %+v", out)
	}
	// memory stays bounded
	for i := 0; i < blockedLogMaxIPs+50; i++ {
		b.admit("2001:db8::"+string(rune('a'+i%26))+string(rune('a'+i/26%26))+string(rune('a'+i/676%26)), line("w"), t0)
	}
	if n := len(b.ips); n > blockedLogMaxIPs {
		t.Fatalf("tracking %d addresses, cap %d", n, blockedLogMaxIPs)
	}
}
