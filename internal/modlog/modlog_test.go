package modlog

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"
)

var bg = context.Background()

// testClock is a settable clock.
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func openTest(t *testing.T) (*Logger, *testClock, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "modlog.db")
	l, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	clock := &testClock{t: time.Unix(1_700_000_000, 0)}
	l.SetClock(clock.now)
	t.Cleanup(func() { l.Close() })
	return l, clock, path
}

func TestNilLoggerDoesNothing(t *testing.T) {
	var l *Logger
	if id := l.ConnectionOpened(Conn{Username: "a"}); id != 0 {
		t.Fatalf("nil logger gave id %d", id)
	}
	l.ConnectionClosed(1)
	l.Chat(ChatMessage{Name: "a", Text: "hi"})
	l.GameStarted(Game{MatchID: 1})
	l.GamePlayerLeft(1, 1, "left")
	l.GameEnded(1, EndFinished)
	l.RoomClosed(1, "r", EndHostLeft, "")
	l.Event(Event{Text: "x"})
	l.Audit(AuditEntry{Admin: "a", Action: "kick"})
	l.Flush()
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	if room, match := l.MaxIDs(); room != 0 || match != 0 {
		t.Fatal("nil MaxIDs")
	}
}

func TestFileIsPrivate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no owner-only mode bits on Windows")
	}
	path := filepath.Join(t.TempDir(), "modlog.db")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	l, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		t.Fatalf("mode = %v", fi.Mode().Perm())
	}
}

func TestChatPagingAndFilters(t *testing.T) {
	l, clock, _ := openTest(t)
	for i := 0; i < 10; i++ {
		clock.advance(time.Second)
		l.Chat(ChatMessage{Channel: "lobby", Name: "Ace", UserID: 1, IP: "203.0.113.5", Text: "hello number " + string(rune('a'+i))})
	}
	l.Chat(ChatMessage{Channel: "room", RoomID: 7, RoomName: "Remix 1v1", MatchID: 3, Name: "Bee", IP: "198.51.100.2",
		Text: "GG WP Ünïcode"})
	l.Chat(ChatMessage{Channel: "lobby", Name: "bee", IP: "198.51.100.2", Text: "bad words", Blocked: BlockedFilter})
	l.Chat(ChatMessage{Channel: "spectate", RoomID: 7, MatchID: 3, Name: "Ace", Text: "nice"})
	l.Flush()

	page, more, err := l.ChatLog(bg, ChatQuery{Limit: 5})
	if err != nil || len(page) != 5 || !more {
		t.Fatalf("first page = %d rows, more %v, %v", len(page), more, err)
	}
	if page[0].Text != "nice" || page[0].Channel != "spectate" {
		t.Fatalf("newest first: %+v", page[0])
	}
	older, more, _ := l.ChatLog(bg, ChatQuery{Limit: 5, Before: page[4].ID})
	if len(older) != 5 || !more || older[0].ID >= page[4].ID {
		t.Fatalf("before page = %+v more %v", older, more)
	}
	last, more, _ := l.ChatLog(bg, ChatQuery{Limit: 5, Before: older[4].ID})
	if len(last) != 3 || more {
		t.Fatalf("last page = %d more %v", len(last), more)
	}
	// a live tail reads oldest first after an id
	tail, more, _ := l.ChatLog(bg, ChatQuery{After: page[2].ID})
	if len(tail) != 2 || tail[0].ID != page[1].ID || tail[1].ID != page[0].ID || more {
		t.Fatalf("tail = %+v more %v", tail, more)
	}
	tail, more, _ = l.ChatLog(bg, ChatQuery{After: older[4].ID, Limit: 3})
	if len(tail) != 3 || !more || tail[0].ID != older[4].ID+1 {
		t.Fatalf("limited tail = %+v more %v", tail, more)
	}

	// names match without regard to case
	byName, _, _ := l.ChatLog(bg, ChatQuery{Name: "BEE"})
	if len(byName) != 2 {
		t.Fatalf("by name = %+v", byName)
	}
	if got, _, _ := l.ChatLog(bg, ChatQuery{Text: "gg wp ünï"}); len(got) != 1 || got[0].RoomName != "Remix 1v1" ||
		got[0].MatchID != 3 || got[0].From != "Bee" {
		t.Fatalf("text search = %+v", got)
	}
	if got, _, _ := l.ChatLog(bg, ChatQuery{Blocked: "any"}); len(got) != 1 || got[0].Blocked != BlockedFilter {
		t.Fatalf("blocked = %+v", got)
	}
	if got, _, _ := l.ChatLog(bg, ChatQuery{IP: "203.0.113.5", Channel: "lobby"}); len(got) != 10 {
		t.Fatalf("by ip = %d", len(got))
	}
	if got, _, _ := l.ChatLog(bg, ChatQuery{RoomID: 7}); len(got) != 2 {
		t.Fatalf("by room = %d", len(got))
	}
	if got, _, _ := l.ChatLog(bg, ChatQuery{MatchID: 3, Channel: "room"}); len(got) != 1 {
		t.Fatalf("by match = %d", len(got))
	}
	// a query with a quote in it is a value, never SQL
	if got, _, err := l.ChatLog(bg, ChatQuery{Text: "' OR 1=1 --", Name: "x' OR '1'='1"}); err != nil || len(got) != 0 {
		t.Fatalf("injection-looking query = %+v %v", got, err)
	}
}

func TestConnectionsAndTrace(t *testing.T) {
	l, clock, _ := openTest(t)
	connect := func(name, ip string) int64 {
		clock.advance(time.Minute)
		return l.ConnectionOpened(Conn{IP: ip, Username: name, Version: "v0.9.16", Country: "SE"})
	}
	a1 := connect("Ace", "203.0.113.5")
	l.ConnectionClosed(a1)
	connect("AceAlt", "203.0.113.5") // an alt on the same IP, still online
	a2 := connect("ace", "2001:db8::7")
	clock.advance(time.Minute)
	l.ConnectionClosed(a2)
	connect("Bee", "198.51.100.2")
	l.Flush()

	rows, more, err := l.Connections(bg, ConnQuery{Name: "ACE"})
	if err != nil || len(rows) != 2 || more {
		t.Fatalf("connections by name = %+v %v %v", rows, more, err)
	}
	if rows[0].IP != "2001:db8::7" || rows[0].Disconnected == 0 || rows[0].Version != "v0.9.16" {
		t.Fatalf("newest connection = %+v", rows[0])
	}
	page, more, _ := l.Connections(bg, ConnQuery{Limit: 2})
	if len(page) != 2 || !more {
		t.Fatalf("page = %d more %v", len(page), more)
	}
	if next, more, _ := l.Connections(bg, ConnQuery{Limit: 2, Before: page[1].ID}); len(next) != 2 || more {
		t.Fatalf("next page = %d more %v", len(next), more)
	}

	trace, ok, err := l.TraceName(bg, "ACE")
	if err != nil || !ok {
		t.Fatalf("trace: %v %v", ok, err)
	}
	if trace.Name != "ace" || len(trace.IPs) != 2 {
		t.Fatalf("trace = %+v", trace)
	}
	var v4, v6 SeenIP
	for _, ip := range trace.IPs {
		if ip.IP == "203.0.113.5" {
			v4 = ip
		} else {
			v6 = ip
		}
	}
	if v4.Connections != 1 || len(v4.Names) != 1 || v4.Names[0].Name != "AceAlt" || v4.Country != "SE" {
		t.Fatalf("v4 = %+v", v4)
	}
	// the alt is still online: last seen is now
	if v4.Names[0].Last != clock.now().Unix() {
		t.Fatalf("online alt last = %d, now %d", v4.Names[0].Last, clock.now().Unix())
	}
	if len(v6.Names) != 0 {
		t.Fatalf("v6 = %+v", v6)
	}

	byIP, ok, _ := l.TraceIP(bg, "203.0.113.5")
	if !ok || len(byIP.Names) != 2 || byIP.Country != "SE" {
		t.Fatalf("ip trace = %+v", byIP)
	}
	for _, n := range byIP.Names {
		switch n.Name {
		case "AceAlt":
			if n.OtherIPs != 0 {
				t.Fatalf("alt = %+v", n)
			}
		case "Ace":
			if n.OtherIPs != 1 || n.Connections != 1 {
				t.Fatalf("ace = %+v", n)
			}
		default:
			t.Fatalf("unexpected %+v", n)
		}
	}
	if _, ok, _ := l.TraceName(bg, "nobody"); ok {
		t.Fatal("unknown name traced")
	}
	if _, ok, _ := l.TraceIP(bg, "192.0.2.1"); ok {
		t.Fatal("unknown ip traced")
	}
}

func TestGameLifecycle(t *testing.T) {
	l, clock, _ := openTest(t)
	l.GameStarted(Game{MatchID: 40, RoomID: 9, RoomName: "Kart night", Game: "Mario Kart 64", RomMD5: "ABC",
		Players: []Player{
			{UserID: 1, Name: "Ace", IP: "203.0.113.5", Slot: 1},
			{UserID: 2, Name: "Bee", IP: "198.51.100.2", Slot: 2},
			{UserID: 3, Name: "Cee", IP: "198.51.100.2", Slot: 3},
		}})
	clock.advance(2 * time.Minute)
	l.GameConnected(40)
	l.GamePlayerLeft(40, 2, "dropped")
	l.GamePlayerLeft(40, 2, "left") // only the first leave counts
	clock.advance(time.Minute)
	l.GamePlayerJoined(40, Player{UserID: 4, Name: "Late", IP: "192.0.2.9", Slot: 4})
	l.GameSpectators(40, 3)
	l.GameSpectators(40, 1)
	l.Chat(ChatMessage{Channel: "room", RoomID: 9, MatchID: 40, Name: "Ace", Text: "gl"})
	l.Chat(ChatMessage{Channel: "spectate", RoomID: 9, MatchID: 40, Name: "Viewer", Text: "nice"})
	l.Chat(ChatMessage{Channel: "lobby", Name: "Ace", Text: "not counted"})
	clock.advance(time.Minute)
	l.GameEnded(40, EndFinished)
	l.GameEnded(40, EndHostLeft) // already over
	l.Flush()

	games, more, err := l.Games(bg, GameQuery{})
	if err != nil || len(games) != 1 || more {
		t.Fatalf("games = %+v %v", games, err)
	}
	g := games[0]
	if g.MatchID != 40 || g.Game != "Mario Kart 64" || g.EndReason != EndFinished || g.Ended != g.Started+240 ||
		g.Spectators != 3 || len(g.Players) != 4 {
		t.Fatalf("game = %+v", g)
	}
	for _, p := range g.Players {
		switch p.Name {
		case "Bee":
			if p.Left != g.Started+120 {
				t.Fatalf("bee left = %+v", p)
			}
		case "Late":
			if p.JoinedLate != g.Started+180 || p.Slot != 4 {
				t.Fatalf("late = %+v", p)
			}
		}
	}
	if got, _, _ := l.Games(bg, GameQuery{Name: "late"}); len(got) != 1 {
		t.Fatal("late joiner's games")
	}
	if got, _, _ := l.Games(bg, GameQuery{IP: "198.51.100.2"}); len(got) != 1 {
		t.Fatal("games by ip")
	}
	if got, _, _ := l.Games(bg, GameQuery{Match: 40}); len(got) != 1 {
		t.Fatal("games by match")
	}
	detail, events, chats, err := l.GameByID(bg, g.ID)
	if err != nil || detail.ID != g.ID || chats != 2 {
		t.Fatalf("detail chats=%d err=%v", chats, err)
	}
	types := map[string]int{}
	for _, e := range events {
		types[e.Type]++
	}
	if types[EventGameStarted] != 1 || types[EventPlayerLeft] != 1 || types[EventPlayerJoined] != 1 ||
		types[EventGameEnded] != 1 {
		t.Fatalf("events = %+v", events)
	}
	if _, _, _, err := l.GameByID(bg, 999); err != ErrNotFound {
		t.Fatalf("missing game: %v", err)
	}
	if room, match := l.MaxIDs(); room != 9 || match != 40 {
		t.Fatalf("max ids = %d %d", room, match)
	}
}

func TestPunchFailAndRoomClose(t *testing.T) {
	l, clock, _ := openTest(t)
	l.GameStarted(Game{MatchID: 1, RoomID: 2, Players: []Player{{UserID: 1, Name: "A", Slot: 1}, {UserID: 2, Name: "B", Slot: 2}}})
	l.GamePunchFailed(1, "A", "B")
	clock.advance(30 * time.Second)
	l.RoomClosed(2, "room", EndHostLeft, "")
	l.GameStarted(Game{MatchID: 5, RoomID: 6, Players: []Player{{UserID: 9, Name: "C", Slot: 1}}})
	l.Flush()
	if l.OpenGames() != 1 {
		t.Fatalf("open games = %d", l.OpenGames())
	}
	games, _, _ := l.Games(bg, GameQuery{Match: 1})
	if len(games) != 1 || games[0].EndReason != EndPunchFailed {
		t.Fatalf("game = %+v", games)
	}
}

func TestStartupClosesOpenRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "modlog.db")
	l, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	conn := l.ConnectionOpened(Conn{IP: "203.0.113.5", Username: "Ace"})
	l.GameStarted(Game{MatchID: 3, RoomID: 4, Players: []Player{{UserID: 1, Name: "Ace", Slot: 1}}})
	l.Flush()
	// a crash: nothing closes the rows; the heartbeat says when it was last alive
	if _, err := l.db.Exec(`UPDATE meta SET value = 1700000500 WHERE key = 'last_alive'`); err != nil {
		t.Fatal(err)
	}
	l.mu.Lock()
	l.closed = true
	close(l.queue)
	l.mu.Unlock()
	<-l.done
	l.db.Close()

	l2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer l2.Close()
	rows, _, _ := l2.Connections(bg, ConnQuery{})
	if len(rows) != 1 || rows[0].ID != conn || rows[0].Disconnected == 0 {
		t.Fatalf("connection after restart = %+v", rows)
	}
	games, _, _ := l2.Games(bg, GameQuery{})
	if len(games) != 1 || games[0].Ended == 0 || games[0].EndReason != "server restarted" {
		t.Fatalf("game after restart = %+v", games)
	}
	// new rows keep counting past the old ones
	if next := l2.ConnectionOpened(Conn{IP: "203.0.113.5", Username: "Ace"}); next != conn+1 {
		t.Fatalf("next id = %d after %d", next, conn)
	}
}

func TestCloseEndsOpenRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "modlog.db")
	l, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	l.ConnectionOpened(Conn{IP: "203.0.113.5", Username: "Ace"})
	l.GameStarted(Game{MatchID: 3, RoomID: 4, Players: []Player{{UserID: 1, Name: "Ace", Slot: 1}}})
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	l.Chat(ChatMessage{Name: "late", Text: "after close"}) // ignored, no panic
	l.Audit(AuditEntry{Admin: "a", Action: "kick"})        // likewise
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var open int
	db.QueryRow(`SELECT (SELECT COUNT(*) FROM connections WHERE disconnected = 0) +
		(SELECT COUNT(*) FROM games WHERE ended = 0)`).Scan(&open)
	if open != 0 {
		t.Fatalf("%d rows still open after Close", open)
	}
	var reason string
	db.QueryRow(`SELECT end_reason FROM games`).Scan(&reason)
	if reason != "server stopped" {
		t.Fatalf("reason = %q", reason)
	}
}

func TestPrune(t *testing.T) {
	l, clock, _ := openTest(t)
	old := l.ConnectionOpened(Conn{IP: "203.0.113.5", Username: "Old"})
	l.ConnectionClosed(old)
	openOld := l.ConnectionOpened(Conn{IP: "203.0.113.6", Username: "StillHere"})
	l.Chat(ChatMessage{Name: "Old", Text: "ancient"})
	l.GameStarted(Game{MatchID: 1, RoomID: 1, Players: []Player{{UserID: 1, Name: "Old", Slot: 1}}})
	l.GameEnded(1, EndFinished)
	l.Event(Event{Type: EventRoomCreated, RoomID: 1, Text: "old room"})
	l.Audit(AuditEntry{Admin: "Mod", Action: "ban", Detail: "kept forever"})
	l.Flush()

	clock.advance(100 * 24 * time.Hour)
	l.ConnectionOpened(Conn{IP: "203.0.113.5", Username: "New"})
	l.Chat(ChatMessage{Name: "New", Text: "fresh"})
	l.Flush()

	l.SetRetention(func() Retention { return Retention{ChatDays: 90, ConnectionDays: 90, GameDays: 0} })
	l.Prune()
	if rows, _, _ := l.ChatLog(bg, ChatQuery{}); len(rows) != 1 || rows[0].Text != "fresh" {
		t.Fatalf("chat after prune = %+v", rows)
	}
	rows, _, _ := l.Connections(bg, ConnQuery{})
	if len(rows) != 2 {
		t.Fatalf("connections after prune = %+v", rows)
	}
	for _, r := range rows {
		if r.ID == old {
			t.Fatal("old closed connection kept")
		}
	}
	if _, ok, _ := l.TraceName(bg, "Old"); ok {
		t.Fatal("pruned connection's names kept")
	}
	if _, ok, _ := l.TraceName(bg, "StillHere"); !ok {
		t.Fatalf("an open connection (%d) was pruned", openOld)
	}
	// games are kept forever with 0
	if games, _, _ := l.Games(bg, GameQuery{}); len(games) != 1 {
		t.Fatal("game pruned with gameLogDays 0")
	}
	l.SetRetention(func() Retention { return Retention{GameDays: 30} })
	l.Prune()
	if games, _, _ := l.Games(bg, GameQuery{}); len(games) != 0 {
		t.Fatal("old game kept")
	}
	var players int
	l.db.QueryRow(`SELECT COUNT(*) FROM game_players`).Scan(&players)
	if players != 0 {
		t.Fatalf("%d player rows outlived their game", players)
	}
	if audit, _, _ := l.AuditLog(bg, 0, 0); len(audit) != 1 {
		t.Fatalf("audit after prune = %+v", audit)
	}
}

func TestAuditPaging(t *testing.T) {
	l, clock, _ := openTest(t)
	for i := 0; i < 5; i++ {
		clock.advance(time.Second)
		l.Audit(AuditEntry{Admin: "Mod", Action: "kick", Detail: "Kicked " + string(rune('A'+i)), Source: SourceSite, IP: "127.0.0.1"})
	}
	page, more, err := l.AuditLog(bg, 0, 3)
	if err != nil || len(page) != 3 || !more || page[0].Detail != "Kicked E" || page[0].Source != SourceSite ||
		page[0].IP != "127.0.0.1" || page[0].Time != 1_700_000_005 {
		t.Fatalf("audit page = %+v %v %v", page, more, err)
	}
	rest, more, _ := l.AuditLog(bg, page[2].ID, 3)
	if len(rest) != 2 || more || rest[1].Detail != "Kicked A" {
		t.Fatalf("older audit = %+v %v", rest, more)
	}
}

func TestQueueOverflowDrops(t *testing.T) {
	old := queueSize
	queueSize = 4
	defer func() { queueSize = old }()
	l, _, _ := openTest(t)
	// hold the writer up with a long transaction so the queue fills
	block := make(chan struct{})
	l.enqueue(func(tx *sql.Tx) error { <-block; return nil })
	time.Sleep(50 * time.Millisecond)
	for i := 0; i < 20; i++ {
		l.Chat(ChatMessage{Name: "spam", Text: "x"})
	}
	close(block)
	l.Flush()
	if l.Dropped() == 0 {
		t.Fatal("nothing dropped from a full queue")
	}
	rows, _, _ := l.ChatLog(bg, ChatQuery{})
	if int64(len(rows))+l.Dropped() != 20 {
		t.Fatalf("%d written + %d dropped != 20", len(rows), l.Dropped())
	}
}
