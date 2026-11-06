package ws

import (
	"encoding/json"
	"testing"

	"github.com/jay-day/rmgk-lobby/internal/moderation"
	"github.com/jay-day/rmgk-lobby/internal/protocol"
	"github.com/jay-day/rmgk-lobby/internal/state"
)

func (f *lateJoinFixture) sharesSpectatorChat(name string) *state.User {
	u := f.socialUser(name)
	u.SpectatorChat = true
	return u
}

// broadcasting starts a broadcast of m, hosted by its first player.
func (f *lateJoinFixture) broadcasting(m *state.Match) {
	f.t.Helper()
	ids := make([]uint64, 0, len(m.Players))
	for _, player := range m.Players {
		ids = append(ids, player.ID)
	}
	if !f.h.broadcasts.begin(m.ID, m.RoomID, m.Players[0].ID, ids, "") {
		f.t.Fatal("broadcast didn't begin")
	}
	f.t.Cleanup(func() { f.h.broadcasts.endByMatch(m.ID) })
}

func (f *lateJoinFixture) watch(name string, m *state.Match) {
	f.t.Helper()
	f.h.broadcasts.spectateStart(f.client(name), m.ID)
	if !f.watching(name, m) {
		f.t.Fatalf("%s isn't watching", name)
	}
}

func (f *lateJoinFixture) watching(name string, m *state.Match) bool {
	b := f.h.broadcasts.byMatch[m.ID]
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.spectators[f.users[name].ID] != nil
}

func (f *lateJoinFixture) sayOnSpectatorChat(name, message string) {
	f.t.Helper()
	f.send(name, protocol.TypeChatSend, protocol.ChatSendRequest{Channel: channelSpectate, Message: message})
}

func (f *lateJoinFixture) chatHeardBy(name string) []protocol.ChatMsgPayload {
	f.t.Helper()
	var out []protocol.ChatMsgPayload
	for _, env := range f.drain(name) {
		if env.Type != protocol.TypeChatMsg {
			continue
		}
		var msg protocol.ChatMsgPayload
		if err := json.Unmarshal(env.Data, &msg); err != nil {
			f.t.Fatalf("bad chat: %v", err)
		}
		out = append(out, msg)
	}
	return out
}

// heardOnce fails unless name got exactly this line, and returns it.
func (f *lateJoinFixture) heardOnce(name, message string) protocol.ChatMsgPayload {
	f.t.Helper()
	got := f.chatHeardBy(name)
	if len(got) != 1 || got[0].Message != message || got[0].Channel != channelSpectate {
		f.t.Fatalf("%s heard %+v, want %q", name, got, message)
	}
	return got[0]
}

func (f *lateJoinFixture) heardNothing(name string) {
	f.t.Helper()
	if got := f.chatHeardBy(name); len(got) != 0 {
		f.t.Fatalf("%s heard %+v", name, got)
	}
}

func spectatorChatMatch(t *testing.T) (*lateJoinFixture, *state.Match) {
	f := newLateJoinFixture(t, 4)
	f.sharesSpectatorChat("mira")
	f.sharesSpectatorChat("kart")
	f.sharesSpectatorChat("viewer")
	match := f.startMatch(2, "mira", "kart")
	f.broadcasting(match)
	f.watch("viewer", match)
	return f, match
}

func TestSpectatorCommentsReachThePlayers(t *testing.T) {
	f, _ := spectatorChatMatch(t)

	f.sayOnSpectatorChat("viewer", "that edgeguard!!")

	for _, name := range []string{"viewer", "mira", "kart"} {
		if got := f.heardOnce(name, "that edgeguard!!"); got.FromPlayer {
			t.Fatalf("%s was told a viewer is a player", name)
		}
	}
}

func TestPlayersCanAnswerTheirSpectators(t *testing.T) {
	f, _ := spectatorChatMatch(t)

	f.sayOnSpectatorChat("mira", "ty, close one")

	for _, name := range []string{"viewer", "mira", "kart"} {
		got := f.heardOnce(name, "ty, close one")
		if !got.FromPlayer || got.FromUsername != "mira" {
			t.Fatalf("%s got %+v", name, got)
		}
	}
}

func TestOldSpectatorsKeepThePromiseTheirClientMade(t *testing.T) {
	f, match := spectatorChatMatch(t)
	f.socialUser("oldviewer") // told the players can't read this
	f.watch("oldviewer", match)

	f.sayOnSpectatorChat("oldviewer", "kart keeps rolling in")

	f.heardOnce("oldviewer", "kart keeps rolling in")
	f.heardOnce("viewer", "kart keeps rolling in")
	f.heardNothing("mira")
	f.heardNothing("kart")

	// They still see what the players say, like anyone else watching.
	f.sayOnSpectatorChat("mira", "gg")
	f.heardOnce("oldviewer", "gg")
}

func TestOldPlayersGetNothingAndCantPost(t *testing.T) {
	f := newLateJoinFixture(t, 4)
	f.sharesSpectatorChat("mira")
	f.socialUser("oldplayer")
	f.sharesSpectatorChat("viewer")
	match := f.startMatch(2, "mira", "oldplayer")
	f.broadcasting(match)
	f.watch("viewer", match)

	f.sayOnSpectatorChat("viewer", "hello")
	f.heardOnce("mira", "hello")
	f.heardNothing("oldplayer")
	f.chatHeardBy("viewer")

	f.sayOnSpectatorChat("oldplayer", "from an old client")
	f.heardNothing("mira")
	f.heardNothing("viewer")
}

func TestSpectatorChatStaysWithThoseInTheMatch(t *testing.T) {
	f := newLateJoinFixture(t, 4)
	f.sharesSpectatorChat("mira")
	kart := f.sharesSpectatorChat("kart")
	waiting := f.sharesSpectatorChat("waiting")
	f.sharesSpectatorChat("viewer")
	f.sharesSpectatorChat("lobby")
	match := f.startMatch(3, "mira", "kart")
	f.broadcasting(match)
	f.watch("viewer", match)
	// Seated beside the running match, not in it yet.
	if err := f.h.seatUser(waiting, f.room, state.UserStateInRoom); err != nil {
		t.Fatal(err)
	}
	f.drain("waiting")

	f.sayOnSpectatorChat("viewer", "one")
	f.heardNothing("waiting")
	f.heardNothing("lobby")
	f.heardOnce("mira", "one")
	f.chatHeardBy("kart")
	f.chatHeardBy("viewer")

	// Nor can they post, and neither can someone in the lobby.
	f.sayOnSpectatorChat("waiting", "from the room")
	f.sayOnSpectatorChat("lobby", "from the lobby")
	f.heardNothing("viewer")
	f.heardNothing("mira")

	// Dropping out ends it.
	kart.CurrentMatch = nil
	f.sayOnSpectatorChat("viewer", "two")
	f.heardNothing("kart")
	f.heardOnce("mira", "two")

	// Joining mid-match starts it.
	waiting.CurrentMatch = match
	f.sayOnSpectatorChat("viewer", "three")
	f.heardOnce("waiting", "three")
}

func TestLocalPlayersGetOneCopyThroughTheirOwner(t *testing.T) {
	f := newLateJoinFixture(t, 4)
	f.sharesSpectatorChat("mira")
	f.addLocal("mira", "sam")
	f.sharesSpectatorChat("kart")
	f.sharesSpectatorChat("viewer")
	f.drain("mira")
	match := f.startMatch(3, "mira", "kart")
	f.broadcasting(match)
	f.watch("viewer", match)

	f.sayOnSpectatorChat("viewer", "@sam nice")
	f.heardOnce("mira", "@sam nice")
}

func TestSpectatorChatEndsWithTheBroadcast(t *testing.T) {
	f, match := spectatorChatMatch(t)
	f.h.broadcasts.endByMatch(match.ID)
	f.sayOnSpectatorChat("mira", "anyone there?")
	f.sayOnSpectatorChat("viewer", "still here")
	for _, name := range []string{"viewer", "mira", "kart"} {
		f.heardNothing(name)
	}
}

func TestNoBroadcastNoSpectatorChat(t *testing.T) {
	f := newLateJoinFixture(t, 4)
	f.sharesSpectatorChat("mira")
	f.sharesSpectatorChat("kart")
	f.startMatch(2, "mira", "kart")

	f.sayOnSpectatorChat("mira", "hello?")

	f.heardNothing("mira")
	f.heardNothing("kart")
}

func TestPlayersAnswersObeyMuteAndFilter(t *testing.T) {
	f, _ := spectatorChatMatch(t)
	f.h.mod = moderation.NewManager(moderation.NewFilter([]string{"badword"}), &moderation.AdminStore{}, "")
	raw := func(msg string) protocol.Envelope {
		data, _ := json.Marshal(protocol.ChatSendRequest{Channel: channelSpectate, Message: msg})
		return protocol.Envelope{Type: protocol.TypeChatSend, Data: data}
	}
	mira := f.client("mira")
	mira.ip = "198.51.100.1"

	f.h.dispatch(mira, raw("a badword here"))
	f.heardNothing("viewer")

	f.h.mod.Mute("198.51.100.1", "mira", "mod", "", 0)
	f.h.dispatch(mira, raw("hello"))
	f.heardNothing("viewer")
}

func TestLocalPlayersAddedLaterShareTheFlag(t *testing.T) {
	f := newLateJoinFixture(t, 4)
	f.sharesSpectatorChat("mira")
	if sam := f.addLocal("mira", "sam"); !sam.SpectatorChat {
		t.Fatal("a local player of a client that shares spectator chat should too")
	}
}
