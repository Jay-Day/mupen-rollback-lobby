package ws

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/jay-day/rmgk-lobby/internal/protocol"
	"github.com/jay-day/rmgk-lobby/internal/state"
)

// fastSessions shortens the session timers for a test.
func fastSessions(t *testing.T) {
	t.Helper()
	next, rematch, recheck := qmNextSetDelay, qmRematchAfter, qmRematchRecheck
	qmNextSetDelay, qmRematchAfter, qmRematchRecheck = 20*time.Millisecond, 60*time.Millisecond, 20*time.Millisecond
	t.Cleanup(func() { qmNextSetDelay, qmRematchAfter, qmRematchRecheck = next, rematch, recheck })
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// drainAll returns every queued message.
func drainAll(c *Client) []protocol.Envelope {
	var out []protocol.Envelope
	for {
		select {
		case raw := <-c.send:
			var env protocol.Envelope
			if json.Unmarshal(raw, &env) == nil {
				out = append(out, env)
			}
		default:
			return out
		}
	}
}

func ofType(msgs []protocol.Envelope, msgType string) []protocol.Envelope {
	var out []protocol.Envelope
	for _, m := range msgs {
		if m.Type == msgType {
			out = append(out, m)
		}
	}
	return out
}

func roomLeftReasons(msgs []protocol.Envelope) []string {
	var out []string
	for _, m := range ofType(msgs, protocol.TypeRoomLeft) {
		var p protocol.RoomLeftPayload
		json.Unmarshal(m.Data, &p)
		out = append(out, p.Reason)
	}
	return out
}

var sessionRequest = &protocol.RankedRoomRequest{Enabled: true}

func (f *rankedFixture) session(c *Client) {
	f.t.Helper()
	f.hub.handleQuickMatchJoin(c, envelope(f.t, protocol.QuickMatchJoinRequest{
		Rom: protocol.RomInfo{Name: "Remix", MD5: testRomMD5}, Ranked: sessionRequest, Continuous: true,
	}))
}

// startQuickMatch starts a paired room's set the way the warmup timer does.
func (f *rankedFixture) startQuickMatch(room *state.Room) uint64 {
	f.t.Helper()
	f.hub.matchmaker.markStarted(room.ID)
	f.hub.startMatchInRoom(room, "quick-match")
	match := room.Host.CurrentMatch
	if match == nil {
		f.t.Fatal("quick match didn't start")
	}
	return match.ID
}

// playSet reports a Bo3 that slot 1 wins.
func (f *rankedFixture) playSet(matchID uint64, players ...*Client) {
	f.t.Helper()
	for _, p := range players {
		f.hub.handleMatchResult(p, envelope(f.t, result(matchID, 1, []int{2, 0})))
	}
}

func TestRankedSessionSearchesAgainAvoidingOpponent(t *testing.T) {
	fastSessions(t)
	f := newRankedFixture(t)
	a, b := f.connect("alice", true), f.connect("bob", true)
	f.session(a)
	f.session(b)
	room := a.user.CurrentRoom
	if room == nil || b.user.CurrentRoom != room {
		t.Fatal("session searchers didn't pair")
	}
	matchID := f.startQuickMatch(room)
	drainAll(a)
	drainAll(b)
	f.playSet(matchID, a, b)

	// after the result, the room closes and both search again
	waitFor(t, "both searching again", func() bool {
		return f.hub.matchmaker.contains(a.user.ID) && f.hub.matchmaker.contains(b.user.ID)
	})
	if a.user.CurrentRoom != nil || b.user.CurrentRoom != nil || f.hub.state.GetRoom(room.ID) != nil {
		t.Fatal("the finished room is still open")
	}
	msgs := drainAll(a)
	if reasons := roomLeftReasons(msgs); len(reasons) != 1 || reasons[0] != "quick_match_next" {
		t.Fatalf("ROOM_LEFT reasons = %v", reasons)
	}
	var status protocol.QuickMatchStatusPayload
	statuses := ofType(msgs, protocol.TypeQuickMatchStatus)
	json.Unmarshal(statuses[len(statuses)-1].Data, &status)
	if !status.Searching || !status.Ranked || !status.Continuous {
		t.Fatalf("status after the set = %+v", status)
	}
	// they don't get each other again ...
	if a.user.CurrentRoom != nil {
		t.Fatal("rematched the previous opponent straight away")
	}
	// ... but a new player gets one of them
	c := f.connect("carol", true)
	f.session(c)
	if c.user.CurrentRoom == nil {
		t.Fatal("a new searcher wasn't matched with the waiting session")
	}
}

func TestRankedSessionRematchOffer(t *testing.T) {
	fastSessions(t)
	f := newRankedFixture(t)
	a, b := f.connect("alice", true), f.connect("bob", true)
	f.session(a)
	f.session(b)
	f.playSet(f.startQuickMatch(a.user.CurrentRoom), a, b)
	waitFor(t, "both searching again", func() bool {
		return f.hub.matchmaker.contains(a.user.ID) && f.hub.matchmaker.contains(b.user.ID)
	})
	drainAll(a)
	drainAll(b)

	// after waiting, each is offered the other
	offers := map[*Client]protocol.QuickMatchRematchOfferPayload{}
	waitFor(t, "rematch offers", func() bool {
		for _, c := range []*Client{a, b} {
			if m := ofType(drainAll(c), protocol.TypeQuickMatchRematchOffer); len(m) > 0 {
				var offer protocol.QuickMatchRematchOfferPayload
				json.Unmarshal(m[0].Data, &offer)
				offers[c] = offer
			}
		}
		return len(offers) == 2
	})
	if len(offers[a].Opponents) != 1 || offers[a].Opponents[0] != "bob" {
		t.Fatalf("alice's offer = %+v", offers[a])
	}

	// bob keeps searching: alice, who said yes, is told and nothing starts
	f.hub.handleQuickMatchRematch(a, envelope(t, protocol.QuickMatchRematchRequest{Accept: true}))
	f.hub.handleQuickMatchRematch(b, envelope(t, protocol.QuickMatchRematchRequest{Accept: false}))
	if a.user.CurrentRoom != nil {
		t.Fatal("rematch started although bob kept searching")
	}
	notices := modNotices(a)
	if len(notices) == 0 || notices[len(notices)-1] != "bob is searching for a new opponent." {
		t.Fatalf("alice's notices = %v", notices)
	}

	// bob is asked again later; both saying yes starts the rematch
	waitFor(t, "a second offer to bob", func() bool {
		return len(ofType(drainAll(b), protocol.TypeQuickMatchRematchOffer)) > 0
	})
	f.hub.handleQuickMatchRematch(b, envelope(t, protocol.QuickMatchRematchRequest{Accept: true}))
	if a.user.CurrentRoom == nil || a.user.CurrentRoom != b.user.CurrentRoom {
		t.Fatal("accepted rematch didn't start")
	}
	if a.user.CurrentRoom.GetRanked() == nil {
		t.Fatal("the rematch room isn't ranked")
	}
}

func TestQuickMatchWarmupLeaveSearchesAgain(t *testing.T) {
	f := newRankedFixture(t)
	a, b := f.connect("alice", true), f.connect("bob", true)
	f.session(a)
	f.session(b)
	room := a.user.CurrentRoom
	drainAll(a)
	// bob (not the host) leaves during the warmup
	f.hub.handleRoomLeave(b, protocol.Envelope{})
	if f.hub.state.GetRoom(room.ID) != nil || a.user.CurrentRoom != nil {
		t.Fatal("the room stayed open with the host alone")
	}
	if !f.hub.matchmaker.contains(a.user.ID) || f.hub.matchmaker.contains(b.user.ID) {
		t.Fatal("expected alice back in the queue and bob out of it")
	}
	msgs := drainAll(a)
	if reasons := roomLeftReasons(msgs); len(reasons) != 1 || reasons[0] != "quick_match_cancelled" {
		t.Fatalf("alice's ROOM_LEFT reasons = %v", reasons)
	}

	// a casual quick match too: the host leaving puts the other one back
	c, d := f.connect("carol", false), f.connect("dave", false)
	for _, p := range []*Client{c, d} {
		f.hub.handleQuickMatchJoin(p, envelope(t, protocol.QuickMatchJoinRequest{Rom: protocol.RomInfo{Name: "Remix", MD5: testRomMD5}}))
	}
	if c.user.CurrentRoom == nil {
		t.Fatal("casual searchers didn't pair")
	}
	host, other := c, d
	if c.user.CurrentRoom.Host != c.user {
		host, other = d, c
	}
	f.hub.handleRoomLeave(host, protocol.Envelope{})
	if !f.hub.matchmaker.contains(other.user.ID) || f.hub.matchmaker.contains(host.user.ID) {
		t.Fatal("the remaining casual player isn't searching again")
	}
}

func TestRankedSessionStopAfterThisSet(t *testing.T) {
	fastSessions(t)
	f := newRankedFixture(t)
	a, b := f.connect("alice", true), f.connect("bob", true)
	f.session(a)
	f.session(b)
	room := a.user.CurrentRoom
	matchID := f.startQuickMatch(room)
	f.hub.handleQuickMatchSession(a, envelope(t, protocol.QuickMatchSessionRequest{Continuous: false}))
	f.playSet(matchID, a, b)
	waitFor(t, "bob searching again", func() bool { return f.hub.matchmaker.contains(b.user.ID) })
	if f.hub.matchmaker.contains(a.user.ID) || a.user.CurrentRoom != nil {
		t.Fatal("alice stopped her session but is searching or still seated")
	}
}

func TestRankedSessionQuitterLeavesSession(t *testing.T) {
	fastSessions(t)
	f := newRankedFixture(t)
	a, b := f.connect("alice", true), f.connect("bob", true)
	f.session(a)
	f.session(b)
	matchID := f.startQuickMatch(a.user.CurrentRoom)
	drainAll(b)
	// alice (the host) quits in game 1: the set is voided, bob searches again
	f.hub.handleRoomLeave(a, protocol.Envelope{})
	f.abandon(b, matchID, 1, nil, nil)
	if res := rankedResultFor(t, b); res.Status != "voided" {
		t.Fatalf("result = %+v", res)
	}
	waitFor(t, "bob searching again", func() bool { return f.hub.matchmaker.contains(b.user.ID) })
	if f.hub.matchmaker.contains(a.user.ID) {
		t.Fatal("the player who quit is searching again")
	}
	// bob avoids the quitter like any previous opponent
	f.session(a)
	if a.user.CurrentRoom != nil {
		t.Fatal("matched with the previous opponent straight away")
	}
}

// The quick match's host leaving mid-set must not read as a host closing a room:
// the client would stop for a "room closed" box until the player dismissed it.
func TestRankedSessionHostLeavesMidSet(t *testing.T) {
	fastSessions(t)
	f := newRankedFixture(t)
	host, guest := f.connect("p1", true), f.connect("p2", true)
	f.session(host)
	f.session(guest)
	room := host.user.CurrentRoom
	if room == nil || room.Host == nil {
		t.Fatal("no room formed")
	}
	if room.Host.ID != host.user.ID {
		host, guest = guest, host
	}
	f.startQuickMatch(room)
	drainAll(guest)

	f.hub.handleRoomLeave(host, protocol.Envelope{})
	if reasons := roomLeftReasons(drainAll(guest)); len(reasons) != 1 || reasons[0] != "quick_match_next" {
		t.Fatalf("the other player's ROOM_LEFT reasons = %v", reasons)
	}
}

func (f *rankedFixture) casualSession(c *Client) {
	f.t.Helper()
	f.hub.handleQuickMatchJoin(c, envelope(f.t, protocol.QuickMatchJoinRequest{
		Rom: protocol.RomInfo{Name: "Remix", MD5: testRomMD5}, Continuous: true,
	}))
}

func TestCasualSessionSearchesAgainWhenAPlayerClosesTheGame(t *testing.T) {
	fastSessions(t)
	f := newRankedFixture(t)
	a, b := f.connect("alice", false), f.connect("bob", false)
	f.casualSession(a)
	f.casualSession(b)
	room := a.user.CurrentRoom
	if room == nil || room.GetRanked() != nil {
		t.Fatal("casual session searchers didn't get a casual room")
	}
	f.startQuickMatch(room)
	drainAll(a)
	// bob (not the host) closes his game: the match is over for both
	other := b
	if room.Host == b.user {
		other = a
	}
	f.hub.handleMatchFinished(other, protocol.Envelope{})
	waitFor(t, "both searching again", func() bool {
		return f.hub.matchmaker.contains(a.user.ID) && f.hub.matchmaker.contains(b.user.ID)
	})
	if f.hub.state.GetRoom(room.ID) != nil {
		t.Fatal("the finished room is still open")
	}
	msgs := drainAll(a)
	if reasons := roomLeftReasons(msgs); len(reasons) != 1 || reasons[0] != "quick_match_next" {
		t.Fatalf("ROOM_LEFT reasons = %v", reasons)
	}
	statuses := ofType(msgs, protocol.TypeQuickMatchStatus)
	var status protocol.QuickMatchStatusPayload
	json.Unmarshal(statuses[len(statuses)-1].Data, &status)
	if !status.Searching || status.Ranked || !status.Continuous {
		t.Fatalf("status = %+v", status)
	}
	if a.user.CurrentRoom != nil {
		t.Fatal("rematched straight away")
	}
	c := f.connect("carol", false)
	f.casualSession(c)
	if c.user.CurrentRoom == nil {
		t.Fatal("a new searcher wasn't matched")
	}
}

func TestCasualSessionHostDisconnects(t *testing.T) {
	fastSessions(t)
	f := newRankedFixture(t)
	a, b := f.connect("alice", false), f.connect("bob", false)
	f.casualSession(a)
	f.casualSession(b)
	room := a.user.CurrentRoom
	f.startQuickMatch(room)
	host, other := a, b
	if room.Host == b.user {
		host, other = b, a
	}
	drainAll(other)
	f.hub.removeUserFromRoom(host.user, room, "disconnect")
	if !f.hub.matchmaker.contains(other.user.ID) {
		t.Fatal("the remaining player isn't searching again")
	}
	if reasons := roomLeftReasons(drainAll(other)); len(reasons) != 1 || reasons[0] != "quick_match_next" {
		t.Fatalf("ROOM_LEFT reasons = %v", reasons)
	}
}

func TestCasualWithoutSessionStaysInRoom(t *testing.T) {
	fastSessions(t)
	f := newRankedFixture(t)
	a, b := f.connect("alice", false), f.connect("bob", false)
	for _, p := range []*Client{a, b} {
		f.hub.handleQuickMatchJoin(p, envelope(t, protocol.QuickMatchJoinRequest{Rom: protocol.RomInfo{Name: "Remix", MD5: testRomMD5}}))
	}
	room := a.user.CurrentRoom
	f.startQuickMatch(room)
	f.hub.handleMatchFinished(a, protocol.Envelope{})
	f.hub.handleMatchFinished(b, protocol.Envelope{})
	time.Sleep(4 * qmNextSetDelay)
	if a.user.CurrentRoom != room || b.user.CurrentRoom != room || f.hub.matchmaker.contains(a.user.ID) {
		t.Fatal("players without a session were moved out of their room")
	}
}

func TestCasualSessionStopAfterThisMatch(t *testing.T) {
	fastSessions(t)
	f := newRankedFixture(t)
	a, b := f.connect("alice", false), f.connect("bob", false)
	f.casualSession(a)
	f.casualSession(b)
	room := a.user.CurrentRoom
	f.startQuickMatch(room)
	f.hub.handleQuickMatchSession(a, envelope(t, protocol.QuickMatchSessionRequest{Continuous: false}))
	f.hub.handleMatchFinished(b, protocol.Envelope{})
	waitFor(t, "bob searching again", func() bool { return f.hub.matchmaker.contains(b.user.ID) })
	if f.hub.matchmaker.contains(a.user.ID) || a.user.CurrentRoom != nil {
		t.Fatal("alice stopped her session but is searching or still seated")
	}
}
