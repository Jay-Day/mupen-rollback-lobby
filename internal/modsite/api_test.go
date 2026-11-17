package modsite

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jay-day/rmgk-lobby/internal/modlog"
	"github.com/jay-day/rmgk-lobby/internal/protocol"
)

type liveReply struct {
	Users []struct {
		ID        uint64
		Username  string
		IP        string
		Moderator bool
		Version   string
		Connected int64
		Status    string
		RoomID    uint64
		RoomName  string
		Muted     *struct {
			IP, Reason, By string
			Until          int64
		}
	}
	Rooms []struct {
		ID         uint64
		Name       string
		Game       string
		Host       string
		Players    []string
		InMatch    bool
		QuickMatch bool
		Locked     bool
		Created    int64
	}
	Bans, Mutes int
}

func itoa(n uint64) string { return strconv.FormatUint(n, 10) }

func TestLiveAndUserActions(t *testing.T) {
	ts := newTestSite(t)
	ace := ts.connect("Ace", "203.0.113.5")
	bee := ts.connect("Bee", "2001:db8::7")
	lobbyMod := ts.connect("Modder", "192.0.2.50")
	lobbyMod.send(protocol.TypeAdminAuth, protocol.AdminAuthRequest{Password: modPassword})
	if _, ok := lobbyMod.await(protocol.TypeAdminAuthOK); !ok {
		t.Fatal("lobby moderator sign-in failed")
	}
	ace.send(protocol.TypeRoomCreate, protocol.RoomCreateRequest{Name: "Remix night", Rom: protocol.RomInfo{Name: "Smash Remix"},
		MaxPlayers: 4, Password: "pw"})
	created, _ := ace.await(protocol.TypeRoomCreated)
	var room protocol.RoomCreatedResponse
	json.Unmarshal(created.Data, &room)
	ace.sync()
	cookie := ts.login(modIP)

	var live liveReply
	if code := ts.api("GET", "/api/admin/live", cookie, nil, &live); code != 200 || len(live.Users) != 3 || len(live.Rooms) != 1 {
		t.Fatalf("live = %d %+v", code, live)
	}
	for _, u := range live.Users {
		switch u.Username {
		case "Ace":
			if u.Status != "room" || u.RoomID != room.RoomID || u.RoomName != "Remix night" || u.IP != "203.0.113.5" ||
				u.Version != "vdev-1" || u.Connected == 0 {
				t.Fatalf("ace = %+v", u)
			}
		case "Bee":
			if u.Status != "lobby" || u.Muted != nil || u.IP != "2001:db8::7" {
				t.Fatalf("bee = %+v", u)
			}
		case "Modder":
			if !u.Moderator {
				t.Fatalf("modder = %+v", u)
			}
		}
	}
	if r := live.Rooms[0]; r.Name != "Remix night" || r.Game != "Smash Remix" || r.Host != "Ace" || !r.Locked ||
		r.InMatch || len(r.Players) != 1 || r.Created == 0 {
		t.Fatalf("room = %+v", r)
	}

	path := func(id uint64, action string) string { return "/api/admin/users/" + itoa(id) + "/" + action }
	if code := ts.api("POST", path(lobbyMod.id, "ban"), cookie, map[string]any{"reason": "x"}, nil); code != 409 {
		t.Fatalf("ban a moderator = %d", code)
	}
	if code := ts.api("POST", path(bee.id, "kick"), cookie, map[string]any{"reason": strings.Repeat("é", 201)}, nil); code != 400 {
		t.Fatalf("long reason = %d", code)
	}
	if code := ts.api("POST", path(bee.id, "mute"), cookie, map[string]any{"duration": "soon"}, nil); code != 400 {
		t.Fatalf("bad duration = %d", code)
	}
	if code := ts.api("POST", path(bee.id, "timeout"), cookie, map[string]any{"duration": ""}, nil); code != 400 {
		t.Fatalf("timeout without duration = %d", code)
	}

	var muted struct {
		OK     bool
		Target string
		IP     string
		Until  int64
	}
	if code := ts.api("POST", path(bee.id, "mute"), cookie, map[string]any{"duration": "10m", "reason": "caps"}, &muted); code != 200 ||
		!muted.OK || muted.Target != "Bee" || muted.IP != "2001:db8::7" || muted.Until < time.Now().Unix()+500 {
		t.Fatalf("mute = %d %+v", code, muted)
	}
	if text, ok := bee.awaitNotice("muted"); !ok || !strings.HasSuffix(text, ": caps.") {
		t.Fatalf("bee's mute notice = %q", text)
	}
	ts.api("GET", "/api/admin/live", cookie, nil, &live)
	for _, u := range live.Users {
		if u.Username == "Bee" && (u.Muted == nil || u.Muted.Reason != "caps" || u.Muted.By != "Wren") {
			t.Fatalf("bee after mute = %+v", u)
		}
	}
	if live.Mutes != 1 {
		t.Fatalf("mutes = %d", live.Mutes)
	}

	var kicked struct {
		Target string
		Until  int64
	}
	if code := ts.api("POST", path(bee.id, "kick"), cookie, map[string]any{"reason": "spam"}, &kicked); code != 200 ||
		kicked.Target != "Bee" || kicked.Until != 0 {
		t.Fatalf("kick = %d %+v", code, kicked)
	}
	if text, ok := bee.awaitNotice("kicked"); !ok || text != "You were kicked by a moderator: spam." {
		t.Fatalf("kick notice = %q", text)
	}
	bee.awaitClosed()
	ts.waitOnline(2)
	if code := ts.api("POST", path(bee.id, "kick"), cookie, map[string]any{}, nil); code != 404 {
		t.Fatalf("kick a gone user = %d", code)
	}

	var closed struct{ Name string }
	roomPath := "/api/admin/rooms/" + itoa(room.RoomID) + "/close"
	if code := ts.api("POST", roomPath, cookie, map[string]any{"reason": "testing"}, &closed); code != 200 || closed.Name != "Remix night" {
		t.Fatalf("close = %d %+v", code, closed)
	}
	if left, ok := ace.await(protocol.TypeRoomLeft); !ok || !strings.Contains(string(left.Data), "admin_closed") {
		t.Fatalf("ace's ROOM_LEFT = %s", left.Data)
	}
	if code := ts.api("POST", roomPath, cookie, map[string]any{}, nil); code != 404 {
		t.Fatalf("close twice = %d", code)
	}

	// ban: disconnected, refused at the door
	if code := ts.api("POST", path(ace.id, "ban"), cookie, map[string]any{"reason": "cheating"}, &muted); code != 200 || muted.Until != 0 {
		t.Fatalf("ban = %d %+v", code, muted)
	}
	if text, ok := ace.awaitNotice("banned"); !ok || text != "You were banned by a moderator: cheating." {
		t.Fatalf("ban notice = %q", text)
	}
	ace.awaitClosed()
	ts.waitOnline(1)

	// every action is in the audit trail, as done from the site
	audit, _, _ := ts.log.AuditLog(context.Background(), 0, 50)
	actions := map[string]string{}
	for _, e := range audit {
		if e.Source == modlog.SourceSite && e.Admin == "Wren" {
			actions[e.Action] = e.Detail
			if e.IP != modIP {
				t.Fatalf("audit ip = %+v", e)
			}
		}
	}
	for action, want := range map[string]string{"mute": "Muted Bee (2001:db8::7) until", "kick": "Kicked Bee (2001:db8::7): spam",
		"closeroom": "\"Remix night\": testing", "ban": "Banned Ace (203.0.113.5): cheating", "sign-in": "Signed in"} {
		if !strings.Contains(actions[action], want) {
			t.Fatalf("audit %s = %q, want %q", action, actions[action], want)
		}
	}
}

func TestSanctionsByIP(t *testing.T) {
	ts := newTestSite(t)
	eve := ts.connect("Eve", "198.51.100.30")
	cookie := ts.login(modIP)
	var banned struct {
		OK           bool
		Disconnected int
	}
	if code := ts.api("POST", "/api/admin/sanctions/ban", cookie, map[string]any{"ip": "2001:DB8::99", "name": "Ghost",
		"reason": "alt"}, &banned); code != 200 || !banned.OK || banned.Disconnected != 0 {
		t.Fatalf("offline ban = %d %+v", code, banned)
	}
	if code := ts.api("POST", "/api/admin/sanctions/ban", cookie, map[string]any{"ip": "198.51.100.30", "duration": "1h"},
		&banned); code != 200 || banned.Disconnected != 1 {
		t.Fatalf("online timeout = %d %+v", code, banned)
	}
	eve.awaitClosed()
	if code := ts.api("POST", "/api/admin/sanctions/mute", cookie, map[string]any{"ip": "192.0.2.7", "duration": "2d"}, nil); code != 200 {
		t.Fatalf("mute = %d", code)
	}
	var list struct {
		Bans []struct {
			IP, Name, Reason, By string
			Set, Until           int64
		}
		Mutes []struct {
			IP    string
			Until int64
		}
	}
	ts.api("GET", "/api/admin/sanctions", cookie, nil, &list)
	if len(list.Bans) != 2 || len(list.Mutes) != 1 || list.Mutes[0].Until == 0 {
		t.Fatalf("sanctions = %+v", list)
	}
	for _, b := range list.Bans {
		if b.IP == "2001:db8::99" && (b.Name != "Ghost" || b.Until != 0 || b.By != "Wren" || b.Set == 0) {
			t.Fatalf("ghost ban = %+v", b)
		}
		if b.IP == "198.51.100.30" && (b.Name != "Eve" || b.Until == 0) {
			t.Fatalf("eve's timeout = %+v", b)
		}
	}
	for _, step := range []struct {
		action, ip string
		code       int
	}{
		{"unban", "2001:db8::99", 200}, {"unban", "2001:db8::99", 404},
		{"unmute", "192.0.2.7", 200}, {"unmute", "192.0.2.7", 404},
	} {
		if code := ts.api("POST", "/api/admin/sanctions/"+step.action, cookie, map[string]any{"ip": step.ip}, nil); code != step.code {
			t.Fatalf("%s %s = %d, want %d", step.action, step.ip, code, step.code)
		}
	}
	var audit struct {
		Entries []struct{ Action, Detail, Source string }
		More    bool
	}
	ts.api("GET", "/api/admin/audit", cookie, nil, &audit)
	got := []string{}
	for _, e := range audit.Entries {
		got = append(got, e.Action)
	}
	if strings.Join(got, ",") != "unmute,unban,mute,timeout,ban,sign-in" || audit.More {
		t.Fatalf("audit = %v", got)
	}
	if !strings.Contains(audit.Entries[3].Detail, "Timed out 198.51.100.30 until") ||
		!strings.Contains(audit.Entries[3].Detail, "(1 disconnected)") {
		t.Fatalf("timeout audit = %q", audit.Entries[3].Detail)
	}
	// paging
	var page struct {
		Entries []struct{ ID int64 }
		More    bool
	}
	if ts.api("GET", "/api/admin/audit?limit=4", cookie, nil, &page); len(page.Entries) != 4 || !page.More {
		t.Fatalf("audit page = %+v", page)
	}
	if ts.api("GET", "/api/admin/audit?limit=4&before="+strconv.FormatInt(page.Entries[3].ID, 10), cookie, nil, &page); len(page.Entries) != 2 || page.More {
		t.Fatalf("older audit page = %+v", page)
	}
}

func TestLogEndpoints(t *testing.T) {
	ts := newTestSite(t)
	clock := time.Unix(1_800_000_000, 0)
	ts.log.SetClock(func() time.Time { return clock })
	l := ts.log
	a := l.ConnectionOpened(modlog.Conn{IP: "203.0.113.5", Username: "Ace", Country: "US"})
	l.ConnectionClosed(a)
	l.ConnectionOpened(modlog.Conn{IP: "203.0.113.5", Username: "Alt"})
	l.ConnectionOpened(modlog.Conn{IP: "2001:db8::7", Username: "ace"})
	for i := 0; i < 5; i++ {
		l.Chat(modlog.ChatMessage{Channel: "lobby", Name: "Ace", IP: "203.0.113.5", Text: "hi there"})
	}
	l.Chat(modlog.ChatMessage{Channel: "room", RoomID: 4, RoomName: "R", MatchID: 9, Name: "Alt", Text: "badword",
		Blocked: modlog.BlockedFilter})
	l.Chat(modlog.ChatMessage{Channel: "lobby", Name: "Alt", IP: "203.0.113.5", Text: `<img src=x onerror="alert(1)"><script>x()</script>`})
	l.GameStarted(modlog.Game{MatchID: 9, RoomID: 4, RoomName: "R", Game: "Mario Kart 64",
		Players: []modlog.Player{{UserID: 1, Name: "Ace", IP: "203.0.113.5", Slot: 1}, {UserID: 2, Name: "Alt", Slot: 2}}})
	l.GameEnded(9, modlog.EndFinished)
	l.Flush()
	cookie := ts.login(modIP)

	var chat struct {
		Messages []struct {
			ID       int64
			Channel  string
			RoomName string
			MatchID  uint64
			From, IP string
			Text     string
			Blocked  string
		}
		More bool
	}
	// player text travels as JSON strings, its markup escaped
	raw := ts.serve(req{method: "GET", path: "/api/admin/chat?limit=1", ip: modIP, cookie: cookie}).Body.String()
	if strings.Contains(raw, "<img") || strings.Contains(raw, "<script") || !strings.Contains(raw, `\`+"u003cimg") {
		t.Fatalf("chat JSON = %s", raw)
	}
	if code := ts.api("GET", "/api/admin/chat?limit=4", cookie, nil, &chat); code != 200 || len(chat.Messages) != 4 || !chat.More ||
		!strings.HasPrefix(chat.Messages[0].Text, "<img") || chat.Messages[1].Blocked != "filter" || chat.Messages[1].MatchID != 9 {
		t.Fatalf("chat = %d %+v", code, chat)
	}
	before := chat.Messages[3].ID
	if ts.api("GET", "/api/admin/chat?before="+strconv.FormatInt(before, 10), cookie, nil, &chat); len(chat.Messages) != 3 || chat.More {
		t.Fatalf("older = %+v", chat)
	}
	if ts.api("GET", "/api/admin/chat?after="+strconv.FormatInt(chat.Messages[0].ID, 10), cookie, nil, &chat); len(chat.Messages) != 4 ||
		chat.Messages[0].ID > chat.Messages[1].ID {
		t.Fatalf("tail = %+v", chat)
	}
	if ts.api("GET", "/api/admin/chat?blocked=1&name=ALT", cookie, nil, &chat); len(chat.Messages) != 1 {
		t.Fatalf("blocked = %+v", chat)
	}
	if ts.api("GET", "/api/admin/chat?match=9&channel=room", cookie, nil, &chat); len(chat.Messages) != 1 {
		t.Fatalf("by match = %+v", chat)
	}
	if ts.api("GET", "/api/admin/chat?q=HI+THERE&ip=203.0.113.5", cookie, nil, &chat); len(chat.Messages) != 5 {
		t.Fatalf("text search = %+v", chat)
	}

	var trace struct {
		Name string
		IPs  []struct {
			IP          string
			Connections int
			Country     string
			Ban, Mute   *struct{ IP string }
			Names       []struct{ Name string }
		}
		Online bool
	}
	if code := ts.api("GET", "/api/admin/trace?name=ACE", cookie, nil, &trace); code != 200 || trace.Name != "ace" ||
		len(trace.IPs) != 2 || trace.Online {
		t.Fatalf("trace = %d %+v", code, trace)
	}
	ts.hub.BanIP("Wren", "203.0.113.5", "Ace", 0, "")
	ts.api("GET", "/api/admin/trace?name=ace", cookie, nil, &trace)
	foundBan := false
	for _, ip := range trace.IPs {
		if ip.IP == "203.0.113.5" {
			foundBan = ip.Ban != nil && len(ip.Names) == 1 && ip.Names[0].Name == "Alt" && ip.Country == "US"
		}
	}
	if !foundBan {
		t.Fatalf("trace after ban = %+v", trace)
	}
	var ipTrace struct {
		IP    string
		Ban   *struct{ IP string }
		Names []struct {
			Name     string
			OtherIPs int
		}
		Online []struct{ ID uint64 }
	}
	if code := ts.api("GET", "/api/admin/trace?ip=203.0.113.5", cookie, nil, &ipTrace); code != 200 || ipTrace.Ban == nil ||
		len(ipTrace.Names) != 2 || ipTrace.Online == nil {
		t.Fatalf("ip trace = %d %+v", code, ipTrace)
	}
	for _, q := range []string{"name=nobody", "ip=192.0.2.200"} {
		if code := ts.api("GET", "/api/admin/trace?"+q, cookie, nil, nil); code != 404 {
			t.Fatalf("trace %s = %d", q, code)
		}
	}

	var conns struct {
		Connections []struct {
			ID           int64
			IP, Username string
			Disconnected int64
		}
		More bool
	}
	if ts.api("GET", "/api/admin/connections?name=alt", cookie, nil, &conns); len(conns.Connections) != 1 ||
		conns.Connections[0].Disconnected != 0 {
		t.Fatalf("connections = %+v", conns)
	}
	if ts.api("GET", "/api/admin/connections?limit=1", cookie, nil, &conns); len(conns.Connections) != 1 || !conns.More {
		t.Fatalf("connections page = %+v", conns)
	}

	var games struct {
		Games []struct {
			ID        int64
			MatchID   uint64
			Game      string
			EndReason string
			Players   []struct {
				Name string
				Slot int
			}
		}
	}
	if ts.api("GET", "/api/admin/games?name=alt", cookie, nil, &games); len(games.Games) != 1 ||
		games.Games[0].EndReason != "finished" || len(games.Games[0].Players) != 2 {
		t.Fatalf("games = %+v", games)
	}
	gameID := games.Games[0].ID
	if ts.api("GET", "/api/admin/games?match=9", cookie, nil, &games); len(games.Games) != 1 {
		t.Fatalf("games by match = %+v", games)
	}
	var detail struct {
		Game      struct{ ID int64 }
		Events    []struct{ Type, Text string }
		ChatCount int
	}
	if code := ts.api("GET", "/api/admin/games/"+strconv.FormatInt(gameID, 10), cookie, nil, &detail); code != 200 ||
		len(detail.Events) < 2 || detail.ChatCount != 1 {
		t.Fatalf("game detail = %d %+v", code, detail)
	}
	if code := ts.api("GET", "/api/admin/games/9999", cookie, nil, nil); code != 404 {
		t.Fatalf("missing game = %d", code)
	}
}

func TestLogsOff(t *testing.T) {
	ts := newTestSite(t, siteOpts{noLog: true})
	ts.connect("Ace", "203.0.113.5")
	cookie := ts.login(modIP)
	for _, path := range []string{"/api/admin/chat", "/api/admin/trace?name=a", "/api/admin/connections",
		"/api/admin/games", "/api/admin/games/1", "/api/admin/audit"} {
		var reply struct{ Error string }
		if code := ts.api("GET", path, cookie, nil, &reply); code != 503 || reply.Error != "logging is off on this server" {
			t.Fatalf("%s = %d %+v", path, code, reply)
		}
	}
	var session struct {
		Online  int
		Logging bool
	}
	if ts.api("GET", "/api/admin/session", cookie, nil, &session); session.Logging || session.Online != 1 {
		t.Fatalf("session = %+v", session)
	}
	var live liveReply
	if code := ts.api("GET", "/api/admin/live", cookie, nil, &live); code != 200 || len(live.Users) != 1 {
		t.Fatalf("live = %d %+v", code, live)
	}
}
