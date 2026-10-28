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

	// Clients switch a running match to the new window, so the host may keep
	// changing it in game; it stays host-only there.
	room.SetState(state.RoomStateInGame)
	h.handleRoomUpdatePrediction(&Client{user: guest}, request(2, false))
	if room.Prediction != 4 {
		t.Fatalf("guest changed in-game prediction to %d", room.Prediction)
	}
	h.handleRoomUpdatePrediction(&Client{user: host}, request(9, false))
	if room.Prediction != 9 {
		t.Fatalf("in-game host prediction = %d, want 9", room.Prediction)
	}

	room.SetState(state.RoomStateStarting)
	h.handleRoomUpdatePrediction(&Client{user: host}, request(3, false))
	if room.Prediction != 9 {
		t.Fatalf("prediction changed to %d while starting", room.Prediction)
	}
}

func TestPlayerDelayUpdateIsAcceptedInGame(t *testing.T) {
	player := &state.User{ID: 1, Username: "player"}
	room := &state.Room{ID: 10, Host: player, MaxPlayers: 2, State: state.RoomStateInGame}
	if err := room.AddPlayer(player); err != nil {
		t.Fatalf("AddPlayer: %v", err)
	}
	player.CurrentRoom = room

	request := func(delay int) protocol.Envelope {
		data, err := json.Marshal(protocol.RoomUpdateDelayRequest{Delay: delay})
		if err != nil {
			t.Fatalf("json.Marshal: %v", err)
		}
		return protocol.Envelope{Data: data}
	}
	seatDelay := func() int { return roomStatePayload(room).Players[0].FrameDelay }

	h := &Hub{}
	h.handleRoomUpdateDelay(&Client{user: player}, request(5))
	if got := seatDelay(); got != 5 {
		t.Fatalf("in-game delay = %d, want 5", got)
	}
	if !roomStatePayload(room).InGameSettings {
		t.Fatal("ROOM_STATE does not advertise in-game settings")
	}

	room.SetState(state.RoomStateFinished)
	h.handleRoomUpdateDelay(&Client{user: player}, request(1))
	if got := seatDelay(); got != 5 {
		t.Fatalf("delay changed to %d after the match finished", got)
	}
}

func TestRoomLiveReplayChoiceIsSharedAndHostOnly(t *testing.T) {
	hostMessages := make(chan []byte, 4)
	guestMessages := make(chan []byte, 4)
	host := &state.User{ID: 1, Username: "host", Send: hostMessages}
	guest := &state.User{ID: 2, Username: "guest", Send: guestMessages}
	room := &state.Room{
		ID: 10, Host: host, MaxPlayers: 2, State: state.RoomStateWaiting,
	}
	for _, user := range []*state.User{host, guest} {
		if err := room.AddPlayer(user); err != nil {
			t.Fatalf("AddPlayer: %v", err)
		}
		user.CurrentRoom = room
	}
	h := &Hub{}
	update := func(user *state.User, enabled bool) {
		data, err := json.Marshal(protocol.RoomUpdateLiveReplayRequest{Enabled: enabled})
		if err != nil {
			t.Fatal(err)
		}
		h.dispatch(&Client{user: user}, protocol.Envelope{
			Type: protocol.TypeRoomUpdateLiveReplay, Data: data,
		})
	}
	checkState := func(data []byte, enabled bool) {
		t.Helper()
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(data, &fields); err != nil {
			t.Fatal(err)
		}
		value, present := fields["liveReplayEnabled"]
		var got bool
		if !present || json.Unmarshal(value, &got) != nil || got != enabled {
			t.Fatalf("liveReplayEnabled = %s (present %v), want %v", value, present, enabled)
		}
	}
	for _, enabled := range []bool{true, false} {
		update(host, enabled)
		for name, messages := range map[string]chan []byte{"host": hostMessages, "guest": guestMessages} {
			select {
			case data := <-messages:
				var env protocol.Envelope
				if err := json.Unmarshal(data, &env); err != nil {
					t.Fatal(err)
				}
				if env.Type != protocol.TypeRoomState {
					t.Fatalf("message = %s, want ROOM_STATE", env.Type)
				}
				checkState(env.Data, enabled)
			default:
				t.Fatalf("%s did not receive room state", name)
			}
		}
		update(guest, !enabled)
		if room.LiveReplayEnabled != enabled || len(host.Send) != 0 || len(guest.Send) != 0 {
			t.Fatal("guest changed or broadcast the live replay choice")
		}
		// Joining players receive the same choice in their initial snapshot.
		checkState(mustMarshal(roomStatePayload(room)), enabled)
	}
	for _, phase := range []state.RoomState{state.RoomStateStarting, state.RoomStateInGame} {
		room.SetState(phase)
		update(host, true)
		if room.LiveReplayEnabled || len(host.Send) != 0 || len(guest.Send) != 0 {
			t.Fatalf("host changed live replay during %s", phase)
		}
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
