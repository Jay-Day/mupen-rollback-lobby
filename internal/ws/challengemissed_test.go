package ws

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jay-day/rmgk-lobby/internal/moderation"
	"github.com/jay-day/rmgk-lobby/internal/protocol"
	"github.com/jay-day/rmgk-lobby/internal/state"
)

// logsChallenges adds a social user whose client sent challengeLog in HELLO.
func (f *lateJoinFixture) logsChallenges(name string) *state.User {
	u := f.socialUser(name)
	u.ChallengeLog = true
	return u
}

// challengeFrom sends a CHALLENGE_SEND on c and returns everything c got back.
func (f *lateJoinFixture) challengeFrom(c *Client, req protocol.ChallengeSendRequest) []protocol.Envelope {
	f.t.Helper()
	f.drain(c.user.Username)
	raw, err := json.Marshal(req)
	if err != nil {
		f.t.Fatalf("json.Marshal: %v", err)
	}
	f.h.dispatch(c, protocol.Envelope{Type: protocol.TypeChallengeSend, Data: raw})
	return f.drain(c.user.Username)
}

// toldBusy sends a challenge from one user to another and checks that the
// sender got the plain busy refusal and nothing else.
func (f *lateJoinFixture) toldBusy(from string, to *state.User, req protocol.ChallengeSendRequest) {
	f.t.Helper()
	req.ToUserID = to.ID
	onlyBusy(f.t, f.challengeFrom(f.client(from), req), to.ID)
}

// onlyBusy checks that envs is exactly the busy refusal of a challenge to player
// to, byte for byte, so nothing about the target's PC can ride along with it.
func onlyBusy(t *testing.T, envs []protocol.Envelope, to uint64) {
	t.Helper()
	if len(envs) != 1 || envs[0].Type != protocol.TypeChallengeResult {
		t.Fatalf("sender got %+v, want only a CHALLENGE_RESULT", envs)
	}
	if want := fmt.Sprintf(`{"toUserId":%d,"result":"busy"}`, to); string(envs[0].Data) != want {
		t.Fatalf("sender got %s, want %s", envs[0].Data, want)
	}
}

// missedBy returns the CHALLENGE_MISSED messages queued for name, failing on
// anything else challenge-related.
func (f *lateJoinFixture) missedBy(name string) []protocol.ChallengeMissedPayload {
	f.t.Helper()
	var out []protocol.ChallengeMissedPayload
	for _, env := range f.drain(name) {
		switch env.Type {
		case protocol.TypeChallengeMissed:
			var m protocol.ChallengeMissedPayload
			if err := json.Unmarshal(env.Data, &m); err != nil {
				f.t.Fatalf("bad CHALLENGE_MISSED: %v", err)
			}
			out = append(out, m)
		case protocol.TypeChallengeIncoming, protocol.TypeChallengeResult:
			f.t.Fatalf("%s got %s %s", name, env.Type, env.Data)
		}
	}
	return out
}

func (f *lateJoinFixture) heardOneMissed(name string) protocol.ChallengeMissedPayload {
	f.t.Helper()
	got := f.missedBy(name)
	if len(got) != 1 {
		f.t.Fatalf("%s: CHALLENGE_MISSED %+v, want one", name, got)
	}
	return got[0]
}

func (f *lateJoinFixture) heardNoMissed(name string) {
	f.t.Helper()
	if got := f.missedBy(name); len(got) != 0 {
		f.t.Fatalf("%s: CHALLENGE_MISSED %+v, want none", name, got)
	}
}

// rewind moves a recorded time back by d, as if it had happened that long ago.
func (f *lateJoinFixture) rewind(times map[[2]uint64]time.Time, from, to uint64, d time.Duration) {
	f.h.challenges.mu.Lock()
	defer f.h.challenges.mu.Unlock()
	key := [2]uint64{from, to}
	if at, ok := times[key]; ok {
		times[key] = at.Add(-d)
	}
}

func jsonKeys(t *testing.T, raw []byte) string {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("bad JSON %s: %v", raw, err)
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return strings.Join(keys, ",")
}

func TestChallengeMissedReachesAPlayerInAMatch(t *testing.T) {
	f := newLateJoinFixture(t, 4)
	mira := f.socialUser("mira")
	kart := f.logsChallenges("kart")
	f.logsChallenges("buddy")
	f.startMatch(2, "kart", "buddy")

	f.toldBusy("mira", kart, protocol.ChallengeSendRequest{
		Rom: protocol.RomInfo{Name: " Smash ", MD5: "ABC", Region: "USA"}, Note: " bo5? ", LiveReplay: true,
	})

	envs := f.drain("kart")
	if len(envs) != 1 || envs[0].Type != protocol.TypeChallengeMissed {
		t.Fatalf("kart got %+v, want one CHALLENGE_MISSED", envs)
	}
	if keys := jsonKeys(t, envs[0].Data); keys != "fromUserId,fromUsername,note,reason,rom,toUserId,toUsername" {
		t.Fatalf("CHALLENGE_MISSED fields %s", keys)
	}
	var got protocol.ChallengeMissedPayload
	if err := json.Unmarshal(envs[0].Data, &got); err != nil {
		t.Fatal(err)
	}
	want := protocol.ChallengeMissedPayload{
		FromUserID: mira.ID, FromUsername: "mira", ToUserID: kart.ID, ToUsername: "kart",
		Rom:  protocol.RomInfo{Name: "Smash", MD5: "ABC", Region: "USA"},
		Note: "bo5?", Reason: protocol.ChallengeBusy,
	}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(envs[0].Data, &top); err != nil {
		t.Fatal(err)
	}
	if keys := jsonKeys(t, top["rom"]); keys != "md5,name,region" {
		t.Fatalf("rom fields %s", keys)
	}

	f.heardNoMissed("buddy")
	if n := len(f.h.challenges.pending); n != 0 {
		t.Fatalf("%d challenges left pending", n)
	}
}

func TestChallengeMissedOnlyReachesClientsWithTheLog(t *testing.T) {
	f := newLateJoinFixture(t, 4)
	f.socialUser("mira")
	kart := f.socialUser("kart")
	f.socialUser("buddy")
	f.startMatch(2, "kart", "buddy")

	f.toldBusy("mira", kart, protocol.ChallengeSendRequest{Rom: testRom, Note: "gg"})
	if envs := f.drain("kart"); len(envs) != 0 {
		t.Fatalf("a client without the log got %+v", envs)
	}
}

func TestChallengeMissedComesAfterTheEarlierRefusals(t *testing.T) {
	f := newLateJoinFixture(t, 4)
	f.socialUser("mira")
	kart := f.logsChallenges("kart")
	f.logsChallenges("buddy")
	f.startMatch(2, "kart", "buddy")

	kart.SetChallengesOff(true)
	f.refusedWith("mira", "kart", protocol.ChallengeOff)
	f.heardNoMissed("kart")

	// A client without social features is told nothing, whatever else it sent.
	old := f.user("old", true)
	old.ChallengeLog = true
	f.user("pal", true)
	f.startMatch(2, "old", "pal")
	f.refusedWith("mira", "old", protocol.ReasonNeedsUpdate)
	f.heardNoMissed("old")

	// A sender in a match is refused before anything reaches the target.
	f.refusedWith("pal", "kart", protocol.ChallengeUnavailable)
	f.heardNoMissed("kart")
}

func TestChallengeMissedNotDuringTheDeclineCooldown(t *testing.T) {
	f := newLateJoinFixture(t, 4)
	mira := f.socialUser("mira")
	kart := f.logsChallenges("kart")
	f.logsChallenges("buddy")
	in := f.challenge("mira", "kart")
	f.respond("kart", in.ChallengeID, false)
	f.drain("mira")
	f.startMatch(2, "kart", "buddy")

	f.toldBusy("mira", kart, protocol.ChallengeSendRequest{Rom: testRom})
	f.heardNoMissed("kart")

	f.rewind(f.h.challenges.declined, mira.ID, kart.ID, challengeCooldown)
	f.toldBusy("mira", kart, protocol.ChallengeSendRequest{Rom: testRom})
	f.heardOneMissed("kart")
}

func TestChallengeMissedObeysTheFilter(t *testing.T) {
	f := newLateJoinFixture(t, 4)
	f.h.mod = moderation.NewManager(moderation.NewFilter([]string{"badword"}), &moderation.AdminStore{}, "")
	f.socialUser("mira")
	kart := f.logsChallenges("kart")
	f.logsChallenges("buddy")
	f.startMatch(2, "kart", "buddy")

	// A filtered note is refused as it always is, whoever it was for.
	envs := f.challengeFrom(f.client("mira"), protocol.ChallengeSendRequest{ToUserID: kart.ID, Rom: testRom, Note: "a badword"})
	if got := payloadOf[protocol.ChallengeResultPayload](t, envs, protocol.TypeChallengeResult); got.Result != protocol.ChallengeBlocked {
		t.Fatalf("result %q", got.Result)
	}
	f.heardNoMissed("kart")

	// So is a game that's missing or whose name is filtered, but as busy.
	f.toldBusy("mira", kart, protocol.ChallengeSendRequest{Rom: protocol.RomInfo{Name: "a badword game", MD5: "ABC"}})
	f.toldBusy("mira", kart, protocol.ChallengeSendRequest{Rom: protocol.RomInfo{Name: "rom"}})
	f.toldBusy("mira", kart, protocol.ChallengeSendRequest{})
	f.heardNoMissed("kart")

	f.toldBusy("mira", kart, protocol.ChallengeSendRequest{Rom: testRom, Note: "fine"})
	if got := f.heardOneMissed("kart"); got.Note != "fine" {
		t.Fatalf("note %q", got.Note)
	}
}

func TestChallengeMissedFromAMutedSenderHasNoNote(t *testing.T) {
	f := newLateJoinFixture(t, 4)
	f.h.mod = moderation.NewManager(moderation.NewFilter(nil), &moderation.AdminStore{}, "")
	f.socialUser("mira")
	kart := f.logsChallenges("kart")
	f.logsChallenges("buddy")
	f.startMatch(2, "kart", "buddy")
	f.h.mod.MuteNamed("198.51.100.1", "mira", "mod", "", 0)

	c := f.client("mira")
	c.ip = "198.51.100.1"
	onlyBusy(t, f.challengeFrom(c, protocol.ChallengeSendRequest{ToUserID: kart.ID, Rom: testRom, Note: "hello"}), kart.ID)

	envs := f.drain("kart")
	env, ok := findEnvelope(envs, protocol.TypeChallengeMissed)
	if !ok {
		t.Fatalf("no CHALLENGE_MISSED in %+v", envs)
	}
	if keys := jsonKeys(t, env.Data); strings.Contains(keys, "note") {
		t.Fatalf("a muted sender's note got through: %s", env.Data)
	}
}

func TestChallengeMissedAtMostOncePerMinute(t *testing.T) {
	f := newLateJoinFixture(t, 4)
	mira := f.socialUser("mira")
	f.socialUser("nosy")
	kart := f.logsChallenges("kart")
	lee := f.logsChallenges("lee")
	f.startMatch(2, "kart", "lee")

	f.toldBusy("mira", kart, protocol.ChallengeSendRequest{Rom: testRom})
	f.heardOneMissed("kart")
	for i := 0; i < 3; i++ {
		f.toldBusy("mira", kart, protocol.ChallengeSendRequest{Rom: testRom, Note: "again"})
	}
	f.heardNoMissed("kart")

	// Per sender and player asked.
	f.toldBusy("mira", lee, protocol.ChallengeSendRequest{Rom: testRom})
	f.heardOneMissed("lee")
	f.toldBusy("nosy", kart, protocol.ChallengeSendRequest{Rom: testRom})
	if got := f.heardOneMissed("kart"); got.FromUsername != "nosy" {
		t.Fatalf("from %q", got.FromUsername)
	}

	// Dropped ones didn't restart the minute.
	f.rewind(f.h.challenges.missed, mira.ID, kart.ID, missedNoticeInterval)
	f.toldBusy("mira", kart, protocol.ChallengeSendRequest{Rom: testRom, Note: "later"})
	if got := f.heardOneMissed("kart"); got.Note != "later" || got.FromUserID != mira.ID {
		t.Fatalf("got %+v", got)
	}
}

func TestChallengeMissedForALocalPlayerReachesTheirPC(t *testing.T) {
	f := newLateJoinFixture(t, 4)
	f.socialUser("kart")
	mira := f.logsChallenges("mira")
	sam := f.addLocal("mira", "sam")
	f.logsChallenges("buddy")
	if !sam.ChallengeLog {
		t.Fatal("a local player didn't take its owner's challengeLog")
	}
	f.startMatch(4, "mira", "buddy")

	// The sender hears exactly what a challenge to anyone in a match gets.
	f.toldBusy("kart", sam, protocol.ChallengeSendRequest{Rom: testRom, Note: "you two?"})
	got := f.heardOneMissed("mira")
	if got.ToUserID != sam.ID || got.ToUsername != "sam" || got.FromUsername != "kart" || got.Note != "you two?" {
		t.Fatalf("mira's connection got %+v", got)
	}

	// Each player on the PC is their own: the owner hears of theirs too.
	f.toldBusy("kart", mira, protocol.ChallengeSendRequest{Rom: testRom})
	if got := f.heardOneMissed("mira"); got.ToUserID != mira.ID {
		t.Fatalf("got %+v", got)
	}
}

func TestChallengeMissedInviteCarriesTheRoom(t *testing.T) {
	f := newLateJoinFixture(t, 4)
	mira := f.socialUser("mira")
	kart := f.logsChallenges("kart")
	f.logsChallenges("buddy")
	f.startMatch(2, "kart", "buddy")
	f.send("mira", protocol.TypeRoomCreate, protocol.RoomCreateRequest{
		Name: "Friday sets", Rom: protocol.RomInfo{Name: "Smash", MD5: "DEF", Region: "USA"}, MaxPlayers: 4, Prediction: 7,
	})
	if mira.CurrentRoom == nil {
		t.Fatalf("room wasn't created: %+v", f.drain("mira"))
	}

	// The room's game, whatever the request names.
	f.toldBusy("mira", kart, protocol.ChallengeSendRequest{Rom: protocol.RomInfo{Name: "other", MD5: "ZZZ"}, Note: "join us"})
	got := f.heardOneMissed("kart")
	want := protocol.ChallengeMissedPayload{
		FromUserID: mira.ID, FromUsername: "mira", ToUserID: kart.ID, ToUsername: "kart",
		Rom:  protocol.RomInfo{Name: "Smash", MD5: "DEF", Region: "USA"},
		Note: "join us", Invite: true, RoomName: "Friday sets", Reason: protocol.ChallengeBusy,
	}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestHelloTakesTheChallengeLogFlag(t *testing.T) {
	for _, logs := range []bool{true, false} {
		h := NewHub(state.New(), "", protocol.ICEConfiguration{}, nil, nil)
		c := &Client{hub: h, send: make(chan []byte, 64), ip: "192.168.1.20"}
		raw, _ := json.Marshal(map[string]any{
			"username": "mira", "clientVersion": "vdev-test", "social": true, "challengeLog": logs,
		})
		if err := h.handleHello(c, protocol.Envelope{Type: protocol.TypeHello, Data: raw}); err != nil {
			t.Fatalf("HELLO: %v", err)
		}
		if c.user.ChallengeLog != logs {
			t.Fatalf("HELLO challengeLog %v: user has %v", logs, c.user.ChallengeLog)
		}
		if ok := payloadOf[protocol.HelloOKResponse](t, drainClient(t, c), protocol.TypeHelloOK); !ok.ChallengeLog {
			t.Fatal("HELLO_OK doesn't offer the challenge log")
		}
	}
}
