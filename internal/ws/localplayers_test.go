package ws

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/jay-day/rmgk-lobby/internal/protocol"
	"github.com/jay-day/rmgk-lobby/internal/state"
)

// addLocal adds a local player to owner's connection through the protocol and
// returns the new user.
func (f *lateJoinFixture) addLocal(owner, name string) *state.User {
	f.t.Helper()
	f.send(owner, protocol.TypeLocalPlayerAdd, protocol.LocalPlayerAddRequest{Name: name})
	u := f.h.state.GetUserByName(name)
	if u == nil {
		f.t.Fatalf("local player %s wasn't added: %+v", name, f.drain(owner))
	}
	f.users[name] = u
	return u
}

func (f *lateJoinFixture) capableUser(name string) *state.User {
	u := f.user(name, true)
	u.LocalPlayers = true
	return u
}

func roomStateOf(t *testing.T, envs []protocol.Envelope) protocol.RoomStatePayload {
	t.Helper()
	var out protocol.RoomStatePayload
	found := false
	for _, env := range envs {
		if env.Type == protocol.TypeRoomState || env.Type == protocol.TypeRoomJoinOK {
			if err := json.Unmarshal(env.Data, &out); err != nil {
				t.Fatalf("bad room state: %v", err)
			}
			found = true
		}
	}
	if !found {
		t.Fatalf("no room state in %d envelopes", len(envs))
	}
	return out
}

func (f *lateJoinFixture) createRoom(host string, maxPlayers int) *state.Room {
	f.t.Helper()
	f.send(host, protocol.TypeRoomCreate, protocol.RoomCreateRequest{
		Name: "room", Rom: protocol.RomInfo{Name: "rom", MD5: "abc"}, MaxPlayers: maxPlayers, Prediction: 7,
	})
	room := f.users[host].CurrentRoom
	if room == nil {
		f.t.Fatalf("room wasn't created: %+v", f.drain(host))
	}
	f.drain(host)
	f.room = room
	return room
}

func reasonOf(t *testing.T, envs []protocol.Envelope, msgType string) string {
	t.Helper()
	env, ok := findEnvelope(envs, msgType)
	if !ok {
		t.Fatalf("no %s in %+v", msgType, envs)
	}
	var fail protocol.RoomJoinFailPayload
	if err := json.Unmarshal(env.Data, &fail); err != nil {
		t.Fatalf("bad %s: %v", msgType, err)
	}
	return fail.Reason
}

func TestLocalPlayerIsAnOrdinaryUserInPresence(t *testing.T) {
	f := newLateJoinFixture(t, 4)
	f.capableUser("mira")
	sam := f.addLocal("mira", "sam")

	if sam.Owner != f.users["mira"] || sam.Send != nil {
		t.Fatalf("sam owner=%v send=%v, want mira and no connection", sam.Owner, sam.Send)
	}
	envs := f.drain("mira")
	if _, ok := findEnvelope(envs, protocol.TypeLocalPlayerAdded); !ok {
		t.Fatalf("owner got no LOCAL_PLAYER_ADDED: %+v", envs)
	}

	// Nothing a presence row carries marks sam as sharing a PC.
	raw, err := json.Marshal(f.h.presenceUserFromState(sam))
	if err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{"owner", "peerId", "local", "mira"} {
		if strings.Contains(strings.ToLower(string(raw)), strings.ToLower(leak)) {
			t.Fatalf("presence mentions %q: %s", leak, raw)
		}
	}
}

func TestLocalPlayerAddRefusals(t *testing.T) {
	f := newLateJoinFixture(t, 4)
	f.capableUser("mira")
	f.capableUser("other")

	refused := func(name, want string) {
		t.Helper()
		f.drain("mira")
		f.send("mira", protocol.TypeLocalPlayerAdd, protocol.LocalPlayerAddRequest{Name: name})
		envs := f.drain("mira")
		env, ok := findEnvelope(envs, protocol.TypeLocalPlayerFail)
		if !ok {
			t.Fatalf("adding %q wasn't refused: %+v", name, envs)
		}
		var fail protocol.LocalPlayerFailPayload
		if err := json.Unmarshal(env.Data, &fail); err != nil {
			t.Fatal(err)
		}
		if fail.Reason != want {
			t.Fatalf("adding %q: reason %q, want %q", name, fail.Reason, want)
		}
	}

	refused("a", protocol.ReasonInvalidName)
	refused("has space", protocol.ReasonInvalidName)
	refused("other", protocol.ReasonNameTaken)

	f.addLocal("mira", "sam")
	f.addLocal("mira", "lee")
	f.addLocal("mira", "kai")
	refused("one-too-many", protocol.ReasonTooManyPlayers)

	// The roster is locked inside a room.
	f.createRoom("mira", 4)
	refused("late", protocol.ReasonInRoom)
	f.send("mira", protocol.TypeLocalPlayerRemove, protocol.LocalPlayerRemoveRequest{UserID: f.users["sam"].ID})
	if len(f.users["mira"].Group()) != 4 {
		t.Fatalf("removing inside a room changed the roster: %d", len(f.users["mira"].Group()))
	}
}

func TestLocalPlayerRemoveFreesTheName(t *testing.T) {
	f := newLateJoinFixture(t, 4)
	f.capableUser("mira")
	sam := f.addLocal("mira", "sam")
	f.drain("mira")

	// Someone else's local player isn't yours to remove.
	f.capableUser("other")
	f.send("other", protocol.TypeLocalPlayerRemove, protocol.LocalPlayerRemoveRequest{UserID: sam.ID})
	if f.h.state.GetUser(sam.ID) == nil {
		t.Fatal("another connection removed sam")
	}

	f.send("mira", protocol.TypeLocalPlayerRemove, protocol.LocalPlayerRemoveRequest{UserID: sam.ID})
	if _, ok := findEnvelope(f.drain("mira"), protocol.TypeLocalPlayerRemoved); !ok {
		t.Fatal("owner got no LOCAL_PLAYER_REMOVED")
	}
	if f.h.state.GetUser(sam.ID) != nil || f.h.state.GetUserByName("sam") != nil {
		t.Fatal("sam still exists")
	}
	if len(f.users["mira"].Group()) != 1 {
		t.Fatalf("group = %d, want 1", len(f.users["mira"].Group()))
	}
	f.addLocal("mira", "sam")
}

func TestCreateRoomSeatsEveryLocalPlayerOrRefuses(t *testing.T) {
	f := newLateJoinFixture(t, 4)
	f.capableUser("mira")
	sam := f.addLocal("mira", "sam")
	kai := f.addLocal("mira", "kai")
	f.drain("mira")

	f.send("mira", protocol.TypeRoomCreate, protocol.RoomCreateRequest{
		Name: "room", Rom: protocol.RomInfo{Name: "rom", MD5: "abc"}, MaxPlayers: 2, Prediction: 7,
	})
	if got := reasonOf(t, f.drain("mira"), protocol.TypeRoomCreateFail); got != protocol.ReasonNotEnoughSeats {
		t.Fatalf("reason %q, want %q", got, protocol.ReasonNotEnoughSeats)
	}
	if f.users["mira"].CurrentRoom != nil || sam.CurrentRoom != nil {
		t.Fatal("a refused create left someone seated")
	}

	room := f.createRoom("mira", 4)
	if len(room.Players) != 3 {
		t.Fatalf("seated %d, want 3", len(room.Players))
	}
	if sam.CurrentRoom != room || kai.CurrentRoom != room {
		t.Fatal("local players weren't seated")
	}
	if f.users["mira"].GetState() != state.UserStateHosting || sam.GetState() != state.UserStateInRoom {
		t.Fatalf("states %s/%s, want hosting/in_room", f.users["mira"].GetState(), sam.GetState())
	}
	payload := roomStatePayload(room)
	for i, want := range []string{"mira", "sam", "kai"} {
		seat := payload.Players[i]
		if seat.Username != want || seat.Slot != i+1 || seat.PeerID != f.users["mira"].ID {
			t.Fatalf("seat %d = %+v, want %s in slot %d on mira's connection", i, seat, want, i+1)
		}
	}
}

func TestJoinSeatsEveryLocalPlayerOrRefuses(t *testing.T) {
	f := newLateJoinFixture(t, 4)
	f.capableUser("host")
	f.capableUser("guest")
	f.capableUser("mira")
	sam := f.addLocal("mira", "sam")
	f.drain("mira")

	room := f.createRoom("host", 3)
	f.send("guest", protocol.TypeRoomJoin, protocol.RoomJoinRequest{RoomID: room.ID})
	f.drain("guest")

	// One free seat, two players.
	f.send("mira", protocol.TypeRoomJoin, protocol.RoomJoinRequest{RoomID: room.ID})
	if got := reasonOf(t, f.drain("mira"), protocol.TypeRoomJoinFail); got != protocol.ReasonNotEnoughSeats {
		t.Fatalf("reason %q, want %q", got, protocol.ReasonNotEnoughSeats)
	}
	if f.users["mira"].CurrentRoom != nil || sam.CurrentRoom != nil || len(room.Players) != 2 {
		t.Fatal("a refused join left someone seated")
	}

	f.send("guest", protocol.TypeRoomLeave, nil)
	f.drain("host")
	f.send("mira", protocol.TypeRoomJoin, protocol.RoomJoinRequest{RoomID: room.ID})
	state := roomStateOf(t, f.drain("mira"))
	if len(state.Players) != 3 || f.users["mira"].CurrentRoom != room || sam.CurrentRoom != room {
		t.Fatalf("join seated %d: %+v", len(state.Players), state.Players)
	}
	// A room-wide message reaches the PC once, through the owner.
	f.h.broadcastRoomState(room)
	count := 0
	for _, env := range f.drain("mira") {
		if env.Type == protocol.TypeRoomState {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("PC got %d copies of a room-wide message", count)
	}
}

func TestOlderClientsNeverShareARoomWithLocalPlayers(t *testing.T) {
	f := newLateJoinFixture(t, 4)
	f.capableUser("mira")
	f.addLocal("mira", "sam")
	f.user("old", true) // no LocalPlayers capability
	f.capableUser("host")
	f.drain("mira")

	// An older client can't enter a room holding a local player.
	room := f.createRoom("mira", 4)
	f.send("old", protocol.TypeRoomJoin, protocol.RoomJoinRequest{RoomID: room.ID})
	if got := reasonOf(t, f.drain("old"), protocol.TypeRoomJoinFail); got != protocol.ReasonNeedsUpdate {
		t.Fatalf("reason %q, want %q", got, protocol.ReasonNeedsUpdate)
	}
	f.send("mira", protocol.TypeRoomLeave, nil)
	f.drain("mira")

	// A PC with local players can't enter a room holding an older client.
	f.users["old"].LateJoin = true
	f.send("old", protocol.TypeRoomCreate, protocol.RoomCreateRequest{
		Name: "old room", Rom: protocol.RomInfo{Name: "rom", MD5: "abc"}, MaxPlayers: 4, Prediction: 7,
	})
	oldRoom := f.users["old"].CurrentRoom
	f.send("mira", protocol.TypeRoomJoin, protocol.RoomJoinRequest{RoomID: oldRoom.ID})
	if got := reasonOf(t, f.drain("mira"), protocol.TypeRoomJoinFail); got != protocol.ReasonPeerNeedsUpdate {
		t.Fatalf("reason %q, want %q", got, protocol.ReasonPeerNeedsUpdate)
	}

	// Without local players on it, a new client still joins as usual.
	f.send("host", protocol.TypeRoomJoin, protocol.RoomJoinRequest{RoomID: oldRoom.ID})
	if f.users["host"].CurrentRoom != oldRoom {
		t.Fatal("a plain new client couldn't join an older client's room")
	}
}

func TestKickingOneLocalPlayerKicksThePC(t *testing.T) {
	for _, target := range []string{"mira", "sam"} {
		t.Run(target, func(t *testing.T) {
			f := newLateJoinFixture(t, 4)
			f.capableUser("host")
			f.capableUser("mira")
			sam := f.addLocal("mira", "sam")
			room := f.createRoom("host", 4)
			f.send("mira", protocol.TypeRoomJoin, protocol.RoomJoinRequest{RoomID: room.ID})
			f.drain("mira")
			f.drain("host")

			f.send("host", protocol.TypeRoomKick, protocol.RoomKickRequest{UserID: f.users[target].ID})

			if f.users["mira"].CurrentRoom != nil || sam.CurrentRoom != nil || len(room.Players) != 1 {
				t.Fatalf("kick left %d seated", len(room.Players))
			}
			envs := f.drain("mira")
			env, ok := findEnvelope(envs, protocol.TypeRoomLeft)
			if !ok {
				t.Fatalf("owner got no ROOM_LEFT: %+v", envs)
			}
			var left protocol.RoomLeftPayload
			if err := json.Unmarshal(env.Data, &left); err != nil || left.Reason != "kicked" {
				t.Fatalf("ROOM_LEFT reason %q (%v), want kicked", left.Reason, err)
			}

			// The ban covers the whole PC.
			f.send("mira", protocol.TypeRoomJoin, protocol.RoomJoinRequest{RoomID: room.ID})
			if got := reasonOf(t, f.drain("mira"), protocol.TypeRoomJoinFail); got != protocol.ReasonKickedFromRoom {
				t.Fatalf("rejoin reason %q, want %q", got, protocol.ReasonKickedFromRoom)
			}
		})
	}
}

func TestHostCantKickOwnLocalPlayer(t *testing.T) {
	f := newLateJoinFixture(t, 4)
	f.capableUser("mira")
	sam := f.addLocal("mira", "sam")
	room := f.createRoom("mira", 4)

	f.send("mira", protocol.TypeRoomKick, protocol.RoomKickRequest{UserID: sam.ID})
	if sam.CurrentRoom != room || len(room.Players) != 2 {
		t.Fatal("the host kicked their own local player")
	}
}

func TestLeavingTakesThePCOutAndHostLeavingDissolves(t *testing.T) {
	f := newLateJoinFixture(t, 4)
	f.capableUser("host")
	f.capableUser("mira")
	sam := f.addLocal("mira", "sam")
	f.capableUser("guest")
	lee := f.addLocal("guest", "lee")
	room := f.createRoom("host", 4)
	f.send("mira", protocol.TypeRoomJoin, protocol.RoomJoinRequest{RoomID: room.ID})
	if len(room.Players) != 3 {
		t.Fatalf("seated %d, want 3", len(room.Players))
	}

	f.send("mira", protocol.TypeRoomLeave, nil)
	if f.users["mira"].CurrentRoom != nil || sam.CurrentRoom != nil || len(room.Players) != 1 {
		t.Fatalf("leave left %d seated", len(room.Players))
	}
	if sam.GetState() != state.UserStateIdle {
		t.Fatalf("sam state %s, want idle", sam.GetState())
	}

	f.send("guest", protocol.TypeRoomJoin, protocol.RoomJoinRequest{RoomID: room.ID})
	f.drain("guest")
	f.send("host", protocol.TypeRoomLeave, nil)
	if f.users["guest"].CurrentRoom != nil || lee.CurrentRoom != nil {
		t.Fatal("a dissolving room left a PC seated")
	}
	if f.h.state.GetRoom(room.ID) != nil {
		t.Fatal("room wasn't dissolved")
	}
	if _, ok := findEnvelope(f.drain("guest"), protocol.TypeRoomLeft); !ok {
		t.Fatal("guest got no ROOM_LEFT")
	}
}

func TestDisconnectRemovesLocalPlayers(t *testing.T) {
	f := newLateJoinFixture(t, 4)
	f.capableUser("host")
	f.capableUser("mira")
	sam := f.addLocal("mira", "sam")
	room := f.createRoom("host", 4)
	f.send("mira", protocol.TypeRoomJoin, protocol.RoomJoinRequest{RoomID: room.ID})

	f.h.cleanupClient(f.client("mira"))

	if len(room.Players) != 1 {
		t.Fatalf("disconnect left %d seated", len(room.Players))
	}
	if f.h.state.GetUser(sam.ID) != nil || f.h.state.GetUserByName("sam") != nil {
		t.Fatal("sam outlived the connection")
	}
}

func TestDelayAppliesToEverySeatOnAPC(t *testing.T) {
	f := newLateJoinFixture(t, 4)
	f.capableUser("mira")
	f.addLocal("mira", "sam")
	room := f.createRoom("mira", 4)

	f.send("mira", protocol.TypeRoomUpdateDelay, protocol.RoomUpdateDelayRequest{Delay: 4})
	for _, seat := range roomStatePayload(room).Players {
		if seat.FrameDelay != 4 || seat.DelayAuto {
			t.Fatalf("%s delay %d auto %v, want 4 and fixed", seat.Username, seat.FrameDelay, seat.DelayAuto)
		}
	}
}

func TestQuickMatchRefusesAPCWithLocalPlayers(t *testing.T) {
	f := newLateJoinFixture(t, 4)
	f.capableUser("mira")
	f.addLocal("mira", "sam")
	f.drain("mira")

	f.send("mira", protocol.TypeQuickMatchJoin, protocol.QuickMatchJoinRequest{
		Rom: protocol.RomInfo{Name: "rom", MD5: "abc"},
	})
	if f.h.matchmaker.contains(f.users["mira"].ID) {
		t.Fatal("a PC with local players entered the queue")
	}
	env, ok := findEnvelope(f.drain("mira"), protocol.TypeQuickMatchStatus)
	if !ok {
		t.Fatal("no QUICK_MATCH_STATUS")
	}
	var status protocol.QuickMatchStatusPayload
	if err := json.Unmarshal(env.Data, &status); err != nil {
		t.Fatal(err)
	}
	if status.Searching || status.Reason != protocol.ReasonLocalPlayers {
		t.Fatalf("status %+v, want refused for local players", status)
	}

	// The roster can't change while searching, so it can't get around that.
	f.capableUser("solo")
	f.send("solo", protocol.TypeQuickMatchJoin, protocol.QuickMatchJoinRequest{
		Rom: protocol.RomInfo{Name: "rom", MD5: "def"},
	})
	f.drain("solo")
	f.send("solo", protocol.TypeLocalPlayerAdd, protocol.LocalPlayerAddRequest{Name: "late"})
	if len(f.users["solo"].Group()) != 1 {
		t.Fatal("added a local player while searching")
	}
}

func TestMatchBeginCarriesEverySeatWithItsPeer(t *testing.T) {
	f := newLateJoinFixture(t, 4)
	f.capableUser("host")
	f.capableUser("mira")
	sam := f.addLocal("mira", "sam")
	room := f.createRoom("host", 4)
	// Late join is the host's choice, off in a new room.
	f.send("host", protocol.TypeRoomUpdateLateJoin, protocol.RoomUpdateLateJoinRequest{Enabled: true})
	f.send("mira", protocol.TypeRoomJoin, protocol.RoomJoinRequest{RoomID: room.ID})
	f.drain("mira")
	f.drain("host")

	f.send("host", protocol.TypeRoomStart, nil)

	env, ok := findEnvelope(f.drain("mira"), protocol.TypeMatchBegin)
	if !ok {
		t.Fatal("no MATCH_BEGIN")
	}
	var begin protocol.MatchBeginPayload
	if err := json.Unmarshal(env.Data, &begin); err != nil {
		t.Fatal(err)
	}
	if len(begin.Peers) != 3 || begin.Seats != 4 {
		t.Fatalf("peers %d seats %d, want 3 and 4", len(begin.Peers), begin.Seats)
	}
	for _, peer := range begin.Peers {
		want := peer.UserID
		if peer.UserID == sam.ID {
			want = f.users["mira"].ID
		}
		if peer.PeerID != want {
			t.Fatalf("%s peerId %d, want %d", peer.Username, peer.PeerID, want)
		}
	}
	if sam.CurrentMatch == nil || sam.GetState() != state.UserStatePlaying {
		t.Fatalf("sam isn't in the match: %s", sam.GetState())
	}
}

func TestStartWithOnlyOnePCsPlayers(t *testing.T) {
	f := newLateJoinFixture(t, 4)
	f.capableUser("mira")
	f.addLocal("mira", "sam")
	f.createRoom("mira", 4)

	f.send("mira", protocol.TypeRoomStart, nil)

	if _, ok := findEnvelope(f.drain("mira"), protocol.TypeMatchBegin); !ok {
		t.Fatal("a PC alone in a room with two players couldn't start")
	}
}

func TestLocalPlayersTakeTheirPCsSaveData(t *testing.T) {
	f := newLateJoinFixture(t, 4)
	f.capableUser("mira").SaveData = true
	sam := f.addLocal("mira", "sam")
	if !sam.SaveData {
		t.Fatal("a local player didn't get its PC's save data support")
	}
	f.createRoom("mira", 4)
	f.send("mira", protocol.TypeRoomUpdateSaveData, testSave)
	f.drain("mira")

	f.send("mira", protocol.TypeRoomStart, nil)

	if _, ok := findEnvelope(f.drain("mira"), protocol.TypeMatchBegin); !ok {
		t.Fatal("a PC with a local player couldn't start a save data match")
	}
}

func TestDroppingTakesThePCOutButKeepsItsSeats(t *testing.T) {
	f := newLateJoinFixture(t, 4)
	f.capableUser("host")
	f.capableUser("mira")
	sam := f.addLocal("mira", "sam")
	room := f.createRoom("host", 4)
	f.send("mira", protocol.TypeRoomJoin, protocol.RoomJoinRequest{RoomID: room.ID})
	f.send("host", protocol.TypeRoomStart, nil)
	f.drain("mira")
	f.drain("host")

	f.send("mira", protocol.TypeMatchFinished, nil)

	if f.users["mira"].CurrentMatch != nil || sam.CurrentMatch != nil {
		t.Fatal("drop left someone in the match")
	}
	if sam.CurrentRoom != room || sam.GetState() != state.UserStateInRoom {
		t.Fatalf("sam lost the seat: %s", sam.GetState())
	}
	// Other seats hear about each player, so each port goes idle.
	left := map[uint64]int{}
	for _, env := range f.drain("host") {
		if env.Type != protocol.TypeMatchPeerLeft {
			continue
		}
		var p protocol.MatchPeerLeftPayload
		if err := json.Unmarshal(env.Data, &p); err != nil {
			t.Fatal(err)
		}
		left[p.UserID] = p.Slot
	}
	if left[f.users["mira"].ID] != 2 || left[sam.ID] != 3 {
		t.Fatalf("MATCH_PEER_LEFT slots %+v, want mira 2 and sam 3", left)
	}
	// The dropping PC isn't told about its own players.
	for _, env := range f.drain("mira") {
		if env.Type == protocol.TypeMatchPeerLeft {
			t.Fatal("the dropping PC got MATCH_PEER_LEFT")
		}
	}
}

func TestAddingOneLocalPlayerToARunningMatchAddsThePC(t *testing.T) {
	for _, target := range []string{"mira", "sam"} {
		t.Run(target, func(t *testing.T) {
			f := newLateJoinFixture(t, 4)
			f.capableUser("host")
			f.capableUser("guest")
			f.capableUser("mira")
			sam := f.addLocal("mira", "sam")
			room := f.createRoom("host", 4)
			// Late join is the host's choice, off in a new room.
			f.send("host", protocol.TypeRoomUpdateLateJoin, protocol.RoomUpdateLateJoinRequest{Enabled: true})
			f.send("guest", protocol.TypeRoomJoin, protocol.RoomJoinRequest{RoomID: room.ID})
			f.send("host", protocol.TypeRoomStart, nil)
			match := f.users["host"].CurrentMatch
			if match == nil || match.Seats != 4 {
				t.Fatalf("match %+v isn't open to late joiners", match)
			}

			f.send("mira", protocol.TypeRoomJoin, protocol.RoomJoinRequest{RoomID: room.ID})
			if f.users["mira"].CurrentRoom != room || sam.CurrentRoom != room {
				t.Fatal("couldn't enter the running room")
			}
			for _, name := range []string{"host", "guest", "mira"} {
				f.drain(name)
			}

			f.send("host", protocol.TypeMatchAdmit, protocol.MatchAdmitRequest{MatchID: match.ID, UserID: f.users[target].ID})

			envs := f.drain("mira")
			env, ok := findEnvelope(envs, protocol.TypeMatchAdmitBegin)
			if !ok {
				t.Fatalf("no MATCH_ADMIT_BEGIN: %+v", envs)
			}
			var begin protocol.MatchAdmitBeginPayload
			if err := json.Unmarshal(env.Data, &begin); err != nil {
				t.Fatal(err)
			}
			if begin.Joiner.UserID != f.users["mira"].ID || len(begin.Joiners) != 2 || len(begin.Peers) != 4 {
				t.Fatalf("begin joiner %d joiners %d peers %d", begin.Joiner.UserID, len(begin.Joiners), len(begin.Peers))
			}
			if begin.Joiners[1].UserID != sam.ID || begin.Joiners[1].PeerID != f.users["mira"].ID {
				t.Fatalf("second joiner %+v", begin.Joiners[1])
			}

			// The owner speaks for the PC, and go commits everyone at once.
			f.send("mira", protocol.TypeMatchAdmitSignal, protocol.MatchAdmitSignalPayload{
				MatchID: match.ID, AdmitID: begin.AdmitID, Step: protocol.AdmitStepReady,
			})
			if len(f.signals("host")) != 1 {
				t.Fatal("the host didn't hear the PC is ready")
			}
			f.send("host", protocol.TypeMatchAdmitSignal, protocol.MatchAdmitSignalPayload{
				MatchID: match.ID, AdmitID: begin.AdmitID, Step: protocol.AdmitStepGo,
			})
			if f.users["mira"].CurrentMatch != match || sam.CurrentMatch != match {
				t.Fatal("go didn't add the whole PC")
			}
			if len(match.Players) != 4 {
				t.Fatalf("match roster %d, want 4", len(match.Players))
			}
			if got := len(f.signals("mira")); got != 1 {
				t.Fatalf("PC got %d go signals, want 1", got)
			}
		})
	}
}

func TestAPCLeavingCancelsItsLateJoin(t *testing.T) {
	f := newLateJoinFixture(t, 4)
	f.capableUser("host")
	f.capableUser("mira")
	sam := f.addLocal("mira", "sam")
	room := f.createRoom("host", 4)
	// Late join is the host's choice, off in a new room.
	f.send("host", protocol.TypeRoomUpdateLateJoin, protocol.RoomUpdateLateJoinRequest{Enabled: true})
	f.send("host", protocol.TypeRoomStart, nil)
	match := f.users["host"].CurrentMatch
	f.send("mira", protocol.TypeRoomJoin, protocol.RoomJoinRequest{RoomID: room.ID})
	f.send("host", protocol.TypeMatchAdmit, protocol.MatchAdmitRequest{MatchID: match.ID, UserID: sam.ID})
	if match.CurrentAdmission() == nil {
		t.Fatal("no hand-off started")
	}
	f.drain("host")

	f.send("mira", protocol.TypeRoomLeave, nil)

	if match.CurrentAdmission() != nil {
		t.Fatal("the hand-off outlived the PC")
	}
	signals := f.signals("host")
	if len(signals) != 1 || signals[0].Step != protocol.AdmitStepAbort || signals[0].Reason != protocol.ReasonJoinerLeft {
		t.Fatalf("host signals %+v, want one joiner_left abort", signals)
	}
}

func TestPingProbeForALocalPlayerReachesItsOwner(t *testing.T) {
	f := newLateJoinFixture(t, 4)
	mira := f.capableUser("mira")
	mira.SetUDPEndpoint("1.2.3.4", 5000)
	sam := f.addLocal("mira", "sam")
	f.capableUser("other")
	f.drain("mira")

	f.send("other", protocol.TypePingProbeRequest, protocol.PingProbeRequestPayload{TargetUserID: sam.ID})
	env, ok := findEnvelope(f.drain("other"), protocol.TypePingProbeReply)
	if !ok {
		t.Fatal("no reply")
	}
	var reply protocol.PingProbeReplyPayload
	if err := json.Unmarshal(env.Data, &reply); err != nil {
		t.Fatal(err)
	}
	if reply.TargetUserID != sam.ID || reply.TargetEndpoint != "1.2.3.4:5000" {
		t.Fatalf("reply %+v, want sam at the owner's endpoint", reply)
	}

	// Probing your own PC goes nowhere.
	f.send("mira", protocol.TypePingProbeRequest, protocol.PingProbeRequestPayload{TargetUserID: sam.ID})
	if _, ok := findEnvelope(f.drain("mira"), protocol.TypePingProbeReply); ok {
		t.Fatal("a PC probed itself")
	}
}
