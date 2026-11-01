package ws

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/jay-day/rmgk-lobby/internal/moderation"
	"github.com/jay-day/rmgk-lobby/internal/protocol"
	"github.com/jay-day/rmgk-lobby/internal/state"
)

var testRom = protocol.RomInfo{Name: "rom", MD5: "ABC"}

func (f *lateJoinFixture) socialUser(name string) *state.User {
	u := f.capableUser(name)
	u.Social = true
	f.h.clients[u.ID] = f.client(name) // accepting checks the sender is still connected
	return u
}

// payloadOf decodes the last envelope of msgType, failing the test if there's none.
func payloadOf[T any](t *testing.T, envs []protocol.Envelope, msgType string) T {
	t.Helper()
	var out T
	found := false
	for _, env := range envs {
		if env.Type == msgType {
			if err := json.Unmarshal(env.Data, &out); err != nil {
				t.Fatalf("bad %s: %v", msgType, err)
			}
			found = true
		}
	}
	if !found {
		t.Fatalf("no %s in %+v", msgType, envs)
	}
	return out
}

func (f *lateJoinFixture) challenge(from, to string) protocol.ChallengeIncomingPayload {
	f.t.Helper()
	f.send(from, protocol.TypeChallengeSend, protocol.ChallengeSendRequest{
		ToUserID: f.users[to].ID, Rom: testRom, Note: "bo5?", LiveReplay: true,
	})
	sent := payloadOf[protocol.ChallengeSentPayload](f.t, f.drain(from), protocol.TypeChallengeSent)
	if sent.ChallengeID == 0 || sent.ToUsername != to {
		f.t.Fatalf("bad ack: %+v", sent)
	}
	owner := f.users[to].Primary().Username
	return payloadOf[protocol.ChallengeIncomingPayload](f.t, f.drain(owner), protocol.TypeChallengeIncoming)
}

func (f *lateJoinFixture) respond(name string, id uint64, accept bool) {
	f.t.Helper()
	f.send(name, protocol.TypeChallengeRespond, protocol.ChallengeRespondRequest{ChallengeID: id, Accept: accept})
}

func (f *lateJoinFixture) refusedWith(from, to, want string) {
	f.t.Helper()
	f.drain(from)
	f.send(from, protocol.TypeChallengeSend, protocol.ChallengeSendRequest{ToUserID: f.users[to].ID, Rom: testRom})
	got := payloadOf[protocol.ChallengeResultPayload](f.t, f.drain(from), protocol.TypeChallengeResult)
	if got.Result != want {
		f.t.Fatalf("challenge %s→%s: result %q, want %q", from, to, got.Result, want)
	}
}

func TestChallengeAcceptMakesAPrivateRoomForBoth(t *testing.T) {
	f := newLateJoinFixture(t, 4)
	mira, kart := f.socialUser("mira"), f.socialUser("kart")
	in := f.challenge("mira", "kart")
	if in.FromUsername != "mira" || in.Note != "bo5?" || in.Rom.MD5 != "ABC" || in.Invite || in.ExpiresInMs <= 0 {
		t.Fatalf("bad incoming: %+v", in)
	}

	f.respond("kart", in.ChallengeID, true)

	room := mira.CurrentRoom
	if room == nil || kart.CurrentRoom != room {
		t.Fatalf("not both seated: mira=%v kart=%v", mira.CurrentRoom, kart.CurrentRoom)
	}
	if room.Host != mira || !room.Private || room.MaxPlayers != 2 || !room.LiveReplayEnabled || room.Name != "mira vs kart" {
		t.Fatalf("bad room: %+v", room)
	}
	for _, name := range []string{"mira", "kart"} {
		envs := f.drain(name)
		state := roomStateOf(t, envs)
		if !state.Private || state.HostID != mira.ID || len(state.Players) != 2 {
			t.Fatalf("%s: bad room state %+v", name, state)
		}
		if _, ok := findEnvelope(envs, protocol.TypeRoomJoinOK); !ok {
			t.Fatalf("%s: no ROOM_JOIN_OK", name)
		}
		if got := payloadOf[protocol.ChallengeResultPayload](t, envs, protocol.TypeChallengeResult); got.Result != protocol.ChallengeAccepted {
			t.Fatalf("%s: result %q", name, got.Result)
		}
	}
}

func TestPrivateRoomRefusesOutsiders(t *testing.T) {
	f := newLateJoinFixture(t, 4)
	mira := f.socialUser("mira")
	f.socialUser("kart")
	f.socialUser("nosy")
	in := f.challenge("mira", "kart")
	f.respond("kart", in.ChallengeID, true)
	room := mira.CurrentRoom

	// Even a seat someone left stays closed.
	f.send("kart", protocol.TypeRoomLeave, struct{}{})
	f.drain("nosy")
	f.send("nosy", protocol.TypeRoomJoin, protocol.RoomJoinRequest{RoomID: room.ID})
	if got := reasonOf(t, f.drain("nosy"), protocol.TypeRoomJoinFail); got != protocol.ReasonPrivateRoom {
		t.Fatalf("reason %q", got)
	}
}

func TestChallengeSeatsEveryoneOnBothPCs(t *testing.T) {
	f := newLateJoinFixture(t, 4)
	mira := f.socialUser("mira")
	sam := f.addLocal("mira", "sam")
	kart := f.socialUser("kart")
	f.drain("mira")

	// Challenging a local player reaches the owner's connection and seats both.
	in := f.challenge("kart", "sam")
	if in.ToUsername != "sam" {
		t.Fatalf("bad incoming: %+v", in)
	}
	f.respond("mira", in.ChallengeID, true)
	room := kart.CurrentRoom
	if room == nil || mira.CurrentRoom != room || sam.CurrentRoom != room || room.MaxPlayers != 3 {
		t.Fatalf("bad seating: room=%+v", room)
	}
	if room.Name != "kart vs sam" {
		t.Fatalf("room name %q", room.Name)
	}
}

func TestChallengeThatWouldNotFitDoesNotSayWhy(t *testing.T) {
	f := newLateJoinFixture(t, 4)
	f.socialUser("mira")
	f.addLocal("mira", "sam")
	f.addLocal("mira", "lee")
	f.socialUser("kart")
	f.addLocal("kart", "kai")

	f.refusedWith("mira", "kart", protocol.ChallengeUnavailable)
	f.refusedWith("kart", "sam", protocol.ChallengeUnavailable)
}

func TestChallengeDeclineStartsACooldown(t *testing.T) {
	f := newLateJoinFixture(t, 4)
	f.socialUser("mira")
	f.socialUser("kart")
	in := f.challenge("mira", "kart")

	f.respond("kart", in.ChallengeID, false)

	if got := payloadOf[protocol.ChallengeResultPayload](t, f.drain("mira"), protocol.TypeChallengeResult); got.Result != protocol.ChallengeDeclined {
		t.Fatalf("result %q", got.Result)
	}
	if f.users["mira"].CurrentRoom != nil {
		t.Fatal("declining made a room")
	}
	f.refusedWith("mira", "kart", protocol.ChallengeCooldown)
}

func TestChallengeCooldownIsPerPlayerAsked(t *testing.T) {
	f := newLateJoinFixture(t, 4)
	f.socialUser("mira")
	f.addLocal("mira", "sam")
	f.socialUser("kart")
	f.drain("mira")

	in := f.challenge("kart", "sam")
	f.respond("mira", in.ChallengeID, false)
	f.drain("kart")

	// Saying no for one name doesn't show on the other: that would give away the shared PC.
	again := f.challenge("kart", "mira")
	f.send("kart", protocol.TypeChallengeCancel, protocol.ChallengeCancelRequest{ChallengeID: again.ChallengeID})
	f.refusedWith("kart", "sam", protocol.ChallengeCooldown)
}

func TestChallengeClipsWhatItPassesOn(t *testing.T) {
	f := newLateJoinFixture(t, 4)
	f.socialUser("mira")
	kart := f.socialUser("kart")
	huge := strings.Repeat("é", 1<<19) // about the most one message can hold

	start := time.Now()
	f.send("mira", protocol.TypeChallengeSend, protocol.ChallengeSendRequest{
		ToUserID: kart.ID, Rom: protocol.RomInfo{Name: huge, MD5: huge, Region: huge}, Note: huge,
	})
	if took := time.Since(start); took > time.Second {
		t.Fatalf("took %v", took)
	}
	in := payloadOf[protocol.ChallengeIncomingPayload](t, f.drain("kart"), protocol.TypeChallengeIncoming)
	if utf8.RuneCountInString(in.Note) != challengeNoteMaxRunes || utf8.RuneCountInString(in.Rom.Name) != romNameMaxRunes ||
		utf8.RuneCountInString(in.Rom.MD5) != romFieldMaxRunes || utf8.RuneCountInString(in.Rom.Region) != romFieldMaxRunes {
		t.Fatalf("note %d, name %d, md5 %d, region %d", utf8.RuneCountInString(in.Note), utf8.RuneCountInString(in.Rom.Name),
			utf8.RuneCountInString(in.Rom.MD5), utf8.RuneCountInString(in.Rom.Region))
	}
}

func TestChallengeGameNameObeysTheFilter(t *testing.T) {
	f := newLateJoinFixture(t, 4)
	f.h.mod = moderation.NewManager(moderation.NewFilter([]string{"badword"}), &moderation.AdminStore{}, "")
	f.socialUser("mira")
	kart := f.socialUser("kart")

	f.send("mira", protocol.TypeChallengeSend, protocol.ChallengeSendRequest{
		ToUserID: kart.ID, Rom: protocol.RomInfo{Name: "a badword game", MD5: "ABC"},
	})
	if got := payloadOf[protocol.ChallengeResultPayload](t, f.drain("mira"), protocol.TypeChallengeResult); got.Result != protocol.ChallengeUnavailable {
		t.Fatalf("result %q", got.Result)
	}
	if envs := f.drain("kart"); len(envs) != 0 {
		t.Fatalf("it reached them: %+v", envs)
	}
}

func TestAcceptFromSomeoneWhoJustLeftMakesNoRoom(t *testing.T) {
	f := newLateJoinFixture(t, 4)
	mira, kart := f.socialUser("mira"), f.socialUser("kart")
	in := f.challenge("mira", "kart")

	// Their cleanup ran, and had no room to clear yet.
	delete(f.h.clients, mira.ID)
	f.respond("kart", in.ChallengeID, true)

	if mira.CurrentRoom != nil || kart.CurrentRoom != nil {
		t.Fatalf("seated: mira=%v kart=%v", mira.CurrentRoom, kart.CurrentRoom)
	}
	if rooms := f.h.state.ListRooms(); len(rooms) != 0 {
		t.Fatalf("left a room behind: %+v", rooms)
	}
	envs := f.drain("kart")
	if got := payloadOf[protocol.ChallengeResultPayload](t, envs, protocol.TypeChallengeResult); got.Result != protocol.ChallengeUnavailable {
		t.Fatalf("result %q", got.Result)
	}
	if _, ok := findEnvelope(envs, protocol.TypeRoomJoinOK); ok {
		t.Fatal("told they'd joined")
	}
}

func TestChallengeRefusals(t *testing.T) {
	f := newLateJoinFixture(t, 4)
	mira := f.socialUser("mira")
	sam := f.addLocal("mira", "sam")
	f.socialUser("kart")
	f.socialUser("lee")
	f.socialUser("playing")
	f.socialUser("buddy")
	f.user("oldclient", true)
	noGame := f.socialUser("nogame")
	noGame.RomHashes = romHashSet([]string{"def"})
	hasGame := f.socialUser("hasgame")
	hasGame.RomHashes = romHashSet([]string{"abc"})
	f.startMatch(2, "playing", "buddy")
	_ = mira

	f.refusedWith("mira", "sam", protocol.ChallengeUnavailable) // same PC
	_ = sam
	f.refusedWith("mira", "oldclient", protocol.ReasonNeedsUpdate)
	f.refusedWith("mira", "playing", protocol.ChallengeBusy)
	f.refusedWith("mira", "nogame", protocol.ChallengeNoGame)

	// One out at a time, and hashes compare case-insensitively.
	f.challenge("mira", "hasgame")
	f.refusedWith("mira", "kart", protocol.ChallengePending)
}

func TestChallengeTimesOut(t *testing.T) {
	saved := challengeAnswerTime
	challengeAnswerTime = 20 * time.Millisecond
	defer func() { challengeAnswerTime = saved }()

	f := newLateJoinFixture(t, 4)
	f.socialUser("mira")
	f.socialUser("kart")
	in := f.challenge("mira", "kart")

	time.Sleep(100 * time.Millisecond)
	for _, name := range []string{"mira", "kart"} {
		if got := payloadOf[protocol.ChallengeResultPayload](t, f.drain(name), protocol.TypeChallengeResult); got.Result != protocol.ChallengeTimedOut {
			t.Fatalf("%s: result %q", name, got.Result)
		}
	}
	// Answering too late does nothing.
	f.respond("kart", in.ChallengeID, true)
	if f.users["kart"].CurrentRoom != nil {
		t.Fatal("late answer made a room")
	}
}

func TestChallengeCancelTellsTheRecipient(t *testing.T) {
	f := newLateJoinFixture(t, 4)
	f.socialUser("mira")
	f.socialUser("kart")
	in := f.challenge("mira", "kart")

	f.send("kart", protocol.TypeChallengeCancel, protocol.ChallengeCancelRequest{ChallengeID: in.ChallengeID})
	if envs := f.drain("mira"); len(envs) != 0 {
		t.Fatalf("recipient cancelled someone else's challenge: %+v", envs)
	}
	f.send("mira", protocol.TypeChallengeCancel, protocol.ChallengeCancelRequest{ChallengeID: in.ChallengeID})
	if got := payloadOf[protocol.ChallengeResultPayload](t, f.drain("kart"), protocol.TypeChallengeResult); got.Result != protocol.ChallengeCancelled {
		t.Fatalf("result %q", got.Result)
	}
}

func TestAcceptingLeavesYourRoom(t *testing.T) {
	f := newLateJoinFixture(t, 4)
	f.socialUser("mira")
	kart := f.socialUser("kart")
	f.socialUser("other")
	old := f.createRoom("other", 4)
	f.send("kart", protocol.TypeRoomJoin, protocol.RoomJoinRequest{RoomID: old.ID})
	if kart.CurrentRoom != old {
		t.Fatalf("kart didn't join: %+v", f.drain("kart"))
	}
	f.drain("kart")

	in := f.challenge("mira", "kart")
	f.respond("kart", in.ChallengeID, true)

	if kart.CurrentRoom == nil || kart.CurrentRoom == old {
		t.Fatalf("kart's room: %+v", kart.CurrentRoom)
	}
	if got := len(old.Players); got != 1 {
		t.Fatalf("old room seats %d", got)
	}
}

func TestStartingAMatchEndsChallenges(t *testing.T) {
	f := newLateJoinFixture(t, 4)
	f.socialUser("mira")
	f.socialUser("kart")
	f.socialUser("buddy")
	in := f.challenge("mira", "kart")

	room := f.createRoom("kart", 2)
	f.send("buddy", protocol.TypeRoomJoin, protocol.RoomJoinRequest{RoomID: room.ID})
	f.drain("mira")
	f.send("kart", protocol.TypeRoomStart, struct{}{})

	if got := payloadOf[protocol.ChallengeResultPayload](t, f.drain("mira"), protocol.TypeChallengeResult); got.Result != protocol.ChallengeBusy || got.ChallengeID != in.ChallengeID {
		t.Fatalf("result %+v", got)
	}
}

func TestInviteJoinsTheSendersRoomPastItsPassword(t *testing.T) {
	f := newLateJoinFixture(t, 4)
	mira := f.socialUser("mira")
	kart := f.socialUser("kart")
	f.send("mira", protocol.TypeRoomCreate, protocol.RoomCreateRequest{
		Name: "room", Rom: testRom, MaxPlayers: 4, Prediction: 7, Password: "secret",
	})
	room := mira.CurrentRoom
	f.drain("mira")

	f.send("mira", protocol.TypeChallengeSend, protocol.ChallengeSendRequest{ToUserID: kart.ID})
	if sent := payloadOf[protocol.ChallengeSentPayload](t, f.drain("mira"), protocol.TypeChallengeSent); !sent.Invite {
		t.Fatalf("not an invite: %+v", sent)
	}
	in := payloadOf[protocol.ChallengeIncomingPayload](t, f.drain("kart"), protocol.TypeChallengeIncoming)
	if !in.Invite || in.RoomName != "room" || in.RoomPlayers != 1 || in.RoomMax != 4 || in.Rom.MD5 != "ABC" {
		t.Fatalf("bad invite: %+v", in)
	}

	f.respond("kart", in.ChallengeID, true)
	if kart.CurrentRoom != room {
		t.Fatalf("kart's room: %+v", kart.CurrentRoom)
	}
}

func TestInviteRefusedWhenTheyWereKicked(t *testing.T) {
	f := newLateJoinFixture(t, 4)
	mira := f.socialUser("mira")
	kart := f.socialUser("kart")
	room := f.createRoom("mira", 4)
	f.send("kart", protocol.TypeRoomJoin, protocol.RoomJoinRequest{RoomID: room.ID})
	f.send("mira", protocol.TypeRoomKick, protocol.RoomKickRequest{UserID: kart.ID})
	if kart.CurrentRoom != nil || mira.CurrentRoom != room {
		t.Fatal("kick didn't land")
	}
	f.refusedWith("mira", "kart", protocol.ChallengeUnavailable)
}

func TestChallengeGamesComparesOnlyTheAskersList(t *testing.T) {
	f := newLateJoinFixture(t, 4)
	mira := f.socialUser("mira")
	sam := f.addLocal("mira", "sam")
	kart := f.socialUser("kart")
	mira.RomHashes = romHashSet([]string{"aaa", "bbb"})
	kart.RomHashes = romHashSet([]string{"BBB", "CCC"})
	f.drain("kart")

	f.send("kart", protocol.TypeChallengeGames, protocol.ChallengeGamesRequest{ToUserID: sam.ID})
	got := payloadOf[protocol.ChallengeGamesReply](t, f.drain("kart"), protocol.TypeChallengeGamesReply)
	if len(got.MD5s) != 1 || got.MD5s[0] != "BBB" {
		t.Fatalf("games %+v", got)
	}
}

func (f *lateJoinFixture) pm(from string, req protocol.ChatSendRequest) {
	f.t.Helper()
	req.Channel = channelPM
	f.send(from, protocol.TypeChatSend, req)
}

func TestPrivateMessageReachesOnlyTheTwoOfThem(t *testing.T) {
	f := newLateJoinFixture(t, 4)
	mira := f.socialUser("mira")
	kart := f.socialUser("kart")
	f.socialUser("other")
	f.h.clients[mira.ID] = f.client("mira")
	f.h.clients[kart.ID] = f.client("kart")

	f.pm("mira", protocol.ChatSendRequest{ToUserID: kart.ID, Message: "  hi there "})

	for _, name := range []string{"mira", "kart"} {
		got := payloadOf[protocol.ChatMsgPayload](t, f.drain(name), protocol.TypeChatMsg)
		if got.Channel != channelPM || got.FromUserID != mira.ID || got.FromUsername != "mira" ||
			got.ToUserID != kart.ID || got.ToUsername != "kart" || got.Message != "hi there" {
			t.Fatalf("%s got %+v", name, got)
		}
	}
	if envs := f.drain("other"); len(envs) != 0 {
		t.Fatalf("a third player saw it: %+v", envs)
	}
	if n := len(f.h.lobbyChat.Snapshot()); n != 0 {
		t.Fatalf("stored %d messages", n)
	}
}

func TestPrivateMessageToALocalPlayerIsAnsweredAsThem(t *testing.T) {
	f := newLateJoinFixture(t, 4)
	f.socialUser("mira")
	sam := f.addLocal("mira", "sam")
	kart := f.socialUser("kart")
	f.drain("mira")

	f.pm("kart", protocol.ChatSendRequest{ToUserID: sam.ID, Message: "hey sam"})
	got := payloadOf[protocol.ChatMsgPayload](t, f.drain("mira"), protocol.TypeChatMsg)
	if got.ToUserID != sam.ID || got.ToUsername != "sam" {
		t.Fatalf("mira's connection got %+v", got)
	}
	f.drain("kart")

	f.pm("mira", protocol.ChatSendRequest{ToUserID: kart.ID, AsUserID: sam.ID, Message: "yo"})
	got = payloadOf[protocol.ChatMsgPayload](t, f.drain("kart"), protocol.TypeChatMsg)
	if got.FromUserID != sam.ID || got.FromUsername != "sam" {
		t.Fatalf("kart got %+v", got)
	}

	// Nobody can talk as someone on another PC.
	f.drain("mira")
	f.pm("kart", protocol.ChatSendRequest{ToUserID: f.users["mira"].ID, AsUserID: sam.ID, Message: "spoof"})
	if envs := f.drain("mira"); len(envs) != 0 {
		t.Fatalf("spoofed: %+v", envs)
	}
}

func TestPrivateMessageAsSomeoneWhoLeftThisPCFails(t *testing.T) {
	f := newLateJoinFixture(t, 4)
	f.socialUser("mira")
	sam := f.addLocal("mira", "sam")
	kart := f.socialUser("kart")
	f.send("mira", protocol.TypeLocalPlayerRemove, protocol.LocalPlayerRemoveRequest{UserID: sam.ID})
	f.drain("mira")
	f.drain("kart")

	f.pm("mira", protocol.ChatSendRequest{ToUserID: kart.ID, AsUserID: sam.ID, Message: "hi"})

	if got := payloadOf[protocol.ChatFailPayload](t, f.drain("mira"), protocol.TypeChatFail); got.ToUserID != kart.ID {
		t.Fatalf("fail %+v", got)
	}
	if envs := f.drain("kart"); len(envs) != 0 {
		t.Fatalf("it reached them: %+v", envs)
	}
}

func TestShieldIsTheModeratorsNotTheirGuests(t *testing.T) {
	f := newLateJoinFixture(t, 4)
	f.socialUser("mod")
	sam := f.addLocal("mod", "sam")
	kart := f.socialUser("kart")
	c := f.client("mod")
	c.isModerator = true

	send := func(as uint64) protocol.ChatMsgPayload {
		t.Helper()
		data, _ := json.Marshal(protocol.ChatSendRequest{Channel: channelPM, ToUserID: kart.ID, AsUserID: as, Message: "hi"})
		f.h.dispatch(c, protocol.Envelope{Type: protocol.TypeChatSend, Data: data})
		return payloadOf[protocol.ChatMsgPayload](t, f.drain("kart"), protocol.TypeChatMsg)
	}
	if got := send(sam.ID); got.Admin {
		t.Fatalf("a guest got the shield: %+v", got)
	}
	if got := send(0); !got.Admin {
		t.Fatalf("the moderator lost it: %+v", got)
	}
}

func TestPrivateMessageRefusals(t *testing.T) {
	f := newLateJoinFixture(t, 4)
	f.socialUser("mira")
	sam := f.addLocal("mira", "sam")
	old := f.user("oldclient", true)
	f.drain("mira")

	refused := func(to uint64, want string) {
		t.Helper()
		f.pm("mira", protocol.ChatSendRequest{ToUserID: to, Message: "hi"})
		got := payloadOf[protocol.ChatFailPayload](t, f.drain("mira"), protocol.TypeChatFail)
		if got.Reason != want || got.ToUserID != to {
			t.Fatalf("got %+v, want %q", got, want)
		}
	}
	refused(sam.ID, protocol.ReasonSamePC)
	refused(old.ID, protocol.ReasonNeedsUpdate)
	refused(99999, protocol.ReasonUserGone)
}

func TestPrivateMessagesObeyMuteAndFilter(t *testing.T) {
	f := newLateJoinFixture(t, 4)
	f.h.mod = moderation.NewManager(moderation.NewFilter([]string{"badword"}), &moderation.AdminStore{}, "")
	f.socialUser("mira")
	kart := f.socialUser("kart")

	send := func() *Client {
		c := f.client("mira")
		c.ip = "198.51.100.1"
		return c
	}
	raw := func(msg string) protocol.Envelope {
		data, _ := json.Marshal(protocol.ChatSendRequest{Channel: channelPM, ToUserID: kart.ID, Message: msg})
		return protocol.Envelope{Type: protocol.TypeChatSend, Data: data}
	}

	f.h.dispatch(send(), raw("a badword here"))
	if envs := f.drain("kart"); len(envs) != 0 {
		t.Fatalf("filter let it through: %+v", envs)
	}
	f.h.mod.Mute("198.51.100.1", "mira", "mod", "", 0)
	f.h.dispatch(send(), raw("hello"))
	if envs := f.drain("kart"); len(envs) != 0 {
		t.Fatalf("mute let it through: %+v", envs)
	}
	notices := payloadOf[protocol.ModNoticePayload](t, f.drain("mira"), protocol.TypeModNotice)
	if !strings.Contains(notices.Text, "muted") {
		t.Fatalf("notice %q", notices.Text)
	}
}

func TestPresenceSaysWhoCanReceiveThese(t *testing.T) {
	f := newLateJoinFixture(t, 4)
	f.socialUser("mira")
	sam := f.addLocal("mira", "sam")
	old := f.user("oldclient", true)
	if !f.h.presenceUserFromState(sam).Social {
		t.Fatal("a local player of a social client should be social")
	}
	if f.h.presenceUserFromState(old).Social {
		t.Fatal("an old client isn't social")
	}
}

func TestModPeersFollowTheRoster(t *testing.T) {
	f := newLateJoinFixture(t, 4)
	mod := f.socialUser("mod")
	c := f.h.clients[mod.ID]
	c.isModerator = true
	roster := func(msgType string, data any) []uint64 {
		t.Helper()
		f.drain("mod")
		raw, _ := json.Marshal(data)
		f.h.dispatch(c, protocol.Envelope{Type: msgType, Data: raw})
		return payloadOf[protocol.ModPeersPayload](t, f.drain("mod"), protocol.TypeModPeers).UserIDs
	}

	if got := roster(protocol.TypeLocalPlayerAdd, protocol.LocalPlayerAddRequest{Name: "sam"}); len(got) != 2 {
		t.Fatalf("after adding: %v", got)
	}
	sam := f.h.state.GetUserByName("sam")
	if got := roster(protocol.TypeLocalPlayerRemove, protocol.LocalPlayerRemoveRequest{UserID: sam.ID}); len(got) != 1 || got[0] != mod.ID {
		t.Fatalf("after removing: %v", got)
	}
}

func TestInspectAndResultForModerators(t *testing.T) {
	f := newLateJoinFixture(t, 4)
	f.h.mod = moderation.NewManager(moderation.NewFilter(nil), &moderation.AdminStore{}, "")
	mod := f.socialUser("mod")
	target := f.socialUser("mira")
	f.addLocal("mira", "sam")
	f.h.clients[mod.ID] = f.client("mod")
	targetClient := f.client("mira")
	targetClient.ip = "198.51.100.2"
	f.h.clients[target.ID] = targetClient
	f.drain("mira")

	modClient := f.h.clients[mod.ID]
	modClient.isModerator = true
	modClient.adminName = "Mod"
	act := func(action, who, dur string) []protocol.Envelope {
		t.Helper()
		data, _ := json.Marshal(protocol.ModActionRequest{Action: action, Target: who, Duration: dur, Reason: "spam"})
		f.h.dispatch(modClient, protocol.Envelope{Type: protocol.TypeModAction, Data: data})
		return f.drain("mod")
	}

	// Names the local player that was asked about, and who else goes with them.
	info := payloadOf[protocol.ModTargetInfoPayload](t, act("inspect", "SAM", ""), protocol.TypeModTargetInfo)
	if info.Target != "sam" || len(info.Others) != 1 || info.Others[0] != "mira" || info.Moderator {
		t.Fatalf("info %+v", info)
	}

	res := payloadOf[protocol.ModResultPayload](t, act("mute", "sam", "1h"), protocol.TypeModResult)
	if res.Action != "mute" || res.Target != "sam" || res.IP != "198.51.100.2" || res.Until == 0 {
		t.Fatalf("result %+v", res)
	}

	list := payloadOf[protocol.ModListReply](t, act("list", "", ""), protocol.TypeModList)
	if len(list.Mutes) != 1 || list.Mutes[0].Name != "sam" || list.Mutes[0].By != "Mod" {
		t.Fatalf("list %+v", list)
	}
}

func TestModPeersReachOnlyModerators(t *testing.T) {
	f := newLateJoinFixture(t, 4)
	f.h.mod = moderation.NewManager(moderation.NewFilter(nil), &moderation.AdminStore{}, "")
	mod := f.socialUser("mod")
	sam := f.addLocal("mod", "sam")
	other := f.socialUser("other")
	modClient, otherClient := f.client("mod"), f.client("other")
	f.h.clients[mod.ID] = modClient
	f.h.clients[other.ID] = otherClient
	modClient.isModerator = true
	f.drain("mod")

	f.h.broadcastModPeers()

	peers := payloadOf[protocol.ModPeersPayload](t, f.drain("mod"), protocol.TypeModPeers)
	if len(peers.UserIDs) != 2 || peers.UserIDs[0] != mod.ID || peers.UserIDs[1] != sam.ID {
		t.Fatalf("peers %+v", peers)
	}
	if envs := f.drain("other"); len(envs) != 0 {
		t.Fatalf("a player learned who the moderators are: %+v", envs)
	}

	f.h.handleAdminLogout(modClient, protocol.Envelope{})
	if modClient.isModerator {
		t.Fatal("logout left the role on")
	}
	if _, ok := findEnvelope(f.drain("mod"), protocol.TypeAdminLoggedOut); !ok {
		t.Fatal("no ADMIN_LOGGED_OUT")
	}
}
