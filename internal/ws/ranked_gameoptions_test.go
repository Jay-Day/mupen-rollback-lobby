package ws

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/jay-day/rmgk-lobby/internal/gameoptions"
	"github.com/jay-day/rmgk-lobby/internal/protocol"
	"github.com/jay-day/rmgk-lobby/internal/state"
)

// 1v1 and 2v2 ladders whose rulesets are played with game options from the
// fixture pack (internal/gameoptions/testdata), as ranked.example.json names them.
const testOptionsConfig = `{"ladders":[
	{"id":"remix","name":"Smash Remix 1v1","moduleId":"smashremix",
	 "rulesets":["remix-1v1-vanilla","remix-1v1-remix"],"roms":[{"md5":"` + testRomMD5 + `"}],
	 "rulesetOptions":{"remix-1v1-vanilla":["ranked-rules","vanilla-only","stages-dream-land"],
	                   "remix-1v1-remix":["ranked-rules","stages-remix-ranked"]}},
	{"id":"remix-2v2","name":"Smash Remix 2v2","moduleId":"smashremix","teamSize":2,
	 "rulesets":["remix-2v2-vanilla","remix-2v2-remix"],"roms":[{"md5":"` + testRomMD5 + `"}],
	 "rulesetOptions":{"remix-2v2-vanilla":["ranked-rules","ranked-teams","vanilla-only","stages-dream-land"],
	                   "remix-2v2-remix":["ranked-rules","ranked-teams","stages-remix-ranked"]},
	 "teamOptions":{"2":"teams-p1p2","3":"teams-p1p3","4":"teams-p1p4"}}]}`

const (
	vanilla1v1Options = "ranked-rules,vanilla-only,stages-dream-land"
	remix1v1Options   = "ranked-rules,stages-remix-ranked"
	vanilla2v2Options = "ranked-rules,ranked-teams,vanilla-only,stages-dream-land"
)

func newRankedOptionsFixture(t *testing.T) *rankedFixture {
	t.Helper()
	f := newRankedFixtureWith(t, testOptionsConfig)
	packs, err := gameoptions.LoadDir("../gameoptions/testdata")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.hub.rankedConfig.UseGameOptions(packs); err != nil {
		t.Fatalf("UseGameOptions: %v", err)
	}
	return f
}

// oldClient connects a logged-in player whose client doesn't send rankedOptions.
func (f *rankedFixture) oldClient(name string) *Client {
	c := f.connect(name, true)
	c.user.RankedOptions = false
	return c
}

func rankedOptionIDs(opts []protocol.RankedGameOption) string {
	ids := make([]string, len(opts))
	for i, opt := range opts {
		ids[i] = opt.ID
	}
	return strings.Join(ids, ",")
}

func roomOptionIDs(opts []protocol.GameOption) string {
	ids := make([]string, len(opts))
	for i, opt := range opts {
		ids[i] = opt.ID
	}
	return strings.Join(ids, ",")
}

// matchBegin returns the one MATCH_BEGIN queued for c, and its JSON.
func matchBegin(t *testing.T, c *Client) (protocol.MatchBeginPayload, string) {
	t.Helper()
	begins := drain(c, protocol.TypeMatchBegin)
	if len(begins) != 1 {
		t.Fatalf("%s got %d MATCH_BEGIN", c.user.Username, len(begins))
	}
	var begin protocol.MatchBeginPayload
	if err := json.Unmarshal(begins[0].Data, &begin); err != nil {
		t.Fatal(err)
	}
	if begin.Ranked == nil {
		t.Fatalf("%s's MATCH_BEGIN isn't ranked", c.user.Username)
	}
	return begin, string(begins[0].Data)
}

func hasNotice(c *Client, text string) bool {
	return containsString(noticesIn(drain(c, protocol.TypeModNotice)), text)
}

// refusal returns the reason of the one failType message queued for c, and
// whether a notice told c text.
func refusal(t *testing.T, c *Client, failType, text string) (string, bool) {
	t.Helper()
	msgs := drainAll(c)
	return failReason(t, ofType(msgs, failType)), containsString(noticesIn(msgs), text)
}

func TestHelloTakesTheRankedOptionsCapability(t *testing.T) {
	for _, capable := range []bool{true, false} {
		h := NewHub(state.New(), "", protocol.ICEConfiguration{}, nil, nil)
		c := &Client{hub: h, send: make(chan []byte, 64)}
		raw, _ := json.Marshal(map[string]any{
			"username": "mira", "clientVersion": "vdev-test", "rankedOptions": capable,
		})
		if err := h.handleHello(c, protocol.Envelope{Type: protocol.TypeHello, Data: raw}); err != nil {
			t.Fatalf("HELLO: %v", err)
		}
		if c.user.RankedOptions != capable {
			t.Fatalf("HELLO rankedOptions %v: user has %v", capable, c.user.RankedOptions)
		}
	}
}

// A logged-in client that can't play with the lobby's game options gets no
// ranked room, seat, search or invite; the reason says to update.
func TestRankedNeedsTheRankedOptionsCapability(t *testing.T) {
	f := newRankedOptionsFixture(t)
	const update = "old needs a newer version of Dale64 to play ranked."
	old := f.oldClient("old")

	if room := f.createRankedRoom(old, &protocol.RankedRoomRequest{Enabled: true}); room != nil {
		t.Fatal("an old client created a ranked room")
	}
	if got, told := refusal(t, old, protocol.TypeRoomCreateFail, update); got != protocol.ReasonRankedNeedsUpdate || !told {
		t.Fatalf("create refused with %q, notice %v", got, told)
	}

	f.quickMatch(old, &protocol.RankedRoomRequest{Enabled: true})
	if f.hub.matchmaker.contains(old.user.ID) {
		t.Fatal("an old client searches for a ranked match")
	}
	if !hasNotice(old, update) {
		t.Fatal("quick ranked: no update notice")
	}
	f.teamSearch(old, teamSessionRequest, "", false)
	if f.hub.matchmaker.contains(old.user.ID) {
		t.Fatal("an old client searches for a 2v2 ranked match")
	}
	// casual searches don't need it
	f.quickMatch(old, nil)
	if !f.hub.matchmaker.contains(old.user.ID) {
		t.Fatal("an old client can't search for a casual match")
	}
	f.hub.handleQuickMatchCancel(old, protocol.Envelope{})

	host := f.connect("host", true)
	room := f.createRankedRoom(host, &protocol.RankedRoomRequest{Enabled: true})
	if room == nil {
		t.Fatal("a current client couldn't create a ranked room")
	}
	f.hub.handleRoomJoin(old, envelope(t, protocol.RoomJoinRequest{RoomID: room.ID}))
	if old.user.CurrentRoom != nil {
		t.Fatal("an old client joined a ranked room")
	}
	if got, told := refusal(t, old, protocol.TypeRoomJoinFail, update); got != protocol.ReasonRankedNeedsUpdate || !told {
		t.Fatalf("join refused with %q, notice %v", got, told)
	}
	if !f.hub.rankedInviteRefusal(old.user, room) || f.hub.rankedInviteRefusal(f.connect("new", true).user, room) {
		t.Fatal("ranked invites don't follow the capability")
	}

	// guests are told to log in first
	guest := f.connect("guest", false)
	guest.user.RankedOptions = false
	f.createRankedRoom(guest, &protocol.RankedRoomRequest{Enabled: true})
	if got, told := refusal(t, guest, protocol.TypeRoomCreateFail,
		"guest needs to log into an account to play ranked."); got != protocol.ReasonNotVerified || !told {
		t.Fatalf("guest refused with %q, notice %v", got, told)
	}

	// a room with an old client seated can't turn ranked
	casualHost := f.connect("casualhost", true)
	f.hub.handleRoomCreate(casualHost, envelope(t, protocol.RoomCreateRequest{
		Name: "casual", Rom: protocol.RomInfo{Name: "Remix", MD5: testRomMD5}, MaxPlayers: 2,
	}))
	casual := casualHost.user.CurrentRoom
	f.hub.handleRoomJoin(old, envelope(t, protocol.RoomJoinRequest{RoomID: casual.ID}))
	if old.user.CurrentRoom != casual {
		t.Fatal("an old client couldn't join a casual room")
	}
	f.hub.handleRoomUpdateRanked(casualHost, envelope(t, protocol.RankedRoomRequest{Enabled: true}))
	if casual.GetRanked() != nil {
		t.Fatal("a room with an old client turned ranked")
	}
	if !hasNotice(casualHost, update) {
		t.Fatal("ranked on: the host wasn't told why")
	}

	// and a set doesn't start with one seated
	player := f.connect("player", true)
	f.hub.handleRoomJoin(player, envelope(t, protocol.RoomJoinRequest{RoomID: room.ID}))
	player.user.RankedOptions = false
	f.hub.handleRoomStart(host, protocol.Envelope{})
	if len(drain(player, protocol.TypeMatchBegin)) != 0 || room.State != state.RoomStateWaiting {
		t.Fatal("a ranked set started with an old client seated")
	}
	if !hasNotice(host, "player needs a newer version of Dale64 to play ranked.") {
		t.Fatal("start: the host wasn't told why")
	}
}

// A ranked room's game options are its ruleset's, shown like a host's and
// locked; turning ranked off hands the choice back to the host, all off.
func TestRankedRoomPlaysTheRulesetsGameOptions(t *testing.T) {
	f := newRankedOptionsFixture(t)
	host := f.connect("host", true)
	host.user.GameOptions = true
	f.hub.handleRoomCreate(host, envelope(t, protocol.RoomCreateRequest{
		Name: "room", Rom: protocol.RomInfo{Name: "Remix", MD5: testRomMD5}, MaxPlayers: 2,
	}))
	room := host.user.CurrentRoom
	f.hub.handleRoomUpdateGameOptions(host, envelope(t, vanillaOnly))
	if optionIDs(room.GameOptionList()) != "vanilla-only" {
		t.Fatalf("casual options = %+v", room.GameOptionList())
	}

	lastState := func() protocol.RoomStatePayload {
		t.Helper()
		states := drain(host, protocol.TypeRoomState)
		if len(states) == 0 {
			t.Fatal("no ROOM_STATE")
		}
		var st protocol.RoomStatePayload
		json.Unmarshal(states[len(states)-1].Data, &st)
		return st
	}

	f.hub.handleRoomUpdateRanked(host, envelope(t, protocol.RankedRoomRequest{Enabled: true}))
	st := lastState()
	if st.Ranked == nil || st.GameOptionsOffered || roomOptionIDs(st.GameOptions) != vanilla1v1Options {
		t.Fatalf("ranked room state: ranked %+v, offered %v, options %+v", st.Ranked, st.GameOptionsOffered, st.GameOptions)
	}
	raw, _ := json.Marshal(f.hub.roomState(room))
	if !strings.Contains(string(raw), `"gameOptions":[{"id":"ranked-rules","label":"Ranked rules"},`+
		`{"id":"vanilla-only","label":"Vanilla only"},{"id":"stages-dream-land","label":"Dream Land only"}]`) ||
		strings.Contains(string(raw), "gameOptionsOffered") {
		t.Fatalf("ranked room state JSON: %s", raw)
	}
	if list := f.hub.buildRoomListPayload(); len(list.Rooms) != 1 || roomOptionIDs(list.Rooms[0].GameOptions) != vanilla1v1Options {
		t.Fatalf("room list = %+v", list.Rooms)
	}

	// the host can't change or clear them
	for _, req := range []protocol.RoomUpdateGameOptionsRequest{vanillaOnly, {}} {
		f.hub.handleRoomUpdateGameOptions(host, envelope(t, req))
		if optionIDs(room.GameOptionList()) != vanilla1v1Options {
			t.Fatalf("the host changed a ranked room's options to %+v", room.GameOptionList())
		}
	}

	// they follow the ruleset
	f.hub.handleRoomUpdateRanked(host, envelope(t, protocol.RankedRoomRequest{Enabled: true, RulesetID: "remix-1v1-remix"}))
	if st := lastState(); roomOptionIDs(st.GameOptions) != remix1v1Options {
		t.Fatalf("remix roster options = %+v", st.GameOptions)
	}

	f.hub.handleRoomUpdateRanked(host, envelope(t, protocol.RankedRoomRequest{Enabled: false}))
	if st := lastState(); st.Ranked != nil || st.GameOptions != nil || !st.GameOptionsOffered {
		t.Fatalf("ranked off: ranked %+v, options %+v, offered %v", st.Ranked, st.GameOptions, st.GameOptionsOffered)
	}
	f.hub.handleRoomUpdateGameOptions(host, envelope(t, vanillaOnly))
	if optionIDs(room.GameOptionList()) != "vanilla-only" {
		t.Fatalf("after ranked: the host's options = %+v", room.GameOptionList())
	}
}

// A ranked set's players need rankedOptions, not the casual gameOptions
// capability, even though the room shows options.
func TestRankedMatchBeginCarriesTheGameOptions(t *testing.T) {
	f := newRankedOptionsFixture(t)
	host, guest := f.connect("host", true), f.connect("guest", true)
	room := f.createRankedRoom(host, &protocol.RankedRoomRequest{Enabled: true})
	f.hub.handleRoomJoin(guest, envelope(t, protocol.RoomJoinRequest{RoomID: room.ID}))
	if host.user.GameOptions || guest.user.GameOptions || len(room.GameOptionList()) == 0 {
		t.Fatal("test setup: the players shouldn't have the casual capability")
	}
	f.hub.handleRoomStart(host, protocol.Envelope{})
	begin, raw := matchBegin(t, guest)
	if room.State != state.RoomStateInGame {
		t.Fatal("the ranked set didn't start")
	}
	opts := begin.Ranked.GameOptions
	if rankedOptionIDs(opts) != vanilla1v1Options || opts[0].Label != "Ranked rules" {
		t.Fatalf("MATCH_BEGIN game options = %+v", opts)
	}
	want := []protocol.RankedGamePatch{
		{Offset: 0x02F00100, Bytes: "2222222222222222", Expect: "0000000000000000"},
		{Offset: 0x02F00200, Bytes: "22222222", Expect: "00000000"},
	}
	if fmt.Sprint(opts[0].Patches) != fmt.Sprint(want) {
		t.Fatalf("ranked-rules patches = %+v", opts[0].Patches)
	}
	if !strings.Contains(raw, `"gameOptions":[{"id":"ranked-rules","label":"Ranked rules","patches":[`+
		`{"offset":49283328,"bytes":"2222222222222222","expect":"0000000000000000"},`+
		`{"offset":49283584,"bytes":"22222222","expect":"00000000"}]},`+
		`{"id":"vanilla-only","label":"Vanilla only","patches":[{"offset":49283072,"bytes":"11111111","expect":"00000000"}]},`+
		`{"id":"stages-dream-land","label":"Dream Land only","patches":[{"offset":49284096,"bytes":"44444444","expect":"00000000"}]}]`) {
		t.Fatalf("MATCH_BEGIN JSON: %s", raw)
	}
	// every player gets the same list
	if hostBegin, _ := matchBegin(t, host); fmt.Sprint(hostBegin.Ranked.GameOptions) != fmt.Sprint(opts) {
		t.Fatalf("the host's options differ: %+v", hostBegin.Ranked.GameOptions)
	}
}

// A ruleset without options still sends the field, empty, so clients can
// tell the lobby decided them.
func TestRankedMatchBeginWithoutGameOptions(t *testing.T) {
	f := newRankedFixture(t)
	host, guest := f.connect("host", true), f.connect("guest", true)
	room := f.createRankedRoom(host, &protocol.RankedRoomRequest{Enabled: true})
	f.hub.handleRoomJoin(guest, envelope(t, protocol.RoomJoinRequest{RoomID: room.ID}))
	if room.GameOptionList() != nil {
		t.Fatalf("room options = %+v", room.GameOptionList())
	}
	f.hub.handleRoomStart(host, protocol.Envelope{})
	if _, raw := matchBegin(t, guest); !strings.Contains(raw, `"gameOptions":[]`) {
		t.Fatalf("MATCH_BEGIN JSON: %s", raw)
	}
}

// A config naming options the lobby didn't load against the packs leaves
// those rulesets unplayable rather than unlocked.
func TestRankedWithoutLoadedGameOptionsIsUnavailable(t *testing.T) {
	f := newRankedFixtureWith(t, testOptionsConfig) // UseGameOptions never called
	host := f.connect("host", true)
	if room := f.createRankedRoom(host, &protocol.RankedRoomRequest{Enabled: true}); room != nil {
		t.Fatal("a ranked room without its game options")
	}
	if got := failReason(t, drain(host, protocol.TypeRoomCreateFail)); got != protocol.ReasonRankedUnavailable {
		t.Fatalf("refused with %q", got)
	}
}

// seatTeamRoom seats four players in a 2v2 ranked room: p1 hosts in slot 1,
// p2-p4 take slots 2-4.
func (f *rankedFixture) seatTeamRoom() (*state.Room, []*Client) {
	f.t.Helper()
	players := []*Client{f.connect("p1", true), f.connect("p2", true), f.connect("p3", true), f.connect("p4", true)}
	room := f.createRankedRoom(players[0], &protocol.RankedRoomRequest{Enabled: true, LadderID: "remix-2v2"})
	if room == nil {
		f.t.Fatal("no 2v2 room")
	}
	for _, p := range players[1:] {
		f.hub.handleRoomJoin(p, envelope(f.t, protocol.RoomJoinRequest{RoomID: room.ID}))
	}
	for i, seat := range room.Players {
		if seat.User != players[i].user || seat.Slot != i+1 {
			f.t.Fatalf("seat %d = %s in slot %d", i, seat.User.Username, seat.Slot)
		}
	}
	return room, players
}

// In 2v2, players who pick their teams in the game get the ruleset's options
// only; teams the lobby matched add the option pairing P1 with its teammate.
func TestTeamMatchBeginCarriesTheTeamOption(t *testing.T) {
	f := newRankedOptionsFixture(t)
	room, players := f.seatTeamRoom()
	f.hub.handleRoomStart(players[0], protocol.Envelope{})
	begin, _ := matchBegin(t, players[2])
	if got := rankedOptionIDs(begin.Ranked.GameOptions); got != vanilla2v2Options {
		t.Fatalf("no matched teams: options %s", got)
	}
	for _, p := range begin.Ranked.Players {
		if p.Team != 0 {
			t.Fatalf("no matched teams: %+v", begin.Ranked.Players)
		}
	}
	if room.State != state.RoomStateInGame {
		t.Fatal("the set didn't start")
	}

	for _, tc := range []struct {
		pair     [2]int // slots the lobby matched as teammates
		teamOpt  string
		p1Team   int
		mateSlot int
	}{
		{[2]int{1, 2}, "teams-p1p2", 1, 2},
		{[2]int{1, 3}, "teams-p1p3", 1, 3},
		{[2]int{4, 1}, "teams-p1p4", 1, 4},
		{[2]int{2, 4}, "teams-p1p3", 1, 3}, // P1 plays with whoever isn't in the pair
		{[2]int{3, 4}, "teams-p1p2", 1, 2},
	} {
		f := newRankedOptionsFixture(t)
		room, players := f.seatTeamRoom()
		settings := room.GetRanked()
		settings.Teammates = [][2]uint64{{players[tc.pair[0]-1].user.ID, players[tc.pair[1]-1].user.ID}}
		room.SetRanked(settings)
		f.hub.handleRoomStart(players[0], protocol.Envelope{})
		begin, _ := matchBegin(t, players[3])
		if got := rankedOptionIDs(begin.Ranked.GameOptions); got != vanilla2v2Options+","+tc.teamOpt {
			t.Errorf("pair %v: options %s, want the ruleset's then %s", tc.pair, got, tc.teamOpt)
		}
		teams := map[int]int{}
		for _, p := range begin.Ranked.Players {
			teams[p.Slot] = p.Team
		}
		if teams[1] != tc.p1Team || teams[tc.mateSlot] != tc.p1Team {
			t.Errorf("pair %v: required teams %v", tc.pair, teams)
		}
	}
}

// A chosen team from Quick Ranked: the option follows the slots the team got.
func TestQuickMatchTeamGetsTheTeamOption(t *testing.T) {
	f := newRankedOptionsFixture(t)
	p1, p2, p3, p4 := f.connect("p1", true), f.connect("p2", true), f.connect("p3", true), f.connect("p4", true)
	f.teamSession(p3, "", false)
	f.teamSession(p4, "", false)
	f.teamSession(p1, "p2", false)
	f.teamSession(p2, "p1", false)
	room := p1.user.CurrentRoom
	if room == nil || p4.user.CurrentRoom != room {
		t.Fatal("the team wasn't matched")
	}
	if optionIDs(room.GameOptionList()) != vanilla2v2Options {
		t.Fatalf("quick match room options = %+v", room.GameOptionList())
	}
	for _, p := range []*Client{p1, p2, p3, p4} {
		drainAll(p)
	}
	f.startQuickMatch(room)
	begin, _ := matchBegin(t, p1)
	slotOf := map[string]int{}
	userAt := map[int]string{}
	teamOf := map[int]int{}
	for _, p := range begin.Ranked.Players {
		slotOf[p.Username] = p.Slot
		userAt[p.Slot] = p.Username
		teamOf[p.Slot] = p.Team
	}
	// p1 and p2 are a team, so p3 and p4 are the other
	mate := slotOf[map[string]string{"p1": "p2", "p2": "p1", "p3": "p4", "p4": "p3"}[userAt[1]]]
	if mate < 2 || teamOf[1] == 0 || teamOf[mate] != teamOf[1] {
		t.Fatalf("required teams %v, slots %v", teamOf, slotOf)
	}
	if got, want := rankedOptionIDs(begin.Ranked.GameOptions), fmt.Sprintf("%s,teams-p1p%d", vanilla2v2Options, mate); got != want {
		t.Fatalf("options %s, want %s (slots %v)", got, want, slotOf)
	}
}
