package ws

import (
	"encoding/json"
	"testing"

	"github.com/jay-day/rmgk-lobby/internal/protocol"
	"github.com/jay-day/rmgk-lobby/internal/state"
)

func liveReplayTestRoom(t *testing.T, roomState state.RoomState) (*state.Room, *Client, *Client, chan []byte, chan []byte) {
	t.Helper()
	hostSend := make(chan []byte, 1)
	guestSend := make(chan []byte, 1)
	host := &state.User{ID: 1, Username: "host", Send: hostSend}
	guest := &state.User{ID: 2, Username: "guest", Send: guestSend}
	room := &state.Room{
		ID:         10,
		Name:       "test room",
		Host:       host,
		MaxPlayers: 2,
		Players: []*state.RoomSeat{
			{User: host, Slot: 1},
			{User: guest, Slot: 2},
		},
		State: roomState,
	}
	host.CurrentRoom = room
	guest.CurrentRoom = room
	return room, &Client{user: host}, &Client{user: guest}, hostSend, guestSend
}

func liveReplayUpdateEnvelope(enabled bool) protocol.Envelope {
	return protocol.Envelope{Data: mustMarshal(protocol.RoomUpdateLiveReplayRequest{Enabled: enabled})}
}

func requireLiveReplayRoomState(t *testing.T, messages <-chan []byte, want bool) {
	t.Helper()
	select {
	case raw := <-messages:
		var env protocol.Envelope
		if err := json.Unmarshal(raw, &env); err != nil {
			t.Fatalf("decode ROOM_STATE envelope: %v", err)
		}
		if env.Type != protocol.TypeRoomState {
			t.Fatalf("message type = %q, want %q", env.Type, protocol.TypeRoomState)
		}
		var payload protocol.RoomStatePayload
		if err := json.Unmarshal(env.Data, &payload); err != nil {
			t.Fatalf("decode ROOM_STATE payload: %v", err)
		}
		if payload.LiveReplayEnabled != want {
			t.Fatalf("liveReplayEnabled = %v, want %v", payload.LiveReplayEnabled, want)
		}
	default:
		t.Fatal("expected ROOM_STATE broadcast")
	}
}

func TestRoomLiveReplayChoiceBroadcastsToEveryPlayer(t *testing.T) {
	room, host, _, hostSend, guestSend := liveReplayTestRoom(t, state.RoomStateWaiting)
	new(Hub).handleRoomUpdateLiveReplay(host, liveReplayUpdateEnvelope(true))

	if !room.LiveReplayEnabled {
		t.Fatal("host update did not change room state")
	}
	requireLiveReplayRoomState(t, hostSend, true)
	requireLiveReplayRoomState(t, guestSend, true)
}

func TestRoomLiveReplayChoiceRejectsNonHostAndMidMatchChanges(t *testing.T) {
	t.Run("non-host", func(t *testing.T) {
		room, _, guest, hostSend, guestSend := liveReplayTestRoom(t, state.RoomStateWaiting)
		new(Hub).handleRoomUpdateLiveReplay(guest, liveReplayUpdateEnvelope(true))
		if room.LiveReplayEnabled {
			t.Fatal("non-host changed room live replay state")
		}
		if len(hostSend) != 0 || len(guestSend) != 0 {
			t.Fatal("rejected non-host update was broadcast")
		}
	})

	t.Run("mid-match", func(t *testing.T) {
		room, host, _, hostSend, guestSend := liveReplayTestRoom(t, state.RoomStateInGame)
		new(Hub).handleRoomUpdateLiveReplay(host, liveReplayUpdateEnvelope(true))
		if room.LiveReplayEnabled {
			t.Fatal("mid-match update changed room live replay state")
		}
		if len(hostSend) != 0 || len(guestSend) != 0 {
			t.Fatal("rejected mid-match update was broadcast")
		}
	})
}
