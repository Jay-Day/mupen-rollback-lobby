package ws

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jay-day/rmgk-lobby/internal/config"
	"github.com/jay-day/rmgk-lobby/internal/modlog"
	"github.com/jay-day/rmgk-lobby/internal/protocol"
	"github.com/jay-day/rmgk-lobby/internal/ratelimit"
	"github.com/jay-day/rmgk-lobby/internal/state"
)

// listHub is a running hub (so trailing lists go out) with one listening
// client, its room list sent at most every interval.
func listHub(t *testing.T, interval time.Duration) (*Hub, *Client, context.CancelFunc) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "lobby.json")
	cfg := fmt.Sprintf(`{"roomListMinIntervalMs": %d}`, interval/time.Millisecond)
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	settings, err := config.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := settings.Get().RoomListMinInterval(); got != interval {
		t.Fatalf("interval = %v, want %v", got, interval)
	}
	h := NewHub(state.New(), "", protocol.ICEConfiguration{}, nil, settings)
	h.broadcasts.dir = t.TempDir()
	c := &Client{user: &state.User{ID: 900, Username: "listener", State: state.UserStateIdle}, send: make(chan []byte, 256)}
	h.clients[c.user.ID] = c
	ctx, cancel := context.WithCancel(context.Background())
	go h.Run(ctx)
	t.Cleanup(cancel)
	return h, c, cancel
}

type received struct {
	at  time.Time
	env protocol.Envelope
}

// listen returns what c receives within d.
func listen(t *testing.T, c *Client, d time.Duration) []received {
	t.Helper()
	var out []received
	deadline := time.After(d)
	for {
		select {
		case raw := <-c.send:
			var env protocol.Envelope
			if err := json.Unmarshal(raw, &env); err != nil {
				t.Fatal(err)
			}
			out = append(out, received{time.Now(), env})
		case <-deadline:
			return out
		}
	}
}

func roomLists(rs []received) []received {
	var out []received
	for _, r := range rs {
		if r.env.Type == protocol.TypeRoomList {
			out = append(out, r)
		}
	}
	return out
}

func addRoom(h *Hub, id uint64) {
	h.state.AddRoom(&state.Room{ID: id, Name: "room", MaxPlayers: 2, State: state.RoomStateWaiting, CreatedAt: time.Now()})
}

func TestRoomListFirstChangeGoesOutAtOnce(t *testing.T) {
	h, c, _ := listHub(t, 300*time.Millisecond)
	start := time.Now()
	h.broadcastRoomList()
	lists := roomLists(listen(t, c, 100*time.Millisecond))
	if len(lists) != 1 || lists[0].at.Sub(start) > 50*time.Millisecond {
		t.Fatalf("first list: %d lists, %v after the change", len(lists), lists)
	}
}

func TestRoomListChangesInsideTheIntervalGoOutOnceAtItsEnd(t *testing.T) {
	const interval = 300 * time.Millisecond
	h, c, _ := listHub(t, interval)
	start := time.Now()
	h.broadcastRoomList() // at once
	for i := uint64(1); i <= 5; i++ {
		addRoom(h, i)
		h.broadcastRoomList() // held back
	}
	got := roomLists(listen(t, c, interval+400*time.Millisecond))
	if len(got) != 2 {
		t.Fatalf("%d lists, want the first and one trailing", len(got))
	}
	if wait := got[1].at.Sub(start); wait < interval-20*time.Millisecond || wait > interval+200*time.Millisecond {
		t.Fatalf("trailing list after %v, want about %v", wait, interval)
	}
	var payload protocol.RoomListPayload
	json.Unmarshal(got[1].env.Data, &payload)
	if len(payload.Rooms) != 5 {
		t.Fatalf("trailing list has %d rooms, want the latest 5", len(payload.Rooms))
	}
	// once the interval is over, the next change goes out at once again
	time.Sleep(interval)
	h.broadcastRoomList()
	if len(roomLists(listen(t, c, 80*time.Millisecond))) != 1 {
		t.Fatal("a change after a quiet interval waited")
	}
}

func TestRoomListCoalescingOff(t *testing.T) {
	h, c, _ := listHub(t, 0)
	for i := 0; i < 5; i++ {
		h.broadcastRoomList()
	}
	if n := len(roomLists(listen(t, c, 100*time.Millisecond))); n != 5 {
		t.Fatalf("%d lists with coalescing off, want 5", n)
	}
}

func TestRoomListTrailingSendEndsWithTheHub(t *testing.T) {
	h, c, cancel := listHub(t, 200*time.Millisecond)
	h.broadcastRoomList()
	h.broadcastRoomList() // held back
	time.Sleep(20 * time.Millisecond)
	cancel()
	if n := len(roomLists(listen(t, c, 400*time.Millisecond))); n != 1 {
		t.Fatalf("%d lists after the hub stopped, want only the first", n)
	}
}

func TestRoomListAndViewerCountsStayInOrder(t *testing.T) {
	h, c, _ := listHub(t, 300*time.Millisecond)
	addRoom(h, 1)
	const matchID = 41
	if !h.broadcasts.begin(matchID, 1, 77, []uint64{77}, "", false) {
		t.Fatal("begin")
	}
	t.Cleanup(func() { h.broadcasts.endByMatch(matchID) })
	h.broadcastRoomList() // count 0, at once
	h.broadcastRoomList() // held back
	viewer := &Client{user: &state.User{ID: 901, Username: "viewer", State: state.UserStateIdle}, send: make(chan []byte, 256)}
	h.mu.Lock()
	h.clients[viewer.user.ID] = viewer
	h.mu.Unlock()
	h.broadcasts.spectateStart(viewer, matchID) // count 1, at once

	// Every list and count, in the order the listener got them: no list may
	// show an older count than a count message before it.
	shown := -1
	lists := 0
	for _, r := range listen(t, c, 700*time.Millisecond) {
		switch r.env.Type {
		case protocol.TypeBroadcastViewerCount:
			var p protocol.BroadcastViewerCountPayload
			json.Unmarshal(r.env.Data, &p)
			shown = p.ViewerCount
		case protocol.TypeRoomList:
			lists++
			var p protocol.RoomListPayload
			json.Unmarshal(r.env.Data, &p)
			if len(p.Rooms) != 1 || p.Rooms[0].ViewerCount < shown {
				t.Fatalf("list %d shows %+v after a count of %d", lists, p.Rooms, shown)
			}
			shown = p.Rooms[0].ViewerCount
		}
	}
	if lists != 2 || shown != 1 {
		t.Fatalf("%d lists, last count %d; want 2 lists ending at 1", lists, shown)
	}
}

func TestViewerCountIsNotRepeated(t *testing.T) {
	h, c, _ := listHub(t, 0)
	addRoom(h, 1)
	const matchID = 42
	if !h.broadcasts.begin(matchID, 1, 77, []uint64{77}, "", false) {
		t.Fatal("begin")
	}
	t.Cleanup(func() { h.broadcasts.endByMatch(matchID) })
	h.broadcasts.notifyViewerCount(matchID) // 0, the first
	viewer := &Client{user: &state.User{ID: 902, Username: "viewer", State: state.UserStateIdle}, send: make(chan []byte, 256)}
	h.broadcasts.spectateStart(viewer, matchID) // 1
	h.broadcasts.spectateStart(viewer, matchID) // re-subscribes: still 1
	h.broadcasts.notifyViewerCount(matchID)     // still 1
	var counts []int
	for _, r := range listen(t, c, 200*time.Millisecond) {
		if r.env.Type == protocol.TypeBroadcastViewerCount {
			var p protocol.BroadcastViewerCountPayload
			json.Unmarshal(r.env.Data, &p)
			counts = append(counts, p.ViewerCount)
		}
	}
	if len(counts) != 2 || counts[0] != 0 || counts[1] != 1 {
		t.Fatalf("counts sent to the lobby = %v, want [0 1]", counts)
	}
}

// logCapture collects the default logger's lines while a test runs.
type logCapture struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *logCapture) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *logCapture) lines(msg string) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
	for _, line := range strings.Split(l.buf.String(), "\n") {
		if strings.Contains(line, "msg=\""+msg+"\"") || strings.Contains(line, "msg="+msg+" ") {
			out = append(out, line)
		}
	}
	return out
}

func captureLogs(t *testing.T) *logCapture {
	t.Helper()
	l := &logCapture{}
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(l, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(old) })
	return l
}

func TestBroadcastLogsOneLinePerBroadcast(t *testing.T) {
	logs := captureLogs(t)
	h := &Hub{clients: map[uint64]*Client{}}
	for i := uint64(1); i <= 5; i++ {
		h.clients[i] = &Client{user: &state.User{ID: i}, send: make(chan []byte)} // always full
	}
	h.broadcastToAll(protocol.TypeChatMsg, protocol.ChatMsgPayload{Message: "hi"}, 0)
	drops := logs.lines("broadcast drop (queue full)")
	if len(drops) != 1 || !strings.Contains(drops[0], "dropped=5") || !strings.Contains(drops[0], "level=WARN") {
		t.Fatalf("drop lines = %q", drops)
	}
	sent := logs.lines("broadcast")
	if len(sent) != 1 || !strings.Contains(sent[0], "level=DEBUG") {
		t.Fatalf("broadcast lines = %q, want one at Debug", sent)
	}
}

func TestPunchFailedLogIsThrottled(t *testing.T) {
	logs := captureLogs(t)
	h := NewHub(state.New(), "", protocol.ICEConfiguration{}, nil, nil)
	c := &Client{ip: "192.0.2.201", user: &state.User{ID: 1, Username: "p"}, send: make(chan []byte, 8)}
	for i := 0; i < 100; i++ {
		h.dispatch(c, protocol.Envelope{Type: protocol.TypeMatchPunchFailed, Data: json.RawMessage(`{"matchId":1}`)})
	}
	if n := len(logs.lines("match punch failed")); n != 1 {
		t.Fatalf("%d log lines for 100 reports, want 1", n)
	}
}

func TestLeaveWithoutARoomLogsSparingly(t *testing.T) {
	logs := captureLogs(t)
	h := NewHub(state.New(), "", protocol.ICEConfiguration{}, nil, nil)
	inbox := make(chan []byte, 128)
	c := &Client{ip: "192.0.2.202", user: &state.User{ID: 1, Username: "p", Send: inbox}, send: inbox}
	for i := 0; i < 50; i++ {
		h.dispatch(c, protocol.Envelope{Type: protocol.TypeRoomLeave})
	}
	if n := len(logs.lines("ROOM_LEAVE with no current room — sending fallback ROOM_LEFT")); n != 1 {
		t.Fatalf("%d fallback lines for 50 leaves, want 1", n)
	}
	for _, line := range logs.lines("sendRoomLeft delivered") {
		if !strings.Contains(line, "level=DEBUG") {
			t.Fatalf("delivery logged above Debug: %s", line)
		}
	}
	if len(inbox) != 50 {
		t.Fatalf("%d ROOM_LEFT replies, want 50 (leaving is never limited)", len(inbox))
	}
}

func TestRateLimitReportSurvivesARestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ratelimits.json")
	// Each hub opens the file afresh, as a restarted server does.
	open := func() *modlog.Logger {
		l, err := modlog.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		return l
	}
	limits := ratelimit.DefaultActionLimits()
	newHub := func() *Hub {
		h := NewHub(state.New(), "", protocol.ICEConfiguration{}, nil, nil)
		h.SetModLog(open())
		l := ratelimit.NewActionLimiter()
		l.SetLimits(func() ratelimit.ActionLimits { return limits })
		h.SetActionLimiter(l)
		return h
	}
	h1 := newHub()
	for i := 0; i < 7; i++ { // room_create allows 5 a minute
		h1.actions.Allow("203.0.113.9", ratelimit.ActionRoomCreate, "Ace")
	}
	h1.SaveRateLimits()

	h2 := newHub()
	if n := h2.LoadRateLimits(); n != 1 {
		t.Fatalf("loaded %d records, want 1", n)
	}
	rec := h2.RateLimitReport().Records[0]
	if rec.IP != "203.0.113.9" || rec.TotalRefused != 2 || rec.Refused[ratelimit.ActionRoomCreate] != 2 || rec.Usernames[0] != "Ace" {
		t.Fatalf("restored record = %+v", rec)
	}
	// unchanged since loading: nothing to write
	h2.SaveRateLimits()
	if !h2.ForgetRateLimit("203.0.113.9") || h2.ForgetRateLimit("203.0.113.9") {
		t.Fatal("forget")
	}
	h2.SaveRateLimits()
	if saved, err := open().RateLimits(); err != nil || len(saved) != 0 {
		t.Fatalf("after forgetting, the file holds %v (%v)", saved, err)
	}
}
