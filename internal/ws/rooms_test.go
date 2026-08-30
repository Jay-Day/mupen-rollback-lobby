package ws

import (
	"encoding/json"
	"testing"

	"github.com/jay-day/rmgk-lobby/internal/protocol"
	"github.com/jay-day/rmgk-lobby/internal/state"
)

func TestRoomStatePayloadIncludesPlayerDelayAndRoomPrediction(t *testing.T) {
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
	if !room.SetPlayerDelay(user.ID, 3, false) {
		t.Fatal("SetPlayerDelay did not update the seat")
	}
	if !room.SetPrediction(5, false) {
		t.Fatal("SetPrediction did not update the room")
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

func TestRoomPredictionUpdateIsHostOnly(t *testing.T) {
	host := &state.User{ID: 1, Username: "host"}
	guest := &state.User{ID: 2, Username: "guest"}
	room := &state.Room{
		ID:             10,
		Host:           host,
		MaxPlayers:     2,
		Prediction:     7,
		PredictionAuto: true,
		State:          state.RoomStateWaiting,
	}
	if err := room.AddPlayer(host); err != nil {
		t.Fatalf("AddPlayer(host): %v", err)
	}
	if err := room.AddPlayer(guest); err != nil {
		t.Fatalf("AddPlayer(guest): %v", err)
	}
	host.CurrentRoom = room
	guest.CurrentRoom = room

	request := func(prediction int, predictionAuto bool) protocol.Envelope {
		data, err := json.Marshal(protocol.RoomUpdatePredictionRequest{
			Prediction: prediction, PredictionAuto: predictionAuto,
		})
		if err != nil {
			t.Fatalf("json.Marshal: %v", err)
		}
		return protocol.Envelope{Data: data}
	}

	h := &Hub{}
	h.handleRoomUpdatePrediction(&Client{user: guest}, request(4, false))
	if room.Prediction != 7 || !room.PredictionAuto {
		t.Fatalf("guest changed room prediction to (%d, %v)",
			room.Prediction, room.PredictionAuto)
	}

	h.handleRoomUpdatePrediction(&Client{user: host}, request(4, false))
	if room.Prediction != 4 || room.PredictionAuto {
		t.Fatalf("host prediction update = (%d, %v), want (4, false)",
			room.Prediction, room.PredictionAuto)
	}

	room.SetState(state.RoomStateInGame)
	h.handleRoomUpdatePrediction(&Client{user: host}, request(9, false))
	if room.Prediction != 4 {
		t.Fatalf("in-game prediction changed to %d", room.Prediction)
	}
}

func TestCompatibilityPlayerSettingsCannotChangePrediction(t *testing.T) {
	guest := &state.User{ID: 2, Username: "guest"}
	room := &state.Room{
		ID: 11, MaxPlayers: 2, Prediction: 7, PredictionAuto: true,
		State: state.RoomStateWaiting,
	}
	if err := room.AddPlayer(guest); err != nil {
		t.Fatalf("AddPlayer: %v", err)
	}
	guest.CurrentRoom = room
	data, err := json.Marshal(protocol.RoomUpdatePlayerSettingsRequest{
		Delay: 3, DelayAuto: false, Prediction: 1, PredictionAuto: false,
	})
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}

	h := &Hub{}
	h.handleRoomUpdatePlayerSettings(&Client{user: guest}, protocol.Envelope{Data: data})
	if room.Prediction != 7 || !room.PredictionAuto {
		t.Fatalf("compatibility message changed prediction to (%d, %v)",
			room.Prediction, room.PredictionAuto)
	}
	if room.Players[0].FrameDelay != 3 || room.Players[0].DelayAuto {
		t.Fatalf("compatibility message did not preserve delay update")
	}
}

func TestCompatibilityHostSettingsChangeRoomPrediction(t *testing.T) {
	host := &state.User{ID: 1, Username: "host"}
	room := &state.Room{
		ID: 12, Host: host, MaxPlayers: 2, Prediction: 7, PredictionAuto: true,
		State: state.RoomStateWaiting,
	}
	if err := room.AddPlayer(host); err != nil {
		t.Fatalf("AddPlayer: %v", err)
	}
	host.CurrentRoom = room
	data, err := json.Marshal(protocol.RoomUpdatePlayerSettingsRequest{
		Delay: 2, DelayAuto: true, Prediction: 5, PredictionAuto: false,
	})
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}

	h := &Hub{}
	h.handleRoomUpdatePlayerSettings(&Client{user: host}, protocol.Envelope{Data: data})
	if room.Prediction != 5 || room.PredictionAuto {
		t.Fatalf("compatibility host prediction = (%d, %v), want (5, false)",
			room.Prediction, room.PredictionAuto)
	}
}
