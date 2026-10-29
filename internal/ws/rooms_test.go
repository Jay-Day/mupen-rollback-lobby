package ws

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/jay-day/rmgk-lobby/internal/config"
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

func TestMatchBeginCarriesConfiguredKeyframeInterval(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lobby.json")
	if err := os.WriteFile(path, []byte(`{"liveReplayKeyframeSeconds": 45}`), 0o644); err != nil {
		t.Fatal(err)
	}
	settings, err := config.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	hostMessages := make(chan []byte, 8)
	guestMessages := make(chan []byte, 8)
	host := &state.User{ID: 1, Username: "host", Send: hostMessages}
	guest := &state.User{ID: 2, Username: "guest", Send: guestMessages}
	room := &state.Room{ID: 10, Host: host, MaxPlayers: 2, State: state.RoomStateWaiting}
	for _, user := range []*state.User{host, guest} {
		if err := room.AddPlayer(user); err != nil {
			t.Fatalf("AddPlayer: %v", err)
		}
		user.CurrentRoom = room
	}
	h := &Hub{state: state.New(), settings: settings}
	h.startMatchInRoom(room, "test")

	for name, messages := range map[string]chan []byte{"host": hostMessages, "guest": guestMessages} {
		var begin *protocol.MatchBeginPayload
		for len(messages) > 0 && begin == nil {
			var env protocol.Envelope
			if err := json.Unmarshal(<-messages, &env); err != nil {
				t.Fatal(err)
			}
			if env.Type == protocol.TypeMatchBegin {
				begin = &protocol.MatchBeginPayload{}
				if err := json.Unmarshal(env.Data, begin); err != nil {
					t.Fatal(err)
				}
			}
		}
		if begin == nil {
			t.Fatalf("%s got no MATCH_BEGIN", name)
		}
		if begin.KeyframeIntervalSec != 45 {
			t.Fatalf("%s MATCH_BEGIN keyframeIntervalSec = %d, want 45", name, begin.KeyframeIntervalSec)
		}
	}
}
