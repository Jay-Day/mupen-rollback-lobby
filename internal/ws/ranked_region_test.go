package ws

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/jay-day/rmgk-lobby/internal/protocol"
)

func (f *rankedFixture) searchWithin(c *Client, maxPing int) {
	f.t.Helper()
	f.hub.handleQuickMatchJoin(c, envelope(f.t, protocol.QuickMatchJoinRequest{
		Rom: protocol.RomInfo{Name: "Remix", MD5: testRomMD5}, Ranked: &protocol.RankedRoomRequest{Enabled: true, MaxPing: maxPing},
	}))
}

func TestRankedQuickMatchMaxPingEstimate(t *testing.T) {
	f := newRankedFixture(t)
	east, tokyo, west := f.connect("east", true), f.connect("tokyo", true), f.connect("west", true)
	east.user.Region, tokyo.user.Region, west.user.Region = "us-east", "asia-east", "us-west"

	// us-east to asia-east is about 180 ms: over a 120 ms limit on either side
	f.searchWithin(east, 120)
	f.searchWithin(tokyo, 0)
	if east.user.CurrentRoom != nil {
		t.Fatal("matched across a 120 ms limit")
	}
	// us-west is about 80 ms away
	f.searchWithin(west, 120)
	if east.user.CurrentRoom == nil || east.user.CurrentRoom != west.user.CurrentRoom {
		t.Fatal("players within the limit didn't match")
	}
	if east.user.CurrentRoom.GetRanked().MaxPing != 120 {
		t.Fatalf("room max ping = %d", east.user.CurrentRoom.GetRanked().MaxPing)
	}
}

func TestRankedQuickMatchMeasuredPingCancels(t *testing.T) {
	f := newRankedFixture(t)
	a, b := f.connect("alice", true), f.connect("bob", true)
	a.user.Region, b.user.Region = "us-east", "us-central" // estimate 40 ms
	f.searchWithin(a, 120)
	f.searchWithin(b, 120)
	room := a.user.CurrentRoom
	if room == nil {
		t.Fatal("didn't match")
	}
	drainAll(a)
	// the real path is much worse than the estimate
	f.hub.handleRoomPeerPing(a, envelope(t, protocol.RoomPeerPingRequest{UserID: b.user.ID, RTTMs: 210}))
	if !f.hub.quickMatchTooFar(room) {
		t.Fatal("a 210 ms path passed a 120 ms limit")
	}
	if f.hub.state.GetRoom(room.ID) != nil || !f.hub.matchmaker.contains(a.user.ID) || !f.hub.matchmaker.contains(b.user.ID) {
		t.Fatal("expected the room closed and both searching again")
	}
	notices := modNotices(a)
	if len(notices) == 0 || !strings.Contains(notices[len(notices)-1], "210 ms") {
		t.Fatalf("notices = %v", notices)
	}
	// they aren't matched again, even after accepting a rematch
	f.hub.matchmaker.update(a.user.ID, func(e *qmEntry) { e.rematchOK = true })
	f.hub.matchmaker.update(b.user.ID, func(e *qmEntry) { e.rematchOK = true })
	f.hub.quickMatchAttempt(a.user)
	if a.user.CurrentRoom != nil {
		t.Fatal("matched the too-far pair again")
	}
}

func TestRankedRoomMaxPing(t *testing.T) {
	f := newRankedFixture(t)
	host, far, near := f.connect("host", true), f.connect("far", true), f.connect("near", true)
	host.user.Region, far.user.Region, near.user.Region = "eu-west", "oceania", "eu-east"
	room := f.createRankedRoom(host, &protocol.RankedRoomRequest{Enabled: true, MaxPing: 120})

	f.hub.handleRoomJoin(far, envelope(t, protocol.RoomJoinRequest{RoomID: room.ID}))
	if far.user.CurrentRoom != nil {
		t.Fatal("a player about 280 ms away joined a 120 ms room")
	}
	fails := drain(far, protocol.TypeRoomJoinFail)
	var fail protocol.RoomJoinFailPayload
	if len(fails) == 1 {
		json.Unmarshal(fails[0].Data, &fail)
	}
	if fail.Reason != protocol.ReasonTooFar {
		t.Fatalf("join failure = %+v", fail)
	}
	f.hub.handleRoomJoin(near, envelope(t, protocol.RoomJoinRequest{RoomID: room.ID}))
	if near.user.CurrentRoom != room {
		t.Fatal("a nearby player couldn't join")
	}
	state := f.hub.roomState(room)
	if state.Ranked == nil || state.Ranked.MaxPing != 120 {
		t.Fatalf("room state ranked = %+v", state.Ranked)
	}

	// a measured path over the limit blocks the start
	f.hub.handleRoomPeerPing(near, envelope(t, protocol.RoomPeerPingRequest{UserID: host.user.ID, RTTMs: 150}))
	if problem := f.hub.rankedStartCheck(room, room.GetRanked()); !strings.Contains(problem, "150 ms") {
		t.Fatalf("start check = %q", problem)
	}
	f.hub.handleRoomPeerPing(host, envelope(t, protocol.RoomPeerPingRequest{UserID: near.user.ID, RTTMs: 60}))
	if problem := f.hub.rankedStartCheck(room, room.GetRanked()); problem != "" {
		t.Fatalf("start check after a better measurement = %q", problem)
	}
	// a report about someone outside the room is ignored
	f.hub.handleRoomPeerPing(near, envelope(t, protocol.RoomPeerPingRequest{UserID: far.user.ID, RTTMs: 999}))
	if _, ok := room.PeerPing(near.user.ID, far.user.ID); ok {
		t.Fatal("stored a ping to a player outside the room")
	}
}

func TestRankedSetRecordsRegions(t *testing.T) {
	f := newRankedFixture(t)
	a, b := f.connect("a", true), f.connect("b", true)
	a.user.Region, b.user.Region = "eu-west", "sa"
	room := f.createRankedRoom(a, &protocol.RankedRoomRequest{Enabled: true})
	f.hub.handleRoomJoin(b, envelope(t, protocol.RoomJoinRequest{RoomID: room.ID}))
	f.hub.handleRoomStart(a, protocol.Envelope{})
	match := a.user.CurrentMatch
	if match == nil {
		t.Fatal("didn't start")
	}
	for _, p := range []*Client{a, b} {
		f.hub.handleMatchResult(p, envelope(t, result(match.ID, 1, []int{2, 0})))
	}
	homes, err := f.store.HomeRegions()
	if err != nil {
		t.Fatal(err)
	}
	if homes[a.user.AccountID] != "eu-west" || homes[b.user.AccountID] != "sa" {
		t.Fatalf("home regions = %v", homes)
	}
}
