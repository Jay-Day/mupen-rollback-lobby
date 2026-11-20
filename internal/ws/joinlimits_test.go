package ws

import (
	"testing"

	"github.com/jay-day/rmgk-lobby/internal/protocol"
	"github.com/jay-day/rmgk-lobby/internal/state"
)

func (f *lateJoinFixture) joinWithPing(name string, room *state.Room, pingMs int) []protocol.Envelope {
	f.t.Helper()
	f.drain(name)
	f.send(name, protocol.TypeRoomJoin, protocol.RoomJoinRequest{RoomID: room.ID, PingMs: pingMs})
	return f.drain(name)
}

func TestJoinLimitsTurnAwayWiFiAndHighPing(t *testing.T) {
	f := newLateJoinFixture(t, 4)
	f.capableUser("host")
	f.capableUser("laptop").Connection = "wifi"
	f.capableUser("phone").Connection = "cellular"
	f.capableUser("far").Connection = "lan"
	f.capableUser("near").Connection = "lan"
	f.capableUser("unknown").Connection = "lan"

	f.send("host", protocol.TypeRoomCreate, protocol.RoomCreateRequest{
		Name: "room", Rom: protocol.RomInfo{Name: "rom", MD5: "abc"}, MaxPlayers: 4, Prediction: 7,
		MaxPing: 100, WiredOnly: true,
	})
	room := f.users["host"].CurrentRoom
	if room == nil {
		t.Fatal("room wasn't created")
	}
	created := roomStateOf(t, f.drain("host"))
	if created.JoinLimits == nil || created.JoinLimits.MaxPing != 100 || !created.JoinLimits.WiredOnly {
		t.Fatalf("room state limits = %+v", created.JoinLimits)
	}
	list := f.h.buildRoomListPayload()
	if len(list.Rooms) != 1 || list.Rooms[0].MaxPing != 100 || !list.Rooms[0].WiredOnly {
		t.Fatalf("room list = %+v", list.Rooms)
	}

	for _, name := range []string{"laptop", "phone"} {
		if got := reasonOf(t, f.joinWithPing(name, room, 20), protocol.TypeRoomJoinFail); got != protocol.ReasonWiredOnly {
			t.Fatalf("%s: reason %q, want %q", name, got, protocol.ReasonWiredOnly)
		}
	}
	if got := reasonOf(t, f.joinWithPing("far", room, 140), protocol.TypeRoomJoinFail); got != protocol.ReasonPingTooHigh {
		t.Fatalf("far: reason %q, want %q", got, protocol.ReasonPingTooHigh)
	}
	if _, ok := findEnvelope(f.joinWithPing("near", room, 100), protocol.TypeRoomJoinOK); !ok {
		t.Fatal("near (exactly at the limit) wasn't let in")
	}
	// A client that can't tell its ping (an older one) isn't held to it.
	if _, ok := findEnvelope(f.joinWithPing("unknown", room, 0), protocol.TypeRoomJoinOK); !ok {
		t.Fatal("a joiner with no ping report wasn't let in")
	}
}

func TestJoinLimitsCheckEveryPlayerOnThePC(t *testing.T) {
	f := newLateJoinFixture(t, 4)
	f.capableUser("host")
	f.capableUser("mira").Connection = "lan"
	f.addLocal("mira", "sam").Connection = "wifi"
	f.send("host", protocol.TypeRoomCreate, protocol.RoomCreateRequest{
		Name: "room", Rom: protocol.RomInfo{Name: "rom", MD5: "abc"}, MaxPlayers: 4, WiredOnly: true,
	})
	room := f.users["host"].CurrentRoom
	if got := reasonOf(t, f.joinWithPing("mira", room, 20), protocol.TypeRoomJoinFail); got != protocol.ReasonWiredOnly {
		t.Fatalf("reason %q, want %q", got, protocol.ReasonWiredOnly)
	}
}

func TestJoinLimitsUpdateIsHostOnlyAndKeepsSeatedPlayers(t *testing.T) {
	f := newLateJoinFixture(t, 4)
	f.capableUser("host")
	f.capableUser("guest").Connection = "wifi"
	f.capableUser("late").Connection = "wifi"
	room := f.createRoom("host", 4)
	if _, ok := findEnvelope(f.joinWithPing("guest", room, 300), protocol.TypeRoomJoinOK); !ok {
		t.Fatal("an unlimited room turned someone away")
	}

	f.send("guest", protocol.TypeRoomUpdateJoinLimits, protocol.RoomUpdateJoinLimitsRequest{MaxPing: 50, WiredOnly: true})
	if maxPing, wiredOnly := room.JoinLimits(); maxPing != 0 || wiredOnly {
		t.Fatalf("a guest changed the limits to (%d, %v)", maxPing, wiredOnly)
	}

	f.drain("guest")
	f.send("host", protocol.TypeRoomUpdateJoinLimits, protocol.RoomUpdateJoinLimitsRequest{MaxPing: 5000, WiredOnly: true})
	if maxPing, wiredOnly := room.JoinLimits(); maxPing != maxJoinPing || !wiredOnly {
		t.Fatalf("limits = (%d, %v), want (%d, true)", maxPing, wiredOnly, maxJoinPing)
	}
	// The guest on Wi-Fi keeps their seat and hears the new limits.
	if f.users["guest"].CurrentRoom != room {
		t.Fatal("the guest lost their seat")
	}
	if st := roomStateOf(t, f.drain("guest")); st.JoinLimits == nil || !st.JoinLimits.WiredOnly {
		t.Fatalf("guest's room state limits = %+v", st.JoinLimits)
	}
	if got := reasonOf(t, f.joinWithPing("late", room, 10), protocol.TypeRoomJoinFail); got != protocol.ReasonWiredOnly {
		t.Fatalf("late: reason %q", got)
	}
}
