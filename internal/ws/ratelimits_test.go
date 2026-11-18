package ws

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jay-day/rmgk-lobby/internal/moderation"
	"github.com/jay-day/rmgk-lobby/internal/protocol"
	"github.com/jay-day/rmgk-lobby/internal/ratelimit"
	"github.com/jay-day/rmgk-lobby/internal/state"
)

// The players in these tests come from documentation-range addresses, which
// the limiter limits (it leaves loopback alone).
const (
	bystanderIP = "198.51.100.1"
	playerIP    = "203.0.113.66"
	otherIP     = "192.0.2.44"
)

// setConfig writes lobby.json and applies it.
func (f *modFixture) setConfig(data string) {
	f.t.Helper()
	if err := os.WriteFile(f.cfgPath, []byte(data), 0o600); err != nil {
		f.t.Fatal(err)
	}
	f.settings.Reload()
}

// newLimitFixture is a lobby with the request limiter set to limits, and
// room lists sent on every change (so what a refusal sends is easy to see).
func newLimitFixture(t *testing.T, limits ratelimit.ActionLimits) (*modFixture, *ratelimit.ActionLimiter) {
	t.Helper()
	l := ratelimit.NewActionLimiter()
	l.SetLimits(func() ratelimit.ActionLimits { return limits })
	f := newModFixture(t, func(h *Hub) { h.SetActionLimiter(l) })
	f.setConfig(`{"roomListMinIntervalMs": 0}`)
	return f, l
}

// only limits just these kinds, with no automatic ban.
func only(rates map[ratelimit.ActionKind]ratelimit.Rate) ratelimit.ActionLimits {
	l := ratelimit.DefaultActionLimits()
	l.Rates = rates
	l.BanStrikes = 0
	return l
}

func once(kinds ...ratelimit.ActionKind) map[ratelimit.ActionKind]ratelimit.Rate {
	m := map[ratelimit.ActionKind]ratelimit.Rate{}
	for _, k := range kinds {
		m[k] = ratelimit.Rate{Count: 1, Window: time.Hour}
	}
	return m
}

var allKinds = []ratelimit.ActionKind{ratelimit.ActionRoomCreate, ratelimit.ActionMatchStart, ratelimit.ActionRoomJoin,
	ratelimit.ActionSpectate, ratelimit.ActionRoomUpdate, ratelimit.ActionLobbyUpdate}

// collect returns everything c received before the reply to a heartbeat:
// everything the hub sent it as a result of what anyone did before.
func (c *wsConn) collect() []protocol.Envelope {
	c.t.Helper()
	c.send(protocol.TypeHeartbeat, protocol.HeartbeatRequest{})
	ctx, cancel := context.WithTimeout(bg, 5*time.Second)
	defer cancel()
	var out []protocol.Envelope
	for {
		_, raw, err := c.conn.Read(ctx)
		if err != nil {
			c.t.Fatalf("connection closed before HEARTBEAT_ACK: %v", err)
		}
		var env protocol.Envelope
		if json.Unmarshal(raw, &env) != nil {
			continue
		}
		if env.Type == protocol.TypeHeartbeatAck {
			return out
		}
		out = append(out, env)
	}
}

func types(envs []protocol.Envelope) []string {
	var out []string
	for _, e := range envs {
		out = append(out, e.Type)
	}
	return out
}

func hasType(envs []protocol.Envelope, msgType string) bool {
	_, ok := findEnvelope(envs, msgType)
	return ok
}

// quiet fails when anything reached c.
func quiet(t *testing.T, c *wsConn, what string) {
	t.Helper()
	if envs := c.collect(); len(envs) != 0 {
		t.Fatalf("%s: other players got %v", what, types(envs))
	}
}

func mustAwait(t *testing.T, c *wsConn, msgType string) protocol.Envelope {
	t.Helper()
	env, ok := c.await(msgType)
	if !ok {
		t.Fatalf("no %s", msgType)
	}
	return env
}

func decode[T any](t *testing.T, env protocol.Envelope) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(env.Data, &v); err != nil {
		t.Fatalf("decode %s: %v", env.Type, err)
	}
	return v
}

func checkWait(t *testing.T, text, prefix string) {
	t.Helper()
	if !strings.HasPrefix(text, prefix+" Try again in ") || !strings.HasSuffix(text, " seconds.") {
		t.Fatalf("refusal %q, want %q + a wait in seconds", text, prefix)
	}
}

var testRoom = protocol.RoomCreateRequest{Name: "Remix night", Rom: protocol.RomInfo{Name: "Smash Remix", MD5: "abc"}, MaxPlayers: 4}

// startAlone creates a room with late join on and starts its match alone,
// returning the match id.
func startAlone(t *testing.T, c *wsConn) uint64 {
	t.Helper()
	c.send(protocol.TypeRoomCreate, testRoom)
	mustAwait(t, c, protocol.TypeRoomCreated)
	c.send(protocol.TypeRoomUpdateLateJoin, protocol.RoomUpdateLateJoinRequest{Enabled: true})
	c.sync()
	c.send(protocol.TypeRoomStart, nil)
	return decode[protocol.MatchBeginPayload](t, mustAwait(t, c, protocol.TypeMatchBegin)).MatchID
}

func TestRoomCreateRefusedBeforeAnyBroadcast(t *testing.T) {
	f, _ := newLimitFixture(t, only(once(ratelimit.ActionRoomCreate)))
	by, _ := f.join("Bystander", bystanderIP)
	a, _ := f.join("Spammer", playerIP)
	by.collect()

	a.send(protocol.TypeRoomCreate, testRoom)
	mustAwait(t, a, protocol.TypeRoomCreated)
	a.sync() // the creator hears first; the lobby right after
	if envs := by.collect(); !hasType(envs, protocol.TypeRoomList) || !hasType(envs, protocol.TypePresenceDelta) {
		t.Fatalf("an allowed create reached the lobby as %v", types(envs))
	}
	a.send(protocol.TypeRoomLeave, nil)
	mustAwait(t, a, protocol.TypeRoomLeft)
	by.collect()

	a.send(protocol.TypeRoomCreate, testRoom)
	fail := decode[protocol.RoomJoinFailPayload](t, mustAwait(t, a, protocol.TypeRoomCreateFail))
	checkWait(t, fail.Reason, "You're creating rooms too fast.")
	a.sync()
	quiet(t, by, "refused create")
	if n := len(f.hub.state.ListRooms()); n != 0 {
		t.Fatalf("%d rooms after a refused create", n)
	}
	// another IP has its own budget
	o, _ := f.join("Other", otherIP)
	o.send(protocol.TypeRoomCreate, testRoom)
	mustAwait(t, o, protocol.TypeRoomCreated)
}

func TestRoomJoinRefusedBeforeAnyBroadcast(t *testing.T) {
	f, _ := newLimitFixture(t, only(once(ratelimit.ActionRoomJoin)))
	host, _ := f.join("Host", bystanderIP)
	host.send(protocol.TypeRoomCreate, testRoom)
	room := decode[protocol.RoomCreatedResponse](t, mustAwait(t, host, protocol.TypeRoomCreated)).RoomID
	j, _ := f.join("Joiner", playerIP)
	host.collect()

	j.send(protocol.TypeRoomJoin, protocol.RoomJoinRequest{RoomID: room})
	mustAwait(t, j, protocol.TypeRoomJoinOK)
	j.send(protocol.TypeRoomLeave, nil)
	mustAwait(t, j, protocol.TypeRoomLeft)
	host.collect()

	j.send(protocol.TypeRoomJoin, protocol.RoomJoinRequest{RoomID: room})
	fail := decode[protocol.RoomJoinFailPayload](t, mustAwait(t, j, protocol.TypeRoomJoinFail))
	checkWait(t, fail.Reason, "You're joining rooms too fast.")
	j.sync()
	quiet(t, host, "refused join")
}

func TestRoomStartRefusedWithANotice(t *testing.T) {
	f, _ := newLimitFixture(t, only(once(ratelimit.ActionMatchStart)))
	by, _ := f.join("Bystander", bystanderIP)
	a, _ := f.join("Host", playerIP)
	matchID := startAlone(t, a)
	a.send(protocol.TypeMatchFinished, map[string]uint64{"matchId": matchID})
	a.sync()
	by.collect()

	a.send(protocol.TypeRoomStart, nil)
	envs := a.collect()
	if hasType(envs, protocol.TypeMatchBegin) {
		t.Fatal("a refused start began a match")
	}
	notice, ok := findEnvelope(envs, protocol.TypeModNotice)
	if !ok {
		t.Fatalf("no notice for a refused start: %v", types(envs))
	}
	checkWait(t, decode[protocol.ModNoticePayload](t, notice).Text, "You're starting games too fast.")
	quiet(t, by, "refused start")
	if room := f.hub.state.ListRooms()[0]; room.State != state.RoomStateWaiting {
		t.Fatalf("room state %s after a refused start", room.State)
	}
}

func TestQuickMatchJoinRefusedButCancelWorks(t *testing.T) {
	f, _ := newLimitFixture(t, only(once(ratelimit.ActionRoomJoin)))
	by, _ := f.join("Bystander", bystanderIP)
	a, _ := f.join("Searcher", playerIP)
	by.collect()
	qm := protocol.QuickMatchJoinRequest{Rom: protocol.RomInfo{Name: "Smash Remix", MD5: "abc"}}

	a.send(protocol.TypeQuickMatchJoin, qm)
	if st := decode[protocol.QuickMatchStatusPayload](t, mustAwait(t, a, protocol.TypeQuickMatchStatus)); !st.Searching {
		t.Fatal("not searching")
	}
	a.send(protocol.TypeQuickMatchCancel, nil) // never limited
	if st := decode[protocol.QuickMatchStatusPayload](t, mustAwait(t, a, protocol.TypeQuickMatchStatus)); st.Searching {
		t.Fatal("cancel didn't stop the search")
	}
	by.collect()

	a.send(protocol.TypeQuickMatchJoin, qm)
	envs := a.collect()
	st, ok := findEnvelope(envs, protocol.TypeQuickMatchStatus)
	if !ok || decode[protocol.QuickMatchStatusPayload](t, st).Searching || !hasType(envs, protocol.TypeModNotice) {
		t.Fatalf("refused search answered with %v", types(envs))
	}
	quiet(t, by, "refused quick match search")
	if f.hub.matchmaker.contains(f.hub.findClientByUsername("Searcher").user.ID) {
		t.Fatal("a refused search is queued")
	}
}

func TestSpectateStartRefused(t *testing.T) {
	f, _ := newLimitFixture(t, only(once(ratelimit.ActionSpectate)))
	host, _ := f.join("Host", bystanderIP)
	matchID := startAlone(t, host)
	host.send(protocol.TypeBroadcastBegin, protocol.BroadcastBeginRequest{MatchID: matchID})
	host.sync()
	v, _ := f.join("Viewer", playerIP)
	by, _ := f.join("Bystander", otherIP)

	v.send(protocol.TypeSpectateStart, protocol.SpectateStartRequest{MatchID: matchID})
	mustAwait(t, v, protocol.TypeSpectateBegin)
	v.send(protocol.TypeSpectateStop, protocol.SpectateStopRequest{MatchID: matchID}) // never limited
	v.sync()
	by.collect()

	v.send(protocol.TypeSpectateStart, protocol.SpectateStartRequest{MatchID: matchID})
	fail := decode[protocol.SpectateFailPayload](t, mustAwait(t, v, protocol.TypeSpectateFail))
	if fail.MatchID != matchID {
		t.Fatalf("SPECTATE_FAIL names match %d, want %d (clients ignore any other)", fail.MatchID, matchID)
	}
	checkWait(t, fail.Reason, "You're opening live replays too fast.")
	v.sync()
	quiet(t, by, "refused spectate")
	if _, _, viewers := f.hub.broadcasts.roomBroadcast(f.hub.state.ListRooms()[0].ID); viewers != 0 {
		t.Fatalf("%d viewers after a refused start", viewers)
	}
}

func TestRoomUpdateRefusedRestoresTheSettings(t *testing.T) {
	f, _ := newLimitFixture(t, only(once(ratelimit.ActionRoomUpdate)))
	host, _ := f.join("Host", playerIP)
	host.send(protocol.TypeRoomCreate, testRoom)
	room := decode[protocol.RoomCreatedResponse](t, mustAwait(t, host, protocol.TypeRoomCreated)).RoomID
	member, _ := f.join("Member", bystanderIP)
	member.send(protocol.TypeRoomJoin, protocol.RoomJoinRequest{RoomID: room})
	mustAwait(t, member, protocol.TypeRoomJoinOK)
	member.collect()
	host.collect()

	host.send(protocol.TypeRoomUpdateLiveReplay, protocol.RoomUpdateLiveReplayRequest{Enabled: true})
	if st := decode[protocol.RoomStatePayload](t, mustAwait(t, member, protocol.TypeRoomState)); !st.LiveReplayEnabled {
		t.Fatal("allowed change didn't reach the room")
	}
	host.collect()
	member.collect()

	host.send(protocol.TypeRoomUpdateLiveReplay, protocol.RoomUpdateLiveReplayRequest{Enabled: false})
	envs := host.collect()
	st, ok := findEnvelope(envs, protocol.TypeRoomState)
	if !ok || !decode[protocol.RoomStatePayload](t, st).LiveReplayEnabled || !hasType(envs, protocol.TypeModNotice) {
		t.Fatalf("refused change answered with %v, want the room as it stands and a notice", types(envs))
	}
	quiet(t, member, "refused room update")
	// a second refusal right away isn't answered again
	host.send(protocol.TypeRoomUpdateLiveReplay, protocol.RoomUpdateLiveReplayRequest{Enabled: false})
	if envs := host.collect(); len(envs) != 0 {
		t.Fatalf("second refusal answered with %v", types(envs))
	}
}

func TestAwayFlipRefusedKeepsTheHeartbeat(t *testing.T) {
	f, _ := newLimitFixture(t, only(once(ratelimit.ActionLobbyUpdate)))
	by, _ := f.join("Bystander", bystanderIP)
	a, _ := f.join("Idler", playerIP)
	by.collect()

	a.send(protocol.TypeHeartbeat, protocol.HeartbeatRequest{Away: true})
	mustAwait(t, a, protocol.TypeHeartbeatAck)
	if !hasType(by.collect(), protocol.TypePresenceDelta) {
		t.Fatal("allowed away flip didn't reach the lobby")
	}
	a.send(protocol.TypeHeartbeat, protocol.HeartbeatRequest{Away: false})
	mustAwait(t, a, protocol.TypeHeartbeatAck) // still answered
	quiet(t, by, "refused away flip")
	if st := f.hub.findClientByUsername("Idler").user.GetState(); st != state.UserStateAway {
		t.Fatalf("state %s after a refused flip, want away", st)
	}
}

func TestMatchAdmitRefused(t *testing.T) {
	f, _ := newLimitFixture(t, only(map[ratelimit.ActionKind]ratelimit.Rate{
		ratelimit.ActionMatchStart: {Count: 2, Window: time.Hour}}))
	host, _ := f.join("Host", playerIP)
	matchID := startAlone(t, host) // 1 of 2
	joiner, hello := f.join("Joiner", bystanderIP)
	room := f.hub.state.ListRooms()[0].ID
	joiner.send(protocol.TypeRoomJoin, protocol.RoomJoinRequest{RoomID: room})
	mustAwait(t, joiner, protocol.TypeRoomJoinOK)
	host.collect()

	host.send(protocol.TypeMatchAdmit, protocol.MatchAdmitRequest{MatchID: matchID, UserID: hello.UserID}) // 2 of 2
	begin := decode[protocol.MatchAdmitBeginPayload](t, mustAwait(t, host, protocol.TypeMatchAdmitBegin))
	host.send(protocol.TypeMatchAdmitSignal, protocol.MatchAdmitSignalPayload{MatchID: matchID, AdmitID: begin.AdmitID,
		Step: protocol.AdmitStepAbort, Reason: "test"}) // never limited
	host.sync()
	joiner.collect()

	host.send(protocol.TypeMatchAdmit, protocol.MatchAdmitRequest{MatchID: matchID, UserID: hello.UserID})
	fail := decode[protocol.MatchAdmitFailPayload](t, mustAwait(t, host, protocol.TypeMatchAdmitFail))
	if !strings.HasPrefix(fail.Reason, "you're adding players too fast; try again in ") || fail.UserID != hello.UserID {
		t.Fatalf("admit refusal = %+v", fail)
	}
	host.sync()
	quiet(t, joiner, "refused admit")
}

func TestLeavingDroppingAndStoppingAreNeverLimited(t *testing.T) {
	f, _ := newLimitFixture(t, only(once(allKinds...)))
	by, _ := f.join("Bystander", bystanderIP)
	a, _ := f.join("Host", playerIP)
	// the same IP, so the same budgets
	v, _ := f.join("Viewer", playerIP)
	// spends room_create, room_update and match_start
	matchID := startAlone(t, a)
	a.send(protocol.TypeBroadcastBegin, protocol.BroadcastBeginRequest{MatchID: matchID})
	a.sync()
	// spends spectate, then is refused one
	v.send(protocol.TypeSpectateStart, protocol.SpectateStartRequest{MatchID: matchID})
	mustAwait(t, v, protocol.TypeSpectateBegin)
	v.send(protocol.TypeSpectateStart, protocol.SpectateStartRequest{MatchID: 999999})
	mustAwait(t, v, protocol.TypeSpectateFail)
	// spends room_join
	v.send(protocol.TypeQuickMatchJoin, protocol.QuickMatchJoinRequest{Rom: protocol.RomInfo{MD5: "abc"}})
	mustAwait(t, v, protocol.TypeQuickMatchStatus)
	by.collect()

	// every kind is spent; none of these may be refused
	v.send(protocol.TypeQuickMatchCancel, nil)
	if st := decode[protocol.QuickMatchStatusPayload](t, mustAwait(t, v, protocol.TypeQuickMatchStatus)); st.Searching {
		t.Fatal("cancel refused")
	}
	v.send(protocol.TypeSpectateStop, protocol.SpectateStopRequest{MatchID: matchID})
	v.sync()
	if _, _, viewers := f.hub.broadcasts.roomBroadcast(f.hub.state.ListRooms()[0].ID); viewers != 0 {
		t.Fatal("spectate stop refused")
	}
	a.send(protocol.TypeBroadcastEnd, protocol.BroadcastEndRequest{MatchID: matchID})
	a.sync()
	if _, live, _ := f.hub.broadcasts.roomBroadcast(f.hub.state.ListRooms()[0].ID); live {
		t.Fatal("broadcast end refused")
	}
	a.send(protocol.TypeMatchFinished, map[string]uint64{"matchId": matchID})
	if st := decode[protocol.RoomStatePayload](t, mustAwait(t, a, protocol.TypeRoomState)); st.State != string(state.RoomStateWaiting) {
		t.Fatalf("drop refused: room %s", st.State)
	}
	a.send(protocol.TypeRoomLeave, nil)
	mustAwait(t, a, protocol.TypeRoomLeft)
	if n := len(f.hub.state.ListRooms()); n != 0 {
		t.Fatal("leave refused")
	}
	if envs := by.collect(); !hasType(envs, protocol.TypeRoomList) {
		t.Fatalf("the lobby didn't hear about any of it: %v", types(envs))
	}
	// and disconnecting cleans up as ever
	v.conn.CloseNow()
	f.waitOnline(2)
}

func TestModeratorsAreNotLimited(t *testing.T) {
	f, _ := newLimitFixture(t, only(once(ratelimit.ActionRoomCreate)))
	m, _ := f.join("Moddy", playerIP)
	f.signInModerator(m)
	for i := 0; i < 3; i++ {
		m.send(protocol.TypeRoomCreate, testRoom)
		mustAwait(t, m, protocol.TypeRoomCreated)
		m.send(protocol.TypeRoomLeave, nil)
		mustAwait(t, m, protocol.TypeRoomLeft)
	}
	// the exemption is the connection's, not the IP's
	p, _ := f.join("Plain", playerIP)
	p.send(protocol.TypeRoomCreate, testRoom)
	mustAwait(t, p, protocol.TypeRoomCreated)
	p.send(protocol.TypeRoomLeave, nil)
	p.send(protocol.TypeRoomCreate, testRoom)
	mustAwait(t, p, protocol.TypeRoomCreateFail)
}

func TestAutomaticBanClosesTheIPAndIsAudited(t *testing.T) {
	limits := only(once(ratelimit.ActionRoomCreate))
	limits.BanStrikes, limits.BanWindow, limits.BanDuration = 3, time.Minute, time.Hour
	f, _ := newLimitFixture(t, limits)
	by, _ := f.join("Bystander", bystanderIP)
	a, _ := f.join("Spammer", playerIP)
	a2, _ := f.join("Spammer2", playerIP)
	a.send(protocol.TypeRoomCreate, testRoom)
	mustAwait(t, a, protocol.TypeRoomCreated)
	a.send(protocol.TypeRoomLeave, nil)
	mustAwait(t, a, protocol.TypeRoomLeft)

	for i := 0; i < 3; i++ { // the third refusal bans
		a.send(protocol.TypeRoomCreate, testRoom)
	}
	if text, ok := a.awaitNotice("too many requests"); !ok || !strings.Contains(text, "blocked until") {
		t.Fatalf("ban notice = %q", text)
	}
	a.awaitClosed()
	a2.awaitClosed() // every connection from the IP
	s, banned := f.mod.IsBanned(playerIP)
	if !banned || s.By != moderation.AutoBanBy || s.Name != "Spammer" || !strings.Contains(s.Reason, "Rate limit: 3 refused requests within 1 minute") ||
		s.Until.Before(time.Now().Add(59*time.Minute)) || s.Until.After(time.Now().Add(61*time.Minute)) {
		t.Fatalf("ban = %+v (banned %v)", s, banned)
	}
	if _, err := f.dial(playerIP); err == nil {
		t.Fatal("a banned IP connected")
	}
	if audit := f.logs.lines("moderator action"); !hasLine(audit, "admin="+moderation.AutoBanBy, "action=timeout",
		"source=auto", "ip="+playerIP) {
		t.Fatalf("audit = %q", audit)
	}
	rep := f.hub.RateLimitReport()
	if len(rep.Records) != 1 || rep.Records[0].AutoBans != 1 || rep.Records[0].TotalRefused != 3 {
		t.Fatalf("report = %+v", rep.Records)
	}
	by.sync() // the bystander is untouched

	// A moderator's early unban lets the next run of refusals ban again.
	if !f.hub.LiftSanction("Mod", "unban", playerIP) {
		t.Fatal("unban")
	}
	b, _ := f.join("Spammer3", playerIP)
	for i := 0; i < 3; i++ {
		b.send(protocol.TypeRoomCreate, testRoom)
	}
	b.awaitClosed()
	if _, banned := f.mod.IsBanned(playerIP); !banned || f.hub.RateLimitReport().Records[0].AutoBans != 2 {
		t.Fatal("not banned again after an unban")
	}
}

func TestOneLiveReplayPerMatch(t *testing.T) {
	f := newModFixture(t)
	f.setConfig(`{"roomListMinIntervalMs": 0}`)
	host, _ := f.join("Host", playerIP)
	matchID := startAlone(t, host)
	by, _ := f.join("Bystander", bystanderIP)

	host.send(protocol.TypeBroadcastBegin, protocol.BroadcastBeginRequest{MatchID: matchID})
	host.sync()
	if envs := by.collect(); !hasType(envs, protocol.TypeRoomList) || !hasType(envs, protocol.TypeBroadcastViewerCount) {
		t.Fatalf("first begin reached the lobby as %v", types(envs))
	}
	host.send(protocol.TypeBroadcastEnd, protocol.BroadcastEndRequest{MatchID: matchID})
	host.sync()
	by.collect()
	host.send(protocol.TypeBroadcastBegin, protocol.BroadcastBeginRequest{MatchID: matchID})
	host.sync()
	quiet(t, by, "second begin in one match")
	roomID := f.hub.state.ListRooms()[0].ID
	if _, live, _ := f.hub.broadcasts.roomBroadcast(roomID); live {
		t.Fatal("a second live replay started in the same match")
	}

	// the next match gets its own
	host.send(protocol.TypeMatchFinished, map[string]uint64{"matchId": matchID})
	host.sync()
	host.send(protocol.TypeRoomStart, nil)
	next := decode[protocol.MatchBeginPayload](t, mustAwait(t, host, protocol.TypeMatchBegin)).MatchID
	host.send(protocol.TypeBroadcastBegin, protocol.BroadcastBeginRequest{MatchID: next})
	host.sync()
	if _, live, _ := f.hub.broadcasts.roomBroadcast(roomID); !live {
		t.Fatal("the next match couldn't start its live replay")
	}
}

func TestRefusalWording(t *testing.T) {
	for _, c := range []struct {
		retry time.Duration
		want  string
	}{
		{0, "Done too fast. Try again in 1 second."},
		{300 * time.Millisecond, "Done too fast. Try again in 1 second."},
		{time.Second, "Done too fast. Try again in 1 second."},
		{1001 * time.Millisecond, "Done too fast. Try again in 2 seconds."},
		{59 * time.Second, "Done too fast. Try again in 59 seconds."},
	} {
		if got := tooFast("Done too fast.", c.retry); got != c.want {
			t.Errorf("tooFast(%v) = %q, want %q", c.retry, got, c.want)
		}
	}
	for d, want := range map[time.Duration]string{
		time.Hour: "1 hour", 10 * time.Minute: "10 minutes", 90 * time.Second: "90 seconds", 48 * time.Hour: "2 days",
	} {
		if got := plainDuration(d); got != want {
			t.Errorf("plainDuration(%v) = %q, want %q", d, got, want)
		}
	}
}
