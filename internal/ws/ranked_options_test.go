package ws

import (
	"encoding/json"
	"testing"

	"github.com/jay-day/rmgk-lobby/internal/protocol"
	"github.com/jay-day/rmgk-lobby/internal/state"
)

// withLocalPlayer gives c's PC a second, local player.
func (f *rankedFixture) withLocalPlayer(c *Client, name string) *state.User {
	f.t.Helper()
	local := &state.User{ID: f.hub.state.NextUserID(), Username: name, State: state.UserStateIdle}
	if err := f.hub.state.AddUser(local); err != nil {
		f.t.Fatalf("AddUser(%s): %v", name, err)
	}
	c.user.LocalPlayers = true
	c.user.AddLocal(local)
	return local
}

func failReason(t *testing.T, envs []protocol.Envelope) string {
	t.Helper()
	if len(envs) != 1 {
		t.Fatalf("got %d failures, want 1", len(envs))
	}
	var fail protocol.RoomJoinFailPayload
	if err := json.Unmarshal(envs[0].Data, &fail); err != nil {
		t.Fatal(err)
	}
	return fail.Reason
}

// A ranked seat is one account at one PC: a PC with local players can neither
// make a ranked room nor join one.
func TestRankedRoomRefusesLocalPlayers(t *testing.T) {
	f := newRankedFixture(t)
	owner := f.connect("owner", true)
	f.withLocalPlayer(owner, "couch")

	if room := f.createRankedRoom(owner, &protocol.RankedRoomRequest{Enabled: true}); room != nil {
		t.Fatal("a PC with a local player created a ranked room")
	}
	if got := failReason(t, drain(owner, protocol.TypeRoomCreateFail)); got != protocol.ReasonLocalPlayers {
		t.Fatalf("create refused with %q, want %q", got, protocol.ReasonLocalPlayers)
	}

	host := f.connect("host", true)
	host.user.LocalPlayers = true // or the room refuses local players for the host's sake
	room := f.createRankedRoom(host, &protocol.RankedRoomRequest{Enabled: true})
	if room == nil {
		t.Fatal("verified host could not create a ranked room")
	}
	f.hub.handleRoomJoin(owner, envelope(t, protocol.RoomJoinRequest{RoomID: room.ID}))
	if owner.user.CurrentRoom != nil {
		t.Fatal("a PC with a local player joined a ranked room")
	}
	if got := failReason(t, drain(owner, protocol.TypeRoomJoinFail)); got != protocol.ReasonLocalPlayers {
		t.Fatalf("join refused with %q, want %q", got, protocol.ReasonLocalPlayers)
	}
}

// Ranked rooms have no late join or save data: turning ranked on drops both,
// the options leave the room state, and updates to them are ignored.
func TestRankedRoomHasNoLateJoinOrSaveData(t *testing.T) {
	f := newRankedFixture(t)
	host := f.connect("host", true)
	f.hub.handleRoomCreate(host, envelope(t, protocol.RoomCreateRequest{
		Name: "casual", Rom: protocol.RomInfo{Name: "Remix", MD5: testRomMD5}, MaxPlayers: 2, Delay: 2, Prediction: 7,
	}))
	room := host.user.CurrentRoom
	if room == nil {
		t.Fatal("room wasn't created")
	}
	saveData := protocol.RoomUpdateSaveDataRequest{Enabled: true, Label: "Blank save", Hash: "0123456789abcdef"}
	f.hub.handleRoomUpdateLateJoin(host, envelope(t, protocol.RoomUpdateLateJoinRequest{Enabled: true}))
	f.hub.handleRoomUpdateSaveData(host, envelope(t, saveData))
	if !room.LateJoinEnabled || room.SaveData == nil {
		t.Fatalf("casual room: late join %v, save data %+v", room.LateJoinEnabled, room.SaveData)
	}

	f.hub.handleRoomUpdateRanked(host, envelope(t, protocol.RankedRoomRequest{Enabled: true}))
	if room.GetRanked() == nil {
		t.Fatal("room didn't turn ranked")
	}
	if room.LateJoinEnabled || room.SaveData != nil {
		t.Fatalf("ranked room kept late join %v, save data %+v", room.LateJoinEnabled, room.SaveData)
	}
	if rs := f.hub.roomState(room); rs.LateJoinEnabled != nil || rs.SaveData != nil {
		t.Fatalf("ranked room state offers late join %v / save data %+v", rs.LateJoinEnabled, rs.SaveData)
	}

	f.hub.handleRoomUpdateLateJoin(host, envelope(t, protocol.RoomUpdateLateJoinRequest{Enabled: true}))
	f.hub.handleRoomUpdateSaveData(host, envelope(t, saveData))
	if room.LateJoinEnabled || room.SaveData != nil || matchSeatsFor(room) != 0 {
		t.Fatalf("ranked room took late join %v, save data %+v", room.LateJoinEnabled, room.SaveData)
	}

	// back to casual, the options return, off
	f.hub.handleRoomUpdateRanked(host, envelope(t, protocol.RankedRoomRequest{Enabled: false}))
	rs := f.hub.roomState(room)
	if rs.LateJoinEnabled == nil || *rs.LateJoinEnabled || rs.SaveData == nil || rs.SaveData.Enabled {
		t.Fatalf("casual again: late join %v, save data %+v", rs.LateJoinEnabled, rs.SaveData)
	}
}

// An invite can't seat someone a ranked room would refuse at the door.
func TestInviteToRankedRoomNeedsEligibility(t *testing.T) {
	f := newRankedFixture(t)
	host := f.connect("host", true)
	host.user.Social = true
	room := f.createRankedRoom(host, &protocol.RankedRoomRequest{Enabled: true})
	if room == nil {
		t.Fatal("verified host could not create a ranked room")
	}

	invite := func(target *Client) string {
		t.Helper()
		drain(host, protocol.TypeChallengeResult)
		host.challengeTimes = nil
		f.hub.handleChallengeSend(host, envelope(t, protocol.ChallengeSendRequest{ToUserID: target.user.ID}))
		for _, env := range drain(host, protocol.TypeChallengeResult) {
			var result protocol.ChallengeResultPayload
			if json.Unmarshal(env.Data, &result) == nil {
				return result.Result
			}
		}
		return ""
	}

	guest := f.connect("guest", false)
	guest.user.Social = true
	if got := invite(guest); got != protocol.ChallengeUnavailable {
		t.Fatalf("invite of a guest = %q, want %q", got, protocol.ChallengeUnavailable)
	}

	owner := f.connect("owner", true)
	owner.user.Social = true
	f.withLocalPlayer(owner, "couch")
	if got := invite(owner); got != protocol.ChallengeUnavailable {
		t.Fatalf("invite of a PC with local players = %q, want %q", got, protocol.ChallengeUnavailable)
	}

	player := f.connect("player", true)
	player.user.Social = true
	if got := invite(player); got != "" {
		t.Fatalf("invite of a verified player refused: %q", got)
	}
}
