package ws

import (
	"testing"

	"github.com/jay-day/rmgk-lobby/internal/protocol"
	"github.com/jay-day/rmgk-lobby/internal/state"
)

func (f *lateJoinFixture) resize(name string, maxPlayers int) {
	f.t.Helper()
	f.send(name, protocol.TypeRoomUpdateMaxPlayers, protocol.RoomUpdateMaxPlayersRequest{MaxPlayers: maxPlayers})
}

func seatSlots(room *state.Room) map[string]int {
	out := map[string]int{}
	for _, seat := range room.Players {
		out[seat.User.Username] = seat.Slot
	}
	return out
}

func TestRoomSizeGrowsAndShrinksForTheHost(t *testing.T) {
	f := newLateJoinFixture(t, 4)
	f.capableUser("host")
	f.capableUser("guest")
	room := f.createRoom("host", 2)
	if !roomStateOf(t, f.joinWithPing("guest", room, 0)).Resizable {
		t.Fatal("a room the host made isn't marked resizable")
	}

	f.resize("guest", 4)
	if room.MaxPlayers != 2 {
		t.Fatalf("a guest changed the size to %d", room.MaxPlayers)
	}

	f.drain("guest")
	f.resize("host", 4)
	if room.MaxPlayers != 4 {
		t.Fatalf("size = %d, want 4", room.MaxPlayers)
	}
	if got := roomStateOf(t, f.drain("guest")); got.MaxPlayers != 4 {
		t.Fatalf("guest's room state says %d seats", got.MaxPlayers)
	}
	list := f.h.buildRoomListPayload()
	if len(list.Rooms) != 1 || list.Rooms[0].MaxPlayers != 4 {
		t.Fatalf("room list = %+v", list.Rooms)
	}

	// Out of range is ignored.
	for _, bad := range []int{1, 5, 0} {
		f.resize("host", bad)
		if room.MaxPlayers != 4 {
			t.Fatalf("size %d was taken: now %d", bad, room.MaxPlayers)
		}
	}

	f.resize("host", 3)
	if room.MaxPlayers != 3 {
		t.Fatalf("size = %d, want 3", room.MaxPlayers)
	}
}

func TestRoomSizeKeepsEveryoneSeated(t *testing.T) {
	f := newLateJoinFixture(t, 4)
	f.capableUser("host")
	f.capableUser("a")
	f.capableUser("b")
	room := f.createRoom("host", 4)
	f.joinWithPing("a", room, 0)
	f.joinWithPing("b", room, 0)

	// Three seated: two seats is too few.
	f.resize("host", 2)
	if room.MaxPlayers != 4 {
		t.Fatalf("shrank below the players seated: %d", room.MaxPlayers)
	}

	// Someone in P4 moves into the free seat when P4 goes away.
	f.send("host", protocol.TypeRoomSwapSlots, protocol.RoomSwapSlotsRequest{SlotA: 2, SlotB: 4})
	if got := seatSlots(room); got["a"] != 4 {
		t.Fatalf("setup: seats %v", got)
	}
	f.resize("host", 3)
	got := seatSlots(room)
	if room.MaxPlayers != 3 || got["host"] != 1 || got["a"] != 2 || got["b"] != 3 {
		t.Fatalf("size %d, seats %v", room.MaxPlayers, got)
	}
}

func TestRoomSizeOnlyWhileWaitingAndNotForQuickMatch(t *testing.T) {
	f := newLateJoinFixture(t, 4)
	f.capableUser("host")
	room := f.createRoom("host", 2)

	room.State = state.RoomStateInGame
	f.resize("host", 4)
	if room.MaxPlayers != 2 {
		t.Fatalf("resized during a match: %d", room.MaxPlayers)
	}

	room.State = state.RoomStateWaiting
	room.QuickMatch = true
	f.resize("host", 4)
	if room.MaxPlayers != 2 {
		t.Fatalf("resized a quick match room: %d", room.MaxPlayers)
	}
	if roomStatePayload(room).Resizable {
		t.Fatal("a quick match room is marked resizable")
	}
}
