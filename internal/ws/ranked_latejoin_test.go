package ws

import (
	"testing"

	"github.com/jay-day/rmgk-lobby/internal/protocol"
	"github.com/jay-day/rmgk-lobby/internal/state"
)

// A ranked set's players are fixed when it starts, even when every client can
// late join: no idle seats, no starting alone, no joining mid-set.
func TestRankedRoomTakesNoLateJoiners(t *testing.T) {
	f := newRankedFixture(t)
	lateJoiner := func(c *Client) *Client {
		c.user.LateJoin, c.user.ICESupported = true, true
		return c
	}
	host := lateJoiner(f.connect("alice", true))
	guest := lateJoiner(f.connect("bob", true))
	third := lateJoiner(f.connect("carol", true))

	room := f.createRankedRoom(host, &protocol.RankedRoomRequest{Enabled: true})
	if room == nil {
		t.Fatal("ranked room not created")
	}
	if roomCanStartAlone(room) {
		t.Fatal("a ranked room offers starting alone")
	}
	f.hub.handleRoomStart(host, protocol.Envelope{})
	if room.State != state.RoomStateWaiting {
		t.Fatal("a ranked set started with one player")
	}

	f.hub.handleRoomJoin(guest, envelope(t, protocol.RoomJoinRequest{RoomID: room.ID}))
	f.hub.handleRoomStart(host, protocol.Envelope{})
	match := host.user.CurrentMatch
	if room.State != state.RoomStateInGame || match == nil {
		t.Fatal("the ranked set didn't start")
	}
	if match.Seats != 0 {
		t.Fatalf("seats = %d, want the set's own players only", match.Seats)
	}
	if roomAcceptsLateJoin(room) {
		t.Fatal("a running ranked set accepts late joiners")
	}

	drain(third, protocol.TypeRoomJoinFail)
	f.hub.handleRoomJoin(third, envelope(t, protocol.RoomJoinRequest{RoomID: room.ID}))
	if len(drain(third, protocol.TypeRoomJoinFail)) != 1 || third.user.CurrentRoom != nil {
		t.Fatal("joined a running ranked set")
	}

	// a player who dropped can't be added back either
	f.hub.handleMatchFinished(guest, protocol.Envelope{})
	drain(host, protocol.TypeMatchAdmitFail)
	f.hub.handleMatchAdmit(host, envelope(t, protocol.MatchAdmitRequest{MatchID: match.ID, UserID: guest.user.ID}))
	if len(drain(host, protocol.TypeMatchAdmitFail)) != 1 || match.CurrentAdmission() != nil {
		t.Fatal("a dropped ranked player was added back")
	}
}
