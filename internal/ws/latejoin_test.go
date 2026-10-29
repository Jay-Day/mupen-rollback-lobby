package ws

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/jay-day/rmgk-lobby/internal/protocol"
	"github.com/jay-day/rmgk-lobby/internal/state"
)

type lateJoinFixture struct {
	t     *testing.T
	h     *Hub
	room  *state.Room
	users map[string]*state.User
	inbox map[string]chan []byte
}

func newLateJoinFixture(t *testing.T, maxPlayers int) *lateJoinFixture {
	t.Helper()
	f := &lateJoinFixture{
		t:     t,
		h:     NewHub(state.New(), "", protocol.ICEConfiguration{}, nil, nil),
		users: map[string]*state.User{},
		inbox: map[string]chan []byte{},
	}
	return f
}

func (f *lateJoinFixture) user(name string, lateJoin bool) *state.User {
	inbox := make(chan []byte, 64)
	f.inbox[name] = inbox
	u := &state.User{
		ID:            f.h.state.NextUserID(),
		Username:      name,
		ClientVersion: "v1",
		ICESupported:  true,
		LateJoin:      lateJoin,
		Send:          inbox,
	}
	if err := f.h.state.AddUser(u); err != nil {
		f.t.Fatalf("AddUser(%s): %v", name, err)
	}
	f.users[name] = u
	return u
}

func (f *lateJoinFixture) client(name string) *Client {
	return &Client{user: f.users[name], send: f.inbox[name]}
}

func (f *lateJoinFixture) send(name, msgType string, data any) {
	f.t.Helper()
	raw, err := json.Marshal(data)
	if err != nil {
		f.t.Fatalf("json.Marshal: %v", err)
	}
	f.h.dispatch(f.client(name), protocol.Envelope{Type: msgType, Data: raw})
}

// drain returns the envelopes queued for name, in order.
func (f *lateJoinFixture) drain(name string) []protocol.Envelope {
	var out []protocol.Envelope
	ch := f.inbox[name]
	for {
		select {
		case b := <-ch:
			var env protocol.Envelope
			if err := json.Unmarshal(b, &env); err != nil {
				f.t.Fatalf("bad envelope: %v", err)
			}
			out = append(out, env)
		default:
			return out
		}
	}
}

func (f *lateJoinFixture) signals(name string) []protocol.MatchAdmitSignalPayload {
	var out []protocol.MatchAdmitSignalPayload
	for _, env := range f.drain(name) {
		if env.Type != protocol.TypeMatchAdmitSignal {
			continue
		}
		var sig protocol.MatchAdmitSignalPayload
		if err := json.Unmarshal(env.Data, &sig); err != nil {
			f.t.Fatalf("bad signal: %v", err)
		}
		out = append(out, sig)
	}
	return out
}

func findEnvelope(envs []protocol.Envelope, msgType string) (protocol.Envelope, bool) {
	for _, env := range envs {
		if env.Type == msgType {
			return env, true
		}
	}
	return protocol.Envelope{}, false
}

// startMatch seats the named users in a fresh room and starts its match.
func (f *lateJoinFixture) startMatch(maxPlayers int, names ...string) *state.Match {
	f.t.Helper()
	host := f.users[names[0]]
	f.room = &state.Room{
		ID:         f.h.state.NextRoomID(),
		Name:       "room",
		Host:       host,
		MaxPlayers: maxPlayers,
		State:      state.RoomStateWaiting,
	}
	f.h.state.AddRoom(f.room)
	for _, name := range names {
		if err := f.h.seatUser(f.users[name], f.room, state.UserStateInRoom); err != nil {
			f.t.Fatalf("seatUser(%s): %v", name, err)
		}
	}
	f.h.startMatchInRoom(f.room, host.Username)
	for _, name := range names {
		f.drain(name)
	}
	return host.CurrentMatch
}

func TestLateJoinMatchIsSizedForEverySeat(t *testing.T) {
	f := newLateJoinFixture(t, 4)
	f.user("host", true)
	f.user("guest", true)
	match := f.startMatch(4, "host", "guest")
	if match.Seats != 4 {
		t.Fatalf("seats = %d, want 4", match.Seats)
	}

	g := newLateJoinFixture(t, 4)
	g.user("host", true)
	g.user("old", false)
	old := g.startMatch(4, "host", "old")
	if old.Seats != 0 {
		t.Fatalf("a match with an older client has seats = %d, want 0", old.Seats)
	}
	g.user("late", true)
	g.send("late", protocol.TypeRoomJoin, protocol.RoomJoinRequest{RoomID: g.room.ID})
	if g.users["late"].CurrentRoom != nil {
		t.Fatal("joined a running match that can't take more players")
	}
}

func TestLateJoinHandOff(t *testing.T) {
	f := newLateJoinFixture(t, 3)
	f.user("host", true)
	f.user("guest", true)
	f.user("joiner", true)
	f.user("old", false)
	match := f.startMatch(3, "host", "guest")

	// An older client can't enter the running match at all.
	f.send("old", protocol.TypeRoomJoin, protocol.RoomJoinRequest{RoomID: f.room.ID})
	if f.users["old"].CurrentRoom != nil {
		t.Fatal("an older client joined a running match")
	}

	f.send("joiner", protocol.TypeRoomJoin, protocol.RoomJoinRequest{RoomID: f.room.ID})
	joiner := f.users["joiner"]
	if joiner.CurrentRoom != f.room || joiner.CurrentMatch != nil {
		t.Fatal("joiner should sit in the room outside the match")
	}
	state := roomStatePayload(f.room)
	if !state.LateJoin {
		t.Fatal("room state should advertise late join")
	}
	// Clients read a missing inMatch as true (older servers), so false must
	// be on the wire.
	if raw, _ := json.Marshal(state); !strings.Contains(string(raw), `"inMatch":false`) {
		t.Fatalf("room state JSON lacks inMatch:false: %s", raw)
	}
	for _, seat := range state.Players {
		if want := seat.UserID != joiner.ID; seat.InMatch != want {
			t.Fatalf("seat %d inMatch = %v, want %v", seat.Slot, seat.InMatch, want)
		}
	}
	for _, name := range []string{"host", "guest", "joiner"} {
		f.drain(name)
	}

	// Only the host may start it.
	f.send("guest", protocol.TypeMatchAdmit, protocol.MatchAdmitRequest{MatchID: match.ID, UserID: joiner.ID})
	if match.CurrentAdmission() != nil {
		t.Fatal("a non-host started a late join")
	}
	f.send("host", protocol.TypeMatchAdmit, protocol.MatchAdmitRequest{MatchID: match.ID, UserID: joiner.ID})
	admit := match.CurrentAdmission()
	if admit == nil {
		t.Fatal("host could not start the late join")
	}
	for _, name := range []string{"host", "guest", "joiner"} {
		env, ok := findEnvelope(f.drain(name), protocol.TypeMatchAdmitBegin)
		if !ok {
			t.Fatalf("%s got no MATCH_ADMIT_BEGIN", name)
		}
		var begin protocol.MatchAdmitBeginPayload
		if err := json.Unmarshal(env.Data, &begin); err != nil {
			t.Fatal(err)
		}
		if begin.Epoch != 1 || begin.Seats != 3 || len(begin.Peers) != 3 ||
			begin.Joiner.UserID != joiner.ID || begin.Joiner.Slot != 3 {
			t.Fatalf("%s begin = %+v", name, begin)
		}
	}

	// A second hand-off waits for this one.
	f.send("host", protocol.TypeMatchAdmit, protocol.MatchAdmitRequest{MatchID: match.ID, UserID: joiner.ID})
	if env, ok := findEnvelope(f.drain("host"), protocol.TypeMatchAdmitFail); !ok {
		t.Fatal("second MATCH_ADMIT was not refused")
	} else {
		var fail protocol.MatchAdmitFailPayload
		_ = json.Unmarshal(env.Data, &fail)
		if fail.Reason != protocol.ReasonAdmitBusy {
			t.Fatalf("refusal reason = %q", fail.Reason)
		}
	}

	step := func(from, stepName string) protocol.MatchAdmitSignalPayload {
		return protocol.MatchAdmitSignalPayload{MatchID: match.ID, AdmitID: admit.ID, Step: stepName}
	}
	expect := func(stepName string, receivers ...string) {
		t.Helper()
		want := map[string]bool{}
		for _, r := range receivers {
			want[r] = true
		}
		for _, name := range []string{"host", "guest", "joiner"} {
			got := false
			for _, sig := range f.signals(name) {
				if sig.Step == stepName {
					got = true
				}
			}
			if got != want[name] {
				t.Fatalf("step %s: %s received = %v, want %v", stepName, name, got, want[name])
			}
		}
	}

	f.send("host", protocol.TypeMatchAdmitSignal, step("host", protocol.AdmitStepManifest))
	expect(protocol.AdmitStepManifest, "joiner")
	f.send("guest", protocol.TypeMatchAdmitSignal, step("guest", protocol.AdmitStepManifest))
	expect(protocol.AdmitStepManifest)
	f.send("joiner", protocol.TypeMatchAdmitSignal, step("joiner", protocol.AdmitStepReady))
	expect(protocol.AdmitStepReady, "host")
	f.send("host", protocol.TypeMatchAdmitSignal, step("host", protocol.AdmitStepCut))
	expect(protocol.AdmitStepCut, "guest")
	f.send("guest", protocol.TypeMatchAdmitSignal, step("guest", protocol.AdmitStepCutReady))
	expect(protocol.AdmitStepCutReady, "host")
	f.send("host", protocol.TypeMatchAdmitSignal, step("host", protocol.AdmitStepState))
	expect(protocol.AdmitStepState, "joiner")
	f.send("joiner", protocol.TypeMatchAdmitSignal, step("joiner", protocol.AdmitStepGo))
	expect(protocol.AdmitStepGo)
	if joiner.CurrentMatch != nil {
		t.Fatal("the joiner committed its own join")
	}

	f.send("host", protocol.TypeMatchAdmitSignal, step("host", protocol.AdmitStepGo))
	expect(protocol.AdmitStepGo, "guest", "joiner")
	if joiner.CurrentMatch != match || match.CurrentAdmission() != nil {
		t.Fatal("go did not commit the join")
	}
	inMatch := 0
	for _, seat := range roomStatePayload(f.room).Players {
		if seat.InMatch {
			inMatch++
		}
	}
	if inMatch != 3 {
		t.Fatalf("players in match = %d, want 3", inMatch)
	}
}

func TestLateJoinAbortsWhenJoinerLeaves(t *testing.T) {
	f := newLateJoinFixture(t, 3)
	f.user("host", true)
	f.user("guest", true)
	f.user("joiner", true)
	match := f.startMatch(3, "host", "guest")
	f.send("joiner", protocol.TypeRoomJoin, protocol.RoomJoinRequest{RoomID: f.room.ID})
	f.send("host", protocol.TypeMatchAdmit, protocol.MatchAdmitRequest{MatchID: match.ID, UserID: f.users["joiner"].ID})
	if match.CurrentAdmission() == nil {
		t.Fatal("late join did not start")
	}
	for _, name := range []string{"host", "guest", "joiner"} {
		f.drain(name)
	}

	f.send("joiner", protocol.TypeRoomLeave, struct{}{})
	if match.CurrentAdmission() != nil {
		t.Fatal("the hand-off outlived its joiner")
	}
	for _, name := range []string{"host", "guest"} {
		aborted := false
		for _, sig := range f.signals(name) {
			if sig.Step == protocol.AdmitStepAbort && sig.Reason == protocol.ReasonJoinerLeft {
				aborted = true
			}
		}
		if !aborted {
			t.Fatalf("%s was not told the join was cancelled", name)
		}
	}

	// A version mismatch is refused up front.
	f.user("other", true).ClientVersion = "v2"
	f.send("other", protocol.TypeRoomJoin, protocol.RoomJoinRequest{RoomID: f.room.ID})
	f.send("host", protocol.TypeMatchAdmit, protocol.MatchAdmitRequest{MatchID: match.ID, UserID: f.users["other"].ID})
	if match.CurrentAdmission() != nil {
		t.Fatal("admitted a joiner on another build")
	}
}
