package ws

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jay-day/rmgk-lobby/internal/accounts"
	"github.com/jay-day/rmgk-lobby/internal/protocol"
	"github.com/jay-day/rmgk-lobby/internal/ranked"
	"github.com/jay-day/rmgk-lobby/internal/rating"
	"github.com/jay-day/rmgk-lobby/internal/state"
)

const testRomMD5 = "2B2D6B295106C54216B7FC7A2F14346E"

const testRankedConfig = `{"ladders":[{"id":"remix","name":"Smash Remix 1v1","moduleId":"smashremix",
	"rulesets":["remix-1v1-vanilla","remix-1v1-remix"],"bestOf":[3,5],
	"rulesetNames":{"remix-1v1-vanilla":"Vanilla roster","remix-1v1-remix":"Remix roster"},
	"roms":[{"md5":"` + testRomMD5 + `","name":"Remix"}]}]}`

type rankedFixture struct {
	t     *testing.T
	hub   *Hub
	store *accounts.Store
}

func newRankedFixture(t *testing.T) *rankedFixture {
	t.Helper()
	return newRankedFixtureWith(t, testRankedConfig)
}

func newRankedFixtureWith(t *testing.T, config string) *rankedFixture {
	t.Helper()
	store, err := accounts.Open(filepath.Join(t.TempDir(), "lobby.db"))
	if err != nil {
		t.Fatalf("accounts.Open: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	cfg, err := ranked.Parse([]byte(config))
	if err != nil {
		t.Fatalf("ranked.Parse: %v", err)
	}
	hub := NewHub(state.New(), "", protocol.ICEConfiguration{}, nil)
	hub.EnableAccounts(store, cfg)
	return &rankedFixture{t: t, hub: hub, store: store}
}

// connect adds a user the way handleHello would, verified when registered.
func (f *rankedFixture) connect(name string, registered bool) *Client {
	f.t.Helper()
	send := make(chan []byte, 256)
	u := &state.User{
		ID: f.hub.state.NextUserID(), Username: name, State: state.UserStateIdle,
		JoinedAt: time.Now(), LastSeen: time.Now(), Send: send,
	}
	if registered {
		acct, err := f.store.Register(name, "password123")
		if err != nil {
			f.t.Fatalf("Register(%s): %v", name, err)
		}
		u.AccountID = acct.ID
	}
	if err := f.hub.state.AddUser(u); err != nil {
		f.t.Fatalf("AddUser: %v", err)
	}
	c := &Client{hub: f.hub, user: u, send: send}
	f.hub.clients[u.ID] = c
	return c
}

func envelope(t *testing.T, v any) protocol.Envelope {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	return protocol.Envelope{Data: data}
}

// drain returns every queued message of type msgType.
func drain(c *Client, msgType string) []protocol.Envelope {
	var out []protocol.Envelope
	for {
		select {
		case raw := <-c.send:
			var env protocol.Envelope
			if json.Unmarshal(raw, &env) == nil && env.Type == msgType {
				out = append(out, env)
			}
		default:
			return out
		}
	}
}

func (f *rankedFixture) createRankedRoom(host *Client, ranked *protocol.RankedRoomRequest) *state.Room {
	f.t.Helper()
	f.hub.handleRoomCreate(host, envelope(f.t, protocol.RoomCreateRequest{
		Name: "ranked", Rom: protocol.RomInfo{Name: "Remix", MD5: testRomMD5},
		MaxPlayers: 4, Delay: 2, Prediction: 7, Ranked: ranked,
	}))
	return host.user.CurrentRoom
}

func TestHelloAuthentication(t *testing.T) {
	f := newRankedFixture(t)
	c := &Client{hub: f.hub, ip: "203.0.113.5"}

	reg := f.hub.authenticateHello(c, &protocol.HelloRequest{Username: "Alice",
		Auth: &protocol.HelloAuth{Mode: "register", Password: "password123"}})
	if reg.reason != "" || reg.account == nil || reg.token == "" {
		t.Fatalf("register = %+v", reg)
	}

	// guests can't take a registered name, in any casing
	if guest := f.hub.authenticateHello(c, &protocol.HelloRequest{Username: "alice"}); guest.reason != protocol.ReasonUsernameRegistered {
		t.Fatalf("guest on registered name = %+v", guest)
	}
	if guest := f.hub.authenticateHello(c, &protocol.HelloRequest{Username: "bob"}); guest.reason != "" || guest.account != nil {
		t.Fatalf("plain guest = %+v", guest)
	}

	login := f.hub.authenticateHello(c, &protocol.HelloRequest{Username: "ALICE",
		Auth: &protocol.HelloAuth{Mode: "login", Password: "password123"}})
	if login.reason != "" || login.account.Username != "Alice" {
		t.Fatalf("login = %+v", login)
	}
	byToken := f.hub.authenticateHello(c, &protocol.HelloRequest{Username: "x",
		Auth: &protocol.HelloAuth{Mode: "token", Token: login.token}})
	if byToken.reason != "" || byToken.account.ID != reg.account.ID {
		t.Fatalf("token login = %+v", byToken)
	}

	// repeated failures lock the IP out
	for i := 0; i < authFailLimit; i++ {
		f.hub.authenticateHello(c, &protocol.HelloRequest{Username: "Alice",
			Auth: &protocol.HelloAuth{Mode: "login", Password: "wrong-password"}})
	}
	locked := f.hub.authenticateHello(c, &protocol.HelloRequest{Username: "Alice",
		Auth: &protocol.HelloAuth{Mode: "login", Password: "password123"}})
	if locked.reason != protocol.ReasonTooManyAttempts {
		t.Fatalf("after %d failures = %+v", authFailLimit, locked)
	}
	// the lockout says when to try again: the oldest failure leaves the window
	if locked.retryAfter < 14*60 || locked.retryAfter > 15*60 || !strings.Contains(locked.message, "Try again in 15 minutes") {
		t.Fatalf("lockout retry = %d, %q", locked.retryAfter, locked.message)
	}
}

func TestRankedRoomEligibility(t *testing.T) {
	f := newRankedFixture(t)
	guestHost := f.connect("guesthost", false)
	if room := f.createRankedRoom(guestHost, &protocol.RankedRoomRequest{Enabled: true}); room != nil {
		t.Fatal("guest created a ranked room")
	}

	host := f.connect("host", true)
	room := f.createRankedRoom(host, &protocol.RankedRoomRequest{Enabled: true, BestOf: 5})
	if room == nil {
		t.Fatal("verified host could not create a ranked room")
	}
	settings := room.GetRanked()
	if settings == nil || settings.BestOf != 5 || settings.LadderID != "remix" || room.MaxPlayers != 2 {
		t.Fatalf("ranked settings = %+v, max %d", settings, room.MaxPlayers)
	}
	if rs := f.hub.roomState(room); rs.Ranked == nil || rs.Ranked.LadderName != "Smash Remix 1v1" ||
		rs.Ranked.RulesetID != "remix-1v1-vanilla" || rs.Ranked.RulesetName != "Vanilla roster" ||
		rs.Players[0].Standing == nil || !rs.Players[0].Verified {
		t.Fatalf("room state = %+v", rs)
	}

	guest := f.connect("guest", false)
	f.hub.handleRoomJoin(guest, envelope(t, protocol.RoomJoinRequest{RoomID: room.ID}))
	if guest.user.CurrentRoom != nil {
		t.Fatal("guest joined a ranked room")
	}
	if fails := drain(guest, protocol.TypeRoomJoinFail); len(fails) != 1 {
		t.Fatalf("join fails = %d", len(fails))
	}

	player := f.connect("player", true)
	f.hub.handleRoomJoin(player, envelope(t, protocol.RoomJoinRequest{RoomID: room.ID}))
	if player.user.CurrentRoom != room {
		t.Fatal("verified player could not join")
	}

	// unranked rooms can't be switched to ranked for a ROM without a ladder
	f.hub.handleRoomUpdateRanked(host, envelope(t, protocol.RankedRoomRequest{Enabled: true, RulesetID: "remix-1v1-remix"}))
	if rs := f.hub.roomState(room); rs.Ranked == nil || rs.Ranked.RulesetName != "Remix roster" {
		t.Fatalf("ruleset switch: %+v", rs.Ranked)
	}

	other := f.connect("other", true)
	f.hub.handleRoomCreate(other, envelope(t, protocol.RoomCreateRequest{
		Name: "casual", Rom: protocol.RomInfo{Name: "Mario Kart", MD5: "00112233445566778899AABBCCDDEEFF"},
		MaxPlayers: 2,
	}))
	f.hub.handleRoomUpdateRanked(other, envelope(t, protocol.RankedRoomRequest{Enabled: true}))
	if other.user.CurrentRoom.GetRanked() != nil {
		t.Fatal("ranked enabled for a ROM without a ladder")
	}
}

func TestRankedSkillFilter(t *testing.T) {
	f := newRankedFixture(t)
	host := f.connect("veteran", true)
	newbie := f.connect("newbie", true)

	// make the host a settled Diamond player on the vanilla roster only
	strong := rating.Rating{Rating: 1800, Deviation: 60, Volatility: 0.06}
	if err := f.store.SeedRating(host.user.AccountID, "remix", "remix-1v1-vanilla", strong); err != nil {
		t.Fatal(err)
	}

	no := false
	room := f.createRankedRoom(host, &protocol.RankedRoomRequest{Enabled: true, MaxTierGap: 1, AllowProvisional: &no})
	f.hub.handleRoomJoin(newbie, envelope(t, protocol.RoomJoinRequest{RoomID: room.ID}))
	if newbie.user.CurrentRoom != nil {
		t.Fatal("provisional Bronze player passed a 1-tier filter with provisional disallowed")
	}

	// the remix roster is rated separately: there both are new, so they match
	f.hub.handleRoomUpdateRanked(host, envelope(t, protocol.RankedRoomRequest{Enabled: true, MaxTierGap: 1,
		AllowProvisional: &no, RulesetID: "remix-1v1-remix"}))
	f.hub.handleRoomJoin(newbie, envelope(t, protocol.RoomJoinRequest{RoomID: room.ID}))
	if newbie.user.CurrentRoom != room {
		t.Fatal("the host's vanilla rating filtered a remix-roster room")
	}
	f.hub.handleRoomLeave(newbie, protocol.Envelope{})

	f.hub.handleRoomUpdateRanked(host, envelope(t, protocol.RankedRoomRequest{Enabled: true, MaxTierGap: 1}))
	f.hub.handleRoomJoin(newbie, envelope(t, protocol.RoomJoinRequest{RoomID: room.ID}))
	if newbie.user.CurrentRoom != room {
		t.Fatal("provisional player rejected although provisional players are allowed")
	}
}

func startRankedMatch(t *testing.T, f *rankedFixture) (*Client, *Client, uint64) {
	t.Helper()
	host := f.connect("p1", true)
	guest := f.connect("p2", true)
	room := f.createRankedRoom(host, &protocol.RankedRoomRequest{Enabled: true})
	f.hub.handleRoomJoin(guest, envelope(t, protocol.RoomJoinRequest{RoomID: room.ID}))
	f.hub.handleRoomStart(host, protocol.Envelope{})
	begins := drain(guest, protocol.TypeMatchBegin)
	if len(begins) != 1 {
		t.Fatalf("MATCH_BEGIN count = %d", len(begins))
	}
	var begin protocol.MatchBeginPayload
	json.Unmarshal(begins[0].Data, &begin)
	if begin.Ranked == nil || begin.Ranked.BestOf != 3 || len(begin.Ranked.Players) != 2 {
		t.Fatalf("MATCH_BEGIN ranked = %+v", begin.Ranked)
	}
	drain(host, protocol.TypeMatchBegin)
	return host, guest, begin.MatchID
}

func result(matchID uint64, winner int, score []int) protocol.MatchResultRequest {
	games := []protocol.MatchResultGame{}
	for slot, won := range score {
		for i := 0; i < won; i++ {
			games = append(games, protocol.MatchResultGame{Game: len(games) + 1, WinnerSlot: slot + 1, Stage: 6})
		}
	}
	return protocol.MatchResultRequest{MatchID: matchID, WinnerSlot: winner, Score: score,
		Games: games, RulesetHash: "abc", Integrity: "ok"}
}

func rankedResultFor(t *testing.T, c *Client) protocol.RankedResultPayload {
	t.Helper()
	msgs := drain(c, protocol.TypeRankedResult)
	if len(msgs) != 1 {
		t.Fatalf("%s got %d RANKED_RESULT", c.user.Username, len(msgs))
	}
	var payload protocol.RankedResultPayload
	json.Unmarshal(msgs[0].Data, &payload)
	return payload
}

func TestRankedSetRated(t *testing.T) {
	f := newRankedFixture(t)
	host, guest, matchID := startRankedMatch(t, f)

	f.hub.handleMatchResult(host, envelope(t, result(matchID, 1, []int{2, 1})))
	if len(drain(host, protocol.TypeRankedResult)) != 0 {
		t.Fatal("set finalized after one report")
	}
	f.hub.handleMatchResult(guest, envelope(t, result(matchID, 1, []int{2, 1})))

	res := rankedResultFor(t, host)
	if res.Status != "rated" || res.WinnerSlot != 1 || len(res.Players) != 2 {
		t.Fatalf("result = %+v", res)
	}
	for _, p := range res.Players {
		if p.After.SetsPlayed != 1 || p.Before.SetsPlayed != 0 {
			t.Fatalf("sets played = %+v", p)
		}
		if (p.Slot == 1) != (p.After.Rating > p.Before.Rating) {
			t.Fatalf("rating moved the wrong way for slot %d: %+v", p.Slot, p)
		}
	}
	rankedResultFor(t, guest)

	// the normal end of a set: both drop afterwards, nothing more happens
	f.hub.handleMatchFinished(guest, protocol.Envelope{})
	f.hub.handleMatchFinished(host, protocol.Envelope{})
	if len(drain(host, protocol.TypeRankedResult)) != 0 {
		t.Fatal("extra RANKED_RESULT after the set")
	}
}

func TestRankedSetDisputed(t *testing.T) {
	f := newRankedFixture(t)
	host, guest, matchID := startRankedMatch(t, f)
	f.hub.handleMatchResult(host, envelope(t, result(matchID, 1, []int{2, 0})))
	f.hub.handleMatchResult(guest, envelope(t, result(matchID, 2, []int{0, 2})))
	if res := rankedResultFor(t, host); res.Status != "disputed" || res.Players[0].After != res.Players[0].Before {
		t.Fatalf("result = %+v", res)
	}
}

func TestRankedSetVoidedWhenPlayerLeaves(t *testing.T) {
	f := newRankedFixture(t)
	host, guest, _ := startRankedMatch(t, f)
	f.hub.handleMatchFinished(guest, protocol.Envelope{}) // drops mid-set
	if res := rankedResultFor(t, host); res.Status != "voided" || res.Reason != "player_left" {
		t.Fatalf("result = %+v", res)
	}
}

func TestJudgeRankedSet(t *testing.T) {
	set := func(reports ...protocol.MatchResultRequest) *pendingSet {
		p := &pendingSet{
			settings:     state.RankedSettings{BestOf: 3},
			participants: []rankedParticipant{{userID: 1, slot: 1}, {userID: 2, slot: 2}},
			reports:      map[uint64]protocol.MatchResultRequest{},
		}
		for i, r := range reports {
			p.reports[uint64(i+1)] = r
		}
		return p
	}
	ok := result(1, 2, []int{1, 2})
	tests := []struct {
		name   string
		set    *pendingSet
		status accounts.SetStatus
	}{
		{"agree", set(ok, ok), accounts.SetRated},
		{"missing", set(ok), accounts.SetVoided},
		{"desync", set(ok, func() protocol.MatchResultRequest { r := ok; r.Integrity = "desync detected"; return r }()), accounts.SetDisputed},
		{"different hash", set(ok, func() protocol.MatchResultRequest { r := ok; r.RulesetHash = "zzz"; return r }()), accounts.SetDisputed},
		{"set not finished", set(result(1, 2, []int{1, 1}), result(1, 2, []int{1, 1})), accounts.SetDisputed},
		{"winner not a player", set(result(1, 3, []int{0, 0, 2}), result(1, 3, []int{0, 0, 2})), accounts.SetDisputed},
	}
	for _, tt := range tests {
		if got, reason := judgeRankedSet(tt.set); got != tt.status {
			t.Errorf("%s: status %s (%s), want %s", tt.name, got, reason, tt.status)
		}
	}
}

func TestRankedSetVoidedWhenReportTimesOut(t *testing.T) {
	saved := rankedResultTimeout
	rankedResultTimeout = 50 * time.Millisecond
	defer func() { rankedResultTimeout = saved }()

	f := newRankedFixture(t)
	host, _, matchID := startRankedMatch(t, f)
	f.hub.handleMatchResult(host, envelope(t, result(matchID, 1, []int{2, 0})))
	time.Sleep(300 * time.Millisecond)
	if res := rankedResultFor(t, host); res.Status != "voided" || res.Reason != "result_timeout" {
		t.Fatalf("result = %+v", res)
	}
}

func modNotices(c *Client) []string {
	var out []string
	for _, env := range drain(c, protocol.TypeModNotice) {
		var p protocol.ModNoticePayload
		json.Unmarshal(env.Data, &p)
		out = append(out, p.Text)
	}
	return out
}

func TestModeratorResolvesDisputedSet(t *testing.T) {
	f := newRankedFixture(t)
	host, guest, matchID := startRankedMatch(t, f)
	f.hub.handleMatchResult(host, envelope(t, result(matchID, 1, []int{2, 0})))
	f.hub.handleMatchResult(guest, envelope(t, result(matchID, 2, []int{0, 2})))
	disputed := rankedResultFor(t, host)
	drain(guest, protocol.TypeRankedResult)
	if disputed.Status != "disputed" || disputed.SetID == 0 {
		t.Fatalf("result = %+v", disputed)
	}

	mod := f.connect("mod", false)
	mod.isModerator, mod.adminName = true, "admin"
	f.hub.modListDisputes(mod)
	if notices := modNotices(mod); len(notices) != 2 || !strings.Contains(notices[0], "P1 won 2-0") ||
		!strings.Contains(notices[0], "P2 won 0-2") {
		t.Fatalf("disputes list = %q", notices)
	}

	f.hub.modResolveSet(mod, fmt.Sprintf("%d p2", disputed.SetID), "p1 confirmed in chat")
	resolved := rankedResultFor(t, guest)
	if resolved.Status != "rated" || resolved.WinnerSlot != 2 || resolved.SetID != disputed.SetID {
		t.Fatalf("resolution = %+v", resolved)
	}
	for _, p := range resolved.Players {
		if (p.Slot == 2) != (p.After.Rating > p.Before.Rating) {
			t.Fatalf("rating moved the wrong way: %+v", p)
		}
	}
	rankedResultFor(t, host)
	f.hub.modResolveSet(mod, fmt.Sprintf("%d void", disputed.SetID), "")
	if notices := modNotices(mod); len(notices) != 2 || !strings.Contains(notices[1], "isn't disputed") {
		t.Fatalf("second resolution notices = %q", notices)
	}
}

func TestPasswordChangeAndReset(t *testing.T) {
	f := newRankedFixture(t)
	c := f.connect("ace", true)

	f.hub.handleAccountChangePassword(c, envelope(t, protocol.AccountChangePasswordRequest{
		CurrentPassword: "password123", NewPassword: "newpassword1"}))
	changed := drain(c, protocol.TypeAccountPasswordChanged)
	var payload protocol.AccountPasswordChangedPayload
	if len(changed) != 1 || json.Unmarshal(changed[0].Data, &payload) != nil || payload.SessionToken == "" {
		t.Fatalf("change password reply = %v", changed)
	}
	f.hub.handleAccountChangePassword(c, envelope(t, protocol.AccountChangePasswordRequest{
		CurrentPassword: "password123", NewPassword: "another1234"}))
	if len(drain(c, protocol.TypeAccountPasswordFail)) != 1 {
		t.Fatal("old password accepted")
	}

	mod := f.connect("mod", false)
	mod.isModerator, mod.adminName = true, "admin"
	f.hub.modPasswordReset(mod, "ACE")
	notices := modNotices(mod)
	if len(notices) != 1 || !strings.Contains(notices[0], "Reset code for ace") {
		t.Fatalf("reset notice = %q", notices)
	}
	code := strings.Fields(strings.SplitN(notices[0], ": ", 2)[1])[0]

	reset := f.hub.authenticateHello(c, &protocol.HelloRequest{Username: "ace",
		Auth: &protocol.HelloAuth{Mode: "reset", ResetCode: code, Password: "resetpass1"}})
	if reset.account == nil || reset.token == "" {
		t.Fatalf("reset hello = %+v", reset)
	}
	again := f.hub.authenticateHello(c, &protocol.HelloRequest{Username: "ace",
		Auth: &protocol.HelloAuth{Mode: "reset", ResetCode: code, Password: "resetpass2"}})
	if again.reason != protocol.ReasonAuthFailed {
		t.Fatalf("reused code = %+v", again)
	}
	if _, err := f.store.Login("ace", "resetpass1"); err != nil {
		t.Fatalf("login after reset: %v", err)
	}
}
