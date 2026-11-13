package ws

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/jay-day/rmgk-lobby/internal/protocol"
	"github.com/jay-day/rmgk-lobby/internal/state"
)

// pmRefused sends from → to and expects the lobby to refuse it with reason.
func (f *lateJoinFixture) pmRefused(from string, to *state.User, reason string) {
	f.t.Helper()
	f.drain(from)
	f.pm(from, protocol.ChatSendRequest{ToUserID: to.ID, Message: "hi"})
	got := payloadOf[protocol.ChatFailPayload](f.t, f.drain(from), protocol.TypeChatFail)
	if got.Reason != reason || got.ToUserID != to.ID {
		f.t.Fatalf("%s → %s: got %+v, want %q", from, to.Username, got, reason)
	}
}

// pmDelivered sends from → to and expects it to arrive on to's PC.
func (f *lateJoinFixture) pmDelivered(from string, to *state.User, toPC string) {
	f.t.Helper()
	f.drain(toPC)
	f.pm(from, protocol.ChatSendRequest{ToUserID: to.ID, Message: "hi"})
	got := payloadOf[protocol.ChatMsgPayload](f.t, f.drain(toPC), protocol.TypeChatMsg)
	if got.ToUserID != to.ID {
		f.t.Fatalf("%s → %s: %s's PC got %+v", from, to.Username, toPC, got)
	}
	f.drain(from)
}

func TestPrivateMessagesOffRefusesThemAndShowsInPresence(t *testing.T) {
	f := newLateJoinFixture(t, 4)
	f.socialUser("mira")
	kart := f.socialUser("kart")
	sam := f.addLocal("kart", "sam")
	f.drain("kart")

	f.send("kart", protocol.TypePMSettings, protocol.PMSettingsRequest{Off: true})
	if !f.h.presenceUserFromState(kart).NoPrivateMessages {
		t.Fatal("presence doesn't say kart takes no private messages")
	}
	// The whole PC: a local player follows its owner.
	if !f.h.presenceUserFromState(sam).NoPrivateMessages {
		t.Fatal("presence doesn't say sam takes no private messages")
	}
	// Everyone hears, so their clients can stop offering it.
	if got := payloadOf[protocol.PresenceDeltaPayload](t, f.drain("mira"), protocol.TypePresenceDelta); len(got.Updated) == 0 {
		t.Fatalf("no presence update: %+v", got)
	}
	f.drain("kart")

	f.pmRefused("mira", kart, protocol.ReasonPMsOff)
	f.pmRefused("mira", sam, protocol.ReasonPMsOff)
	if envs := f.drain("kart"); len(envs) != 0 {
		t.Fatalf("it reached kart's PC: %+v", envs)
	}

	f.send("kart", protocol.TypePMSettings, protocol.PMSettingsRequest{Off: false})
	if f.h.presenceUserFromState(kart).NoPrivateMessages {
		t.Fatal("still off after turning private messages back on")
	}
	f.pmDelivered("mira", kart, "kart")
}

func TestPrivateMessagesOffStillTakesReplies(t *testing.T) {
	f := newLateJoinFixture(t, 4)
	mira := f.socialUser("mira")
	kart := f.socialUser("kart")
	other := f.socialUser("other")
	f.send("kart", protocol.TypePMSettings, protocol.PMSettingsRequest{Off: true})

	// kart writes first, so mira can answer; someone else still can't.
	f.pmDelivered("kart", mira, "mira")
	f.pmDelivered("mira", kart, "kart")
	f.pmRefused("other", kart, protocol.ReasonPMsOff)

	// kart having PMs off doesn't stop kart writing to anyone.
	f.pmDelivered("kart", other, "other")

	// Once the reply window has passed, mira is refused like anyone else.
	kart.NotePrivateMessageSent(mira.ID, time.Now().Add(-state.PrivateMessageReplyWindow-time.Second))
	f.pmRefused("mira", kart, protocol.ReasonPMsOff)
}

func TestPrivateMessagesOffRepliesBelongToWhoeverWasWrittenTo(t *testing.T) {
	f := newLateJoinFixture(t, 4)
	f.socialUser("mira")
	sam := f.addLocal("mira", "sam")
	kart := f.socialUser("kart")
	f.send("kart", protocol.TypePMSettings, protocol.PMSettingsRequest{Off: true})

	// kart wrote to sam, mira's guest: sam can answer, mira herself can't.
	f.pmDelivered("kart", sam, "mira")
	f.pm("mira", protocol.ChatSendRequest{ToUserID: kart.ID, AsUserID: sam.ID, Message: "yo"})
	if got := payloadOf[protocol.ChatMsgPayload](t, f.drain("kart"), protocol.TypeChatMsg); got.FromUserID != sam.ID {
		t.Fatalf("sam's reply: %+v", got)
	}
	f.pmRefused("mira", kart, protocol.ReasonPMsOff)
}

func TestModeratorsReachPrivateMessagesOff(t *testing.T) {
	f := newLateJoinFixture(t, 4)
	f.socialUser("mod")
	sam := f.addLocal("mod", "sam")
	kart := f.socialUser("kart")
	f.send("kart", protocol.TypePMSettings, protocol.PMSettingsRequest{Off: true})
	f.drain("kart")
	c := f.client("mod")
	c.isModerator = true

	send := func(as uint64) []protocol.Envelope {
		t.Helper()
		data, _ := json.Marshal(protocol.ChatSendRequest{Channel: channelPM, ToUserID: kart.ID, AsUserID: as, Message: "hi"})
		f.h.dispatch(c, protocol.Envelope{Type: protocol.TypeChatSend, Data: data})
		return f.drain("kart")
	}
	if got := payloadOf[protocol.ChatMsgPayload](t, send(0), protocol.TypeChatMsg); !got.Admin {
		t.Fatalf("the moderator's message: %+v", got)
	}
	// Their guests aren't moderators.
	if envs := send(sam.ID); len(envs) != 0 {
		t.Fatalf("a moderator's guest got through: %+v", envs)
	}
}
