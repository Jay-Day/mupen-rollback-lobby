package ws

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/jay-day/rmgk-lobby/internal/config"
	"github.com/jay-day/rmgk-lobby/internal/moderation"
	"github.com/jay-day/rmgk-lobby/internal/modlog"
	"github.com/jay-day/rmgk-lobby/internal/protocol"
	"github.com/jay-day/rmgk-lobby/internal/state"
)

var bg = context.Background()

// modFixture is a real lobby over WebSocket with moderation, the login guard
// and the moderators' log. Each connection says which IP it comes from.
type modFixture struct {
	t        *testing.T
	hub      *Hub
	mod      *moderation.Manager
	guard    *moderation.LoginGuard
	log      *modlog.Logger
	url      string
	cfgPath  string // lobby.json (see setConfig)
	settings *config.Store
}

const testModPassword = "modpass123"

// setup runs on the hub before it starts (say, SetActionLimiter).
func newModFixture(t *testing.T, setup ...func(*Hub)) *modFixture {
	t.Helper()
	dir := t.TempDir()
	adminsPath := filepath.Join(dir, "admins.json")
	admins, _ := json.Marshal([]moderation.Admin{{Name: "Mod", Salt: "s", Hash: moderation.HashPassword("s", testModPassword)}})
	if err := os.WriteFile(adminsPath, admins, 0o600); err != nil {
		t.Fatal(err)
	}
	adminStore, err := moderation.LoadAdmins(adminsPath)
	if err != nil {
		t.Fatal(err)
	}
	mod := moderation.NewManager(moderation.NewFilter([]string{"badword"}), adminStore, filepath.Join(dir, "bans.json"))
	t.Cleanup(mod.FlushBans) // the ban writer is done before the directory goes
	cfgPath := filepath.Join(dir, "lobby.json")
	settings, err := config.Open(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	log, err := modlog.Open(filepath.Join(dir, "modlog.db"))
	if err != nil {
		t.Fatal(err)
	}
	hub := NewHub(state.New(), "", protocol.ICEConfiguration{}, mod, settings)
	hub.SetModLog(log)
	guard := moderation.NewLoginGuard(mod)
	hub.SetLoginGuard(guard)
	for _, f := range setup {
		f(hub)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go hub.Run(ctx)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hub.ServeWS(w, r, r.Header.Get("X-Test-IP"))
	}))
	t.Cleanup(func() {
		server.Close()
		cancel()
		log.Close()
	})
	return &modFixture{t: t, hub: hub, mod: mod, guard: guard, log: log,
		url: "ws" + strings.TrimPrefix(server.URL, "http"), cfgPath: cfgPath, settings: settings}
}

// wsConn is one lobby connection in a test.
type wsConn struct {
	t    *testing.T
	conn *websocket.Conn
}

func (c *wsConn) send(msgType string, data any) {
	c.t.Helper()
	payload, _ := json.Marshal(data)
	raw, _ := json.Marshal(protocol.Envelope{Type: msgType, Data: payload})
	ctx, cancel := context.WithTimeout(bg, 5*time.Second)
	defer cancel()
	if err := c.conn.Write(ctx, websocket.MessageText, raw); err != nil {
		c.t.Fatalf("write %s: %v", msgType, err)
	}
}

// await reads until msgType arrives; false when the connection closes first.
func (c *wsConn) await(msgType string) (protocol.Envelope, bool) {
	ctx, cancel := context.WithTimeout(bg, 5*time.Second)
	defer cancel()
	for {
		_, raw, err := c.conn.Read(ctx)
		if err != nil {
			return protocol.Envelope{}, false
		}
		var env protocol.Envelope
		if json.Unmarshal(raw, &env) == nil && env.Type == msgType {
			return env, true
		}
	}
}

// awaitNotice reads until a MOD_NOTICE containing text; ok is false if the
// connection closes first.
func (c *wsConn) awaitNotice(text string) (string, bool) {
	c.t.Helper()
	for {
		env, ok := c.await(protocol.TypeModNotice)
		if !ok {
			return "", false
		}
		var n protocol.ModNoticePayload
		json.Unmarshal(env.Data, &n)
		if strings.Contains(n.Text, text) {
			return n.Text, true
		}
	}
}

// awaitClosed reads until the connection closes.
func (c *wsConn) awaitClosed() {
	c.t.Helper()
	ctx, cancel := context.WithTimeout(bg, 5*time.Second)
	defer cancel()
	for {
		if _, _, err := c.conn.Read(ctx); err != nil {
			if ctx.Err() != nil {
				c.t.Fatal("connection still open")
			}
			return
		}
	}
}

// sync waits until the hub handled everything c sent so far.
func (c *wsConn) sync() {
	c.t.Helper()
	c.send(protocol.TypeHeartbeat, protocol.HeartbeatRequest{})
	if _, ok := c.await(protocol.TypeHeartbeatAck); !ok {
		c.t.Fatal("connection closed before HEARTBEAT_ACK")
	}
}

func (c *wsConn) chat(channel, text string) {
	c.send(protocol.TypeChatSend, protocol.ChatSendRequest{Channel: channel, Message: text})
}

// dial connects from ip; the error is the refused handshake, if any.
func (f *modFixture) dial(ip string) (*wsConn, error) {
	ctx, cancel := context.WithTimeout(bg, 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, f.url, &websocket.DialOptions{HTTPHeader: http.Header{"X-Test-IP": {ip}}})
	if err != nil {
		return nil, err
	}
	conn.SetReadLimit(1 << 20)
	f.t.Cleanup(func() { conn.CloseNow() })
	return &wsConn{t: f.t, conn: conn}, nil
}

// join connects name from ip as a current client and waits until the hub
// lists the connection.
func (f *modFixture) join(name, ip string) (*wsConn, protocol.HelloOKResponse) {
	f.t.Helper()
	c, err := f.dial(ip)
	if err != nil {
		f.t.Fatalf("dial %s: %v", name, err)
	}
	before := f.hub.OnlineCount()
	c.send(protocol.TypeHello, protocol.HelloRequest{Username: name, ClientVersion: "vdev-1", ICESupported: true,
		LateJoin: true})
	env, ok := c.await(protocol.TypeHelloOK)
	if !ok {
		f.t.Fatalf("hello %s refused", name)
	}
	var hello protocol.HelloOKResponse
	json.Unmarshal(env.Data, &hello)
	f.waitOnline(before + 1)
	return c, hello
}

func (f *modFixture) waitOnline(n int) {
	f.t.Helper()
	for deadline := time.Now().Add(3 * time.Second); f.hub.OnlineCount() != n; {
		if time.Now().After(deadline) {
			f.t.Fatalf("online = %d, want %d", f.hub.OnlineCount(), n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (f *modFixture) signInModerator(c *wsConn) {
	f.t.Helper()
	c.send(protocol.TypeAdminAuth, protocol.AdminAuthRequest{Password: testModPassword})
	if _, ok := c.await(protocol.TypeAdminAuthOK); !ok {
		f.t.Fatal("moderator sign-in failed")
	}
}

func TestModLogRecordsASession(t *testing.T) {
	f := newModFixture(t)
	ace, _ := f.join("Ace", "203.0.113.5")
	bee, _ := f.join("Bee", "2001:db8::7")

	// lobby chat: one delivered, one blocked by the filter
	ace.chat(channelLobby, "anyone up for Smash Remix?")
	ace.chat(channelLobby, "you badword")
	ace.sync()

	// a room, a match, room chat, a drop and the host closing the game
	ace.send(protocol.TypeRoomCreate, protocol.RoomCreateRequest{Name: "Remix FFA",
		Rom: protocol.RomInfo{Name: "Smash Remix", MD5: "ABCD"}, MaxPlayers: 4})
	created, ok := ace.await(protocol.TypeRoomCreated)
	if !ok {
		t.Fatal("no ROOM_CREATED")
	}
	var room protocol.RoomCreatedResponse
	json.Unmarshal(created.Data, &room)
	ace.sync()
	bee.send(protocol.TypeRoomJoin, protocol.RoomJoinRequest{RoomID: room.RoomID})
	if _, ok := bee.await(protocol.TypeRoomJoinOK); !ok {
		t.Fatal("no ROOM_JOIN_OK")
	}
	bee.chat(channelRoom, "glhf")
	bee.sync()
	ace.send(protocol.TypeRoomStart, struct{}{})
	begin, ok := ace.await(protocol.TypeMatchBegin)
	if !ok {
		t.Fatal("no MATCH_BEGIN")
	}
	var match protocol.MatchBeginPayload
	json.Unmarshal(begin.Data, &match)
	ace.send(protocol.TypeMatchConnected, protocol.MatchPeerEventRequest{MatchID: match.MatchID})
	ace.chat(channelRoom, "gg")
	ace.sync()
	bee.send(protocol.TypeMatchFinished, struct{}{})
	bee.sync()
	ace.send(protocol.TypeMatchFinished, struct{}{})
	ace.sync()

	// a flood: 5 go through, the rest are dropped and at most 3 logged
	spam, _ := f.join("Spammy", "198.51.100.9")
	for i := 0; i < 9; i++ {
		spam.chat(channelLobby, "buy gold")
	}
	spam.sync()

	bee.conn.Close(websocket.StatusNormalClosure, "")
	f.waitOnline(2)
	f.log.Flush()

	// connections
	conns, _, err := f.log.Connections(bg, modlog.ConnQuery{Name: "bee"})
	if err != nil || len(conns) != 1 {
		t.Fatalf("bee's connections = %+v %v", conns, err)
	}
	if c := conns[0]; c.IP != "2001:db8::7" || c.Disconnected == 0 || c.Version != "vdev-1" {
		t.Fatalf("bee's connection = %+v", c)
	}
	if conns, _, _ := f.log.Connections(bg, modlog.ConnQuery{Name: "Ace"}); len(conns) != 1 || conns[0].Disconnected != 0 {
		t.Fatalf("ace's connection = %+v", conns)
	}

	// chat
	all, _, _ := f.log.ChatLog(bg, modlog.ChatQuery{Limit: 500})
	count := func(match func(modlog.ChatRow) bool) int {
		n := 0
		for _, m := range all {
			if match(m) {
				n++
			}
		}
		return n
	}
	if n := count(func(m modlog.ChatRow) bool {
		return m.Channel == "lobby" && m.From == "Ace" && m.Blocked == "" && m.IP == "203.0.113.5"
	}); n != 1 {
		t.Fatalf("ace's delivered lobby lines = %d: %+v", n, all)
	}
	if n := count(func(m modlog.ChatRow) bool { return m.Blocked == modlog.BlockedFilter && m.Text == "you badword" }); n != 1 {
		t.Fatalf("filtered lines = %d", n)
	}
	if n := count(func(m modlog.ChatRow) bool {
		return m.Channel == "room" && m.RoomID == room.RoomID && m.RoomName == "Remix FFA" && m.MatchID == 0
	}); n != 1 {
		t.Fatalf("room chat before the match = %d: %+v", n, all)
	}
	if n := count(func(m modlog.ChatRow) bool { return m.Channel == "room" && m.MatchID == match.MatchID }); n != 1 {
		t.Fatalf("room chat during the match = %d", n)
	}
	if n := count(func(m modlog.ChatRow) bool { return m.From == "Spammy" && m.Blocked == "" }); n != 5 {
		t.Fatalf("spam delivered = %d", n)
	}
	if n := count(func(m modlog.ChatRow) bool { return m.From == "Spammy" && m.Blocked == modlog.BlockedFlood }); n != 3 {
		t.Fatalf("flood rows = %d, want 3", n)
	}

	// the game
	games, _, _ := f.log.Games(bg, modlog.GameQuery{})
	if len(games) != 1 {
		t.Fatalf("games = %+v", games)
	}
	g := games[0]
	if g.MatchID != match.MatchID || g.RoomID != room.RoomID || g.Game != "Smash Remix" || g.RomMD5 != "ABCD" ||
		g.EndReason != modlog.EndFinished || g.Ended == 0 || len(g.Players) != 2 {
		t.Fatalf("game = %+v", g)
	}
	for _, p := range g.Players {
		switch p.Name {
		case "Ace":
			if p.IP != "203.0.113.5" || p.Slot != 1 || p.Left != 0 {
				t.Fatalf("ace = %+v", p)
			}
		case "Bee":
			if p.Left == 0 || p.IP != "2001:db8::7" {
				t.Fatalf("bee = %+v", p)
			}
		}
	}
	_, events, chats, err := f.log.GameByID(bg, g.ID)
	if err != nil || chats != 1 {
		t.Fatalf("game detail chats = %d %v", chats, err)
	}
	var texts []string
	for _, e := range events {
		texts = append(texts, e.Type+": "+e.Text)
	}
	joined := strings.Join(texts, "\n")
	for _, want := range []string{"game_started: Game started: Ace (P1), Bee (P2)", "player_left: Bee closed the game",
		"game_ended: Game ended: finished", "room_joined: Bee joined the room"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("game events lack %q:\n%s", want, joined)
		}
	}
}

func TestWebModerationMatchesTheLobby(t *testing.T) {
	f := newModFixture(t)
	mod, _ := f.join("Moddy", "192.0.2.1")
	f.signInModerator(mod)
	cee, _ := f.join("Cee", "203.0.113.20")
	dee, deeOK := f.join("Dee", "203.0.113.21")

	// the lobby's kick: the notice arrives before the close
	mod.send(protocol.TypeModAction, protocol.ModActionRequest{Action: "kick", Target: "cee", Reason: "spam"})
	lobbyText, ok := cee.awaitNotice("kicked")
	if !ok {
		t.Fatal("cee got no kick notice")
	}
	cee.awaitClosed()
	if text, ok := mod.awaitNotice("Kicked"); !ok || text != "Kicked Cee." {
		t.Fatalf("moderator's confirmation = %q", text)
	}

	// the website's kick says the same, and the kicked player can come back
	out, err := f.hub.ModerateUser("WebMod", deeOK.UserID, "kick", 0, "spam")
	if err != nil || out.Target != "Dee" || out.IP != "203.0.113.21" {
		t.Fatalf("web kick = %+v %v", out, err)
	}
	webText, ok := dee.awaitNotice("kicked")
	if !ok || webText != lobbyText || webText != "You were kicked by a moderator: spam." {
		t.Fatalf("web kick notice %q, lobby %q", webText, lobbyText)
	}
	dee.awaitClosed()
	f.waitOnline(1)
	if _, err := f.hub.ModerateUser("WebMod", deeOK.UserID, "kick", 0, ""); !errors.Is(err, ErrNotConnected) {
		t.Fatalf("kick of a gone user: %v", err)
	}

	// mute from the website: told, and their chat is blocked (and logged so)
	dee, deeOK = f.join("Dee", "203.0.113.21")
	out, err = f.hub.ModerateUser("WebMod", deeOK.UserID, "mute", 10*time.Minute, "")
	if err != nil || out.Until.IsZero() {
		t.Fatalf("web mute = %+v %v", out, err)
	}
	if _, ok := dee.awaitNotice("You were muted by a moderator until"); !ok {
		t.Fatal("dee wasn't told about the mute")
	}
	dee.chat(channelLobby, "can you hear me")
	if _, ok := dee.awaitNotice("You are muted"); !ok {
		t.Fatal("muted chat went through")
	}
	if !f.hub.LiftSanction("WebMod", "unmute", "203.0.113.21") || f.hub.LiftSanction("WebMod", "unmute", "203.0.113.21") {
		t.Fatal("unmute")
	}

	// a timeout needs a duration
	if _, err := f.hub.ModerateUser("WebMod", deeOK.UserID, "timeout", 0, ""); !errors.Is(err, ErrNeedsDuration) {
		t.Fatalf("timeout without duration: %v", err)
	}
	// moderators can't be sanctioned
	modUser := f.hub.state.GetUserByName("Moddy")
	if _, err := f.hub.ModerateUser("WebMod", modUser.ID, "ban", 0, ""); !errors.Is(err, ErrModerator) {
		t.Fatalf("ban of a moderator: %v", err)
	}

	// ban from the website: disconnected and refused at the door
	if _, err := f.hub.ModerateUser("WebMod", deeOK.UserID, "ban", 0, "cheating"); err != nil {
		t.Fatal(err)
	}
	if text, ok := dee.awaitNotice("banned"); !ok || text != "You were banned by a moderator: cheating." {
		t.Fatalf("ban notice %q", text)
	}
	dee.awaitClosed()
	if _, err := f.dial("203.0.113.21"); err == nil {
		t.Fatal("banned IP connected again")
	}
	bans, _ := f.hub.Sanctions()
	if len(bans) != 1 || bans[0].By != "WebMod" || bans[0].Name != "Dee" || bans[0].Reason != "cheating" {
		t.Fatalf("bans = %+v", bans)
	}

	// an IP ban disconnects everyone there (but not a moderator), and works offline
	eve, _ := f.join("Eve", "198.51.100.30")
	fay, _ := f.join("Fay", "198.51.100.30")
	if n := f.hub.BanIP("WebMod", "198.51.100.30", "", time.Hour, "ban evasion"); n != 2 {
		t.Fatalf("disconnected %d", n)
	}
	for _, c := range []*wsConn{eve, fay} {
		if _, ok := c.awaitNotice("You were timed out by a moderator until"); !ok {
			t.Fatal("no timeout notice")
		}
	}
	if n := f.hub.BanIP("WebMod", "2001:DB8:0::99", "Ghost", 0, ""); n != 0 {
		t.Fatalf("offline ban disconnected %d", n)
	}
	if ban, _ := f.hub.SanctionsOn("2001:db8::99"); ban == nil || ban.Name != "Ghost" {
		t.Fatalf("offline ban = %+v", ban)
	}
	if n := f.hub.BanIP("WebMod", "192.0.2.1", "", 0, ""); n != 0 {
		t.Fatalf("a moderator was disconnected by an IP ban (%d)", n)
	}

	// the lobby's actions are in the audit trail
	audit, _, _ := f.log.AuditLog(bg, 0, 50)
	found := map[string]bool{}
	for _, e := range audit {
		found[e.Admin+" "+e.Action+" "+e.Source] = true
		if e.Action == "kick" && e.Detail != "Kicked Cee (203.0.113.20): spam" {
			t.Fatalf("kick audit = %+v", e)
		}
	}
	if !found["Mod kick lobby"] || !found["Mod sign-in lobby"] {
		t.Fatalf("audit = %+v", audit)
	}
}

func TestWebCloseRoomEndsTheGame(t *testing.T) {
	f := newModFixture(t)
	ace, _ := f.join("Ace", "203.0.113.5")
	bee, _ := f.join("Bee", "203.0.113.6")
	ace.send(protocol.TypeRoomCreate, protocol.RoomCreateRequest{Name: "Kart", Rom: protocol.RomInfo{Name: "Mario Kart 64"},
		MaxPlayers: 2, Password: "pw"})
	created, _ := ace.await(protocol.TypeRoomCreated)
	var room protocol.RoomCreatedResponse
	json.Unmarshal(created.Data, &room)
	ace.sync()
	bee.send(protocol.TypeRoomJoin, protocol.RoomJoinRequest{RoomID: room.RoomID, Password: "pw"})
	bee.await(protocol.TypeRoomJoinOK)
	bee.sync()
	ace.send(protocol.TypeRoomStart, struct{}{})
	ace.await(protocol.TypeMatchBegin)
	ace.sync()
	bee.sync()

	snap := f.hub.LiveSnapshot()
	if len(snap.Rooms) != 1 || !snap.Rooms[0].InMatch || snap.Rooms[0].MatchID == 0 || len(snap.Rooms[0].Players) != 2 ||
		!snap.Rooms[0].Locked || snap.Rooms[0].Host != "Ace" || snap.Rooms[0].Game != "Mario Kart 64" {
		t.Fatalf("live rooms = %+v", snap.Rooms)
	}
	for _, u := range snap.Users {
		if u.Status != "playing" || u.RoomID != room.RoomID || u.Version != "vdev-1" || u.Connected.IsZero() {
			t.Fatalf("live user = %+v", u)
		}
	}

	name, ok := f.hub.CloseRoom("WebMod", room.RoomID, "rule 3")
	if !ok || name != "Kart" {
		t.Fatalf("close = %q %v", name, ok)
	}
	left, ok := bee.await(protocol.TypeRoomLeft)
	if !ok || !strings.Contains(string(left.Data), "admin_closed") {
		t.Fatalf("bee's ROOM_LEFT = %s", left.Data)
	}
	if _, ok := f.hub.CloseRoom("WebMod", room.RoomID, ""); ok {
		t.Fatal("closed a room twice")
	}
	f.log.Flush()
	games, _, _ := f.log.Games(bg, modlog.GameQuery{})
	if len(games) != 1 || games[0].EndReason != modlog.EndModerator {
		t.Fatalf("games = %+v", games)
	}
}

func TestAdminAuthFailuresBanTheIP(t *testing.T) {
	f := newModFixture(t)
	const ip = "198.51.100.66"
	wrong := func(c *wsConn) {
		c.send(protocol.TypeAdminAuth, protocol.AdminAuthRequest{Password: "guess"})
	}
	bystander, _ := f.join("Bystander", ip)
	// three tries on one connection, two on another: the count is per IP
	first, _ := f.join("Guesser", ip)
	for i := 0; i < 3; i++ {
		wrong(first)
		if _, ok := first.await(protocol.TypeAdminAuthFail); !ok {
			t.Fatalf("try %d: no ADMIN_AUTH_FAIL", i+1)
		}
	}
	second, _ := f.join("Guesser2", ip)
	wrong(second)
	if _, ok := second.await(protocol.TypeAdminAuthFail); !ok {
		t.Fatal("fourth try: no ADMIN_AUTH_FAIL")
	}
	if _, banned := f.mod.IsBanned(ip); banned {
		t.Fatal("banned after four failures")
	}
	wrong(second)
	// every connection from the IP hears why and is closed
	for _, c := range []*wsConn{second, first, bystander} {
		if _, ok := c.awaitNotice("too many failed moderator logins"); !ok {
			t.Fatal("no ban notice")
		}
		c.awaitClosed()
	}
	s, banned := f.mod.IsBanned(ip)
	if !banned || s.By != moderation.AutoBanBy || !s.Until.IsZero() {
		t.Fatalf("ban = %+v %v", s, banned)
	}
	if _, err := f.dial(ip); err == nil {
		t.Fatal("the banned IP connected again")
	}
	audit, _, _ := f.log.AuditLog(bg, 0, 10)
	if len(audit) == 0 || audit[0].Admin != "auto" || audit[0].Action != "ban" || audit[0].Source != modlog.SourceAuto ||
		!strings.Contains(audit[0].Detail, "Banned "+ip+": 5 failed moderator logins in a row (lobby)") {
		t.Fatalf("audit = %+v", audit)
	}

	// a success in between starts the IP's count over (each connection keeps
	// its own count of 5, as before, so each round is a new connection)
	const ip2 = "198.51.100.67"
	for round := 0; round < 3; round++ {
		c, _ := f.join(fmt.Sprintf("Mod%d", round), ip2)
		for i := 0; i < maxAuthFails-1; i++ {
			wrong(c)
			if _, ok := c.await(protocol.TypeAdminAuthFail); !ok {
				t.Fatalf("round %d try %d: closed", round, i+1)
			}
		}
		f.signInModerator(c)
	}
	if _, banned := f.mod.IsBanned(ip2); banned {
		t.Fatal("banned although a correct password came between the failures")
	}
}

func TestAdminAuthLoopbackIsLockedOutNotBanned(t *testing.T) {
	f := newModFixture(t)
	c, _ := f.join("Local", "127.0.0.1")
	for i := 0; i < maxAuthFails-1; i++ {
		c.send(protocol.TypeAdminAuth, protocol.AdminAuthRequest{Password: "guess"})
		c.await(protocol.TypeAdminAuthFail)
	}
	c.send(protocol.TypeAdminAuth, protocol.AdminAuthRequest{Password: "guess"})
	// this connection's own limit still disconnects it
	if _, ok := c.awaitNotice("Too many failed login attempts."); !ok {
		t.Fatal("no disconnect notice")
	}
	c.awaitClosed()
	if _, banned := f.mod.IsBanned("127.0.0.1"); banned {
		t.Fatal("loopback was banned")
	}
	// a new connection gets in, but even the right password is refused for now
	again, _ := f.join("Local2", "127.0.0.1")
	again.send(protocol.TypeAdminAuth, protocol.AdminAuthRequest{Password: testModPassword})
	env, ok := again.await(protocol.TypeAdminAuthFail)
	if !ok || !strings.Contains(string(env.Data), "Try again later") {
		t.Fatalf("locked-out sign-in = %s %v", env.Data, ok)
	}
}

func TestModLogLateJoinerAndLeaver(t *testing.T) {
	f := newLateJoinFixture(t, 3)
	log, err := modlog.Open(filepath.Join(t.TempDir(), "modlog.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { log.Close() })
	f.h.SetModLog(log)
	f.user("host", true)
	f.user("guest", true)
	f.user("joiner", true)
	match := f.startMatch(3, "host", "guest")
	f.send("joiner", protocol.TypeRoomJoin, protocol.RoomJoinRequest{RoomID: f.room.ID})
	joiner := f.users["joiner"]
	f.send("host", protocol.TypeMatchAdmit, protocol.MatchAdmitRequest{MatchID: match.ID, UserID: joiner.ID})
	admit := match.CurrentAdmission()
	if admit == nil {
		t.Fatal("no admission")
	}
	f.send("host", protocol.TypeMatchAdmitSignal, protocol.MatchAdmitSignalPayload{MatchID: match.ID, AdmitID: admit.ID,
		Step: protocol.AdmitStepGo})
	if joiner.CurrentMatch != match {
		t.Fatal("join not committed")
	}
	f.send("guest", protocol.TypeMatchFinished, struct{}{})
	f.send("host", protocol.TypeRoomLeave, struct{}{}) // takes the match with them
	log.Flush()

	games, _, _ := log.Games(bg, modlog.GameQuery{})
	if len(games) != 1 || games[0].EndReason != modlog.EndHostLeft || games[0].Ended == 0 {
		t.Fatalf("games = %+v", games)
	}
	byName := map[string]modlog.PlayerRow{}
	for _, p := range games[0].Players {
		byName[p.Name] = p
	}
	if p := byName["joiner"]; p.JoinedLate == 0 || p.Slot != 3 {
		t.Fatalf("joiner = %+v", p)
	}
	if p := byName["guest"]; p.Left == 0 || p.JoinedLate != 0 {
		t.Fatalf("guest = %+v", p)
	}
	if p := byName["host"]; p.Left != 0 {
		t.Fatalf("the host's leave ended the game, it isn't a leave: %+v", p)
	}
	if room, match := log.MaxIDs(); room != f.room.ID || match != games[0].MatchID {
		t.Fatalf("max ids = %d %d", room, match)
	}
}
