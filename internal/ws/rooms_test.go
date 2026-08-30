package ws

import (
	"testing"

	"github.com/jay-day/rmgk-lobby/internal/state"
)

func TestRoomStatePayloadIncludesPlayerRollbackSettings(t *testing.T) {
	room := &state.Room{
		ID:             1,
		MaxPlayers:     2,
		Delay:          2,
		DelayAuto:      true,
		Prediction:     7,
		PredictionAuto: true,
		State:          state.RoomStateWaiting,
	}
	user := &state.User{ID: 42, Username: "player"}
	if err := room.AddPlayer(user); err != nil {
		t.Fatalf("AddPlayer: %v", err)
	}
	if !room.SetPlayerRollbackSettings(user.ID, 3, false, 5, false) {
		t.Fatal("SetPlayerRollbackSettings did not update the seat")
	}

	payload := roomStatePayload(room)
	if len(payload.Players) != 1 {
		t.Fatalf("players = %d, want 1", len(payload.Players))
	}
	seat := payload.Players[0]
	if seat.FrameDelay != 3 || seat.DelayAuto ||
		seat.PredictionWindow != 5 || seat.PredictionAuto {
		t.Fatalf("seat settings = (%d, %v, %d, %v), want (3, false, 5, false)",
			seat.FrameDelay, seat.DelayAuto, seat.PredictionWindow, seat.PredictionAuto)
	}
}
