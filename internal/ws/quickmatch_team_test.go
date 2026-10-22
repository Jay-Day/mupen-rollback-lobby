package ws

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jay-day/rmgk-lobby/internal/protocol"
	"github.com/jay-day/rmgk-lobby/internal/state"
)

var teamSessionRequest = &protocol.RankedRoomRequest{Enabled: true, LadderID: "remix-2v2", BestOf: 3}

// teamSession searches for a 2v2 session, with a random teammate unless
// one is asked for
func (f *rankedFixture) teamSession(c *Client, teammate string, keepTeammate bool) {
	f.t.Helper()
	f.teamSearch(c, teamSessionRequest, teammate, keepTeammate)
}

func (f *rankedFixture) teamSearch(c *Client, ranked *protocol.RankedRoomRequest, teammate string, keepTeammate bool) {
	f.t.Helper()
	f.hub.handleQuickMatchJoin(c, envelope(f.t, protocol.QuickMatchJoinRequest{
		Rom: protocol.RomInfo{Name: "Remix", MD5: testRomMD5}, Ranked: ranked, Continuous: true,
		Teammate: teammate, KeepTeammate: keepTeammate,
	}))
}

func (f *rankedFixture) keepTeammate(c *Client, keep bool) {
	f.t.Helper()
	f.hub.handleQuickMatchTeammate(c, envelope(f.t, protocol.QuickMatchTeammateRequest{Keep: keep}))
}

// playTeamSet starts a paired 2v2 room and reports a Bo3 that a and b win
// as one team.
func (f *rankedFixture) playTeamSet(room *state.Room, a, b *Client, players ...*Client) {
	f.t.Helper()
	slots := map[uint64]int{}
	lowest := 5
	for _, seat := range room.Players {
		slots[seat.User.ID] = seat.Slot
		lowest = min(lowest, seat.Slot)
	}
	// team 1 holds the lowest slot
	abFirst := slots[a.user.ID] == lowest || slots[b.user.ID] == lowest
	teams := make([]int, 4)
	for id, slot := range slots {
		if (id == a.user.ID || id == b.user.ID) == abFirst {
			teams[slot-1] = 1
		} else {
			teams[slot-1] = 2
		}
	}
	matchID := f.startQuickMatch(room)
	for _, p := range players {
		f.hub.handleMatchResult(p, envelope(f.t, teamResult(matchID, teams, teams[slots[a.user.ID]-1])))
	}
}

func (f *rankedFixture) stopAfterThisSet(c *Client) {
	f.t.Helper()
	f.hub.handleQuickMatchSession(c, envelope(f.t, protocol.QuickMatchSessionRequest{Continuous: false}))
}

func partnerOf(f *rankedFixture, c *Client) uint64 {
	e, _ := f.hub.matchmaker.find(c.user.ID)
	return e.partner
}

func noticesIn(msgs []protocol.Envelope) []string {
	var out []string
	for _, env := range ofType(msgs, protocol.TypeModNotice) {
		var p protocol.ModNoticePayload
		json.Unmarshal(env.Data, &p)
		out = append(out, p.Text)
	}
	return out
}

func roomChatIn(msgs []protocol.Envelope) []string {
	var out []string
	for _, env := range ofType(msgs, protocol.TypeChatMsg) {
		var p protocol.ChatMsgPayload
		json.Unmarshal(env.Data, &p)
		if p.FromUserID == 0 {
			out = append(out, p.Message)
		}
	}
	return out
}

func lastStatusIn(msgs []protocol.Envelope) protocol.QuickMatchStatusPayload {
	var out protocol.QuickMatchStatusPayload
	if statuses := ofType(msgs, protocol.TypeQuickMatchStatus); len(statuses) > 0 {
		json.Unmarshal(statuses[len(statuses)-1].Data, &out)
	}
	return out
}

func TestTeamSessionKeepsTeammate(t *testing.T) {
	fastSessions(t)
	f := newRankedFixtureWith(t, testTeamsConfig)
	p1, p2, p3, p4 := f.connect("p1", true), f.connect("p2", true), f.connect("p3", true), f.connect("p4", true)
	for _, p := range []*Client{p1, p2, p3, p4} {
		f.teamSession(p, "", false)
	}
	room := p1.user.CurrentRoom
	if room == nil || p4.user.CurrentRoom != room {
		t.Fatal("2v2 session searchers didn't pair")
	}
	// random teammates: keeping one is turned on in the room
	for _, p := range []*Client{p1, p3, p4} {
		f.keepTeammate(p, true)
	}
	// only p1 and p3 search again; p4 keeps the team but p2 didn't, and p2
	// ends the session
	f.stopAfterThisSet(p2)
	f.stopAfterThisSet(p4)
	drainAll(p1)
	f.playTeamSet(room, p1, p3, p1, p2, p3, p4)
	waitFor(t, "the kept team searching again", func() bool {
		return f.hub.matchmaker.contains(p1.user.ID) && f.hub.matchmaker.contains(p3.user.ID)
	})
	if partnerOf(f, p1) != p3.user.ID || partnerOf(f, p3) != p1.user.ID {
		t.Fatalf("partners = %d / %d", partnerOf(f, p1), partnerOf(f, p3))
	}
	msgs := drainAll(p1)
	if chat := roomChatIn(msgs); !containsString(chat, "p1 and p3 keep their team for the next set.") {
		t.Fatalf("room chat = %v", chat)
	}
	if status := lastStatusIn(msgs); !status.Searching || !status.KeepTeammate || status.Teammate != "p3" {
		t.Fatalf("p1's status = %+v", status)
	}

	// three searchers aren't a match, even though the kept team fits two seats
	n1, n2 := f.connect("n1", true), f.connect("n2", true)
	f.teamSession(n1, "", false)
	if n1.user.CurrentRoom != nil {
		t.Fatal("matched three players")
	}
	f.teamSession(n2, "", false)
	next := n2.user.CurrentRoom
	if next == nil || p1.user.CurrentRoom != next || p3.user.CurrentRoom != next || n1.user.CurrentRoom != next {
		t.Fatal("the kept team wasn't matched with the two new players")
	}
	if !strings.Contains(next.Name, " vs ") ||
		!(strings.Contains(next.Name, "p1 & p3") || strings.Contains(next.Name, "p3 & p1")) {
		t.Fatalf("room name %q", next.Name)
	}
	if chat := roomChatIn(drainAll(n1)); len(chat) < 2 || !strings.HasPrefix(chat[1], "Teams: ") {
		t.Fatalf("n1's room chat = %v", chat)
	}
}

func TestTeamSessionOneSidedKeepAndToggles(t *testing.T) {
	fastSessions(t)
	f := newRankedFixtureWith(t, testTeamsConfig)
	p1, p2, p3, p4 := f.connect("p1", true), f.connect("p2", true), f.connect("p3", true), f.connect("p4", true)
	for _, p := range []*Client{p1, p2, p3, p4} {
		f.teamSession(p, "", false)
	}
	room := p1.user.CurrentRoom
	f.keepTeammate(p1, true)
	f.stopAfterThisSet(p2)
	f.stopAfterThisSet(p4)
	f.playTeamSet(room, p1, p3, p1, p2, p3, p4)
	waitFor(t, "p1 and p3 searching again", func() bool {
		return f.hub.matchmaker.contains(p1.user.ID) && f.hub.matchmaker.contains(p3.user.ID)
	})
	// p3 didn't keep the team: both search on their own
	if partnerOf(f, p1) != 0 || partnerOf(f, p3) != 0 {
		t.Fatal("a team was kept with only one player keeping it")
	}
	drainAll(p1)
	drainAll(p3)

	// p3 keeps it after all, between sets: the team is kept at once
	f.keepTeammate(p3, true)
	if partnerOf(f, p1) != p3.user.ID || partnerOf(f, p3) != p1.user.ID {
		t.Fatal("keeping the teammate between sets didn't keep the team")
	}
	if notices := noticesIn(drainAll(p1)); !containsString(notices, "p3 and p1 keep their team for the next set.") {
		t.Fatalf("p1's notices = %v", notices)
	}
	if status := lastStatusIn(drainAll(p3)); status.Teammate != "p1" || !status.KeepTeammate {
		t.Fatalf("p3's status = %+v", status)
	}

	// p1 turns it off: the team splits
	f.keepTeammate(p1, false)
	if partnerOf(f, p1) != 0 || partnerOf(f, p3) != 0 {
		t.Fatal("turning keep teammate off didn't split the team")
	}
	msgs := drainAll(p3)
	if notices := noticesIn(msgs); !containsString(notices, "p1 isn't keeping the team, so you'll search on your own.") {
		t.Fatalf("p3's notices = %v", notices)
	}
	if status := lastStatusIn(msgs); status.Teammate != "" || !status.Searching {
		t.Fatalf("p3's status = %+v", status)
	}

	// back on, then p3 cancels: p1 goes on alone
	f.keepTeammate(p1, true)
	if partnerOf(f, p1) != p3.user.ID {
		t.Fatal("the team wasn't kept again")
	}
	drainAll(p1)
	f.hub.handleQuickMatchCancel(p3, protocol.Envelope{})
	if partnerOf(f, p1) != 0 || !f.hub.matchmaker.contains(p1.user.ID) {
		t.Fatal("p1 isn't searching on their own after p3 cancelled")
	}
	if notices := noticesIn(drainAll(p1)); !containsString(notices, "p3 isn't searching with you anymore, so you'll search on your own.") {
		t.Fatalf("p1's notices = %v", notices)
	}
}

func TestTeamSessionWarmupLeaveSplitsTeam(t *testing.T) {
	f := newRankedFixtureWith(t, testTeamsConfig)
	p1, p2, p3, p4 := f.connect("p1", true), f.connect("p2", true), f.connect("p3", true), f.connect("p4", true)
	for _, p := range []*Client{p2, p4, p1} {
		f.teamSession(p, "", false)
	}
	// p1 and p3 as a kept team (as after a set): half of it isn't matched
	f.hub.matchmaker.update(p1.user.ID, func(e *qmEntry) { e.partner = p3.user.ID })
	f.teamSession(p3, "", false)
	if p3.user.CurrentRoom != nil {
		t.Fatal("matched half a kept team")
	}
	f.hub.matchmaker.update(p3.user.ID, func(e *qmEntry) { e.partner = p1.user.ID })
	f.hub.quickMatchAttempt(p3.user)
	room := p1.user.CurrentRoom
	if room == nil || p3.user.CurrentRoom != room {
		t.Fatal("the kept team wasn't matched")
	}
	if !strings.Contains(room.Name, "p1 & p3") && !strings.Contains(room.Name, "p3 & p1") {
		t.Fatalf("room name %q", room.Name)
	}
	drainAll(p1)
	// p3 leaves during the warmup: p1 searches again on their own
	f.hub.handleRoomLeave(p3, protocol.Envelope{})
	if !f.hub.matchmaker.contains(p1.user.ID) || partnerOf(f, p1) != 0 {
		t.Fatal("p1 isn't searching on their own")
	}
	if notices := noticesIn(drainAll(p1)); !containsString(notices, "p3 left before the match started. Searching again on your own.") {
		t.Fatalf("p1's notices = %v", notices)
	}
}

func TestKeepTeammateIgnoredIn1v1(t *testing.T) {
	f := newRankedFixtureWith(t, testTeamsConfig)
	a := f.connect("alice", true)
	f.hub.handleQuickMatchJoin(a, envelope(t, protocol.QuickMatchJoinRequest{
		Rom: protocol.RomInfo{Name: "Remix", MD5: testRomMD5}, Ranked: &protocol.RankedRoomRequest{Enabled: true},
		Continuous: true, KeepTeammate: true,
	}))
	if e, ok := f.hub.matchmaker.entry(a.user.ID); !ok || e.keepTeammate {
		t.Fatalf("1v1 search = %+v", e)
	}
	f.keepTeammate(a, true)
	if e, _ := f.hub.matchmaker.entry(a.user.ID); e.keepTeammate {
		t.Fatal("a 1v1 search kept a teammate")
	}
}

// pickGroup with kept teams, on hand-made searches
func TestPickGroupKeptTeams(t *testing.T) {
	settings := &state.RankedSettings{LadderID: "remix-2v2", RulesetID: "remix-2v2-vanilla", BestOf: 3, TeamSize: 2}
	base := time.Now()
	pings := map[uint64]uint16{}
	setup := func(entries map[uint64]qmEntry) *matchmaker {
		m := newMatchmaker()
		for id, e := range entries {
			e.rom, e.ranked = protocol.RomInfo{MD5: testRomMD5}, settings
			e.enqueuedAt = base.Add(time.Duration(id) * time.Millisecond)
			m.add(id, e)
		}
		return m
	}
	ping := func(id uint64) uint16 { return pings[id] }
	// a team's two players come in either order
	sorted := func(ids []uint64) []uint64 {
		slices.Sort(ids)
		return ids
	}

	// a kept team waits for its other player
	m := setup(map[uint64]qmEntry{1: {partner: 2}, 3: {}, 4: {}, 5: {}})
	if got := m.pickGroup(1, 0, ping, false); got != nil {
		t.Fatalf("matched without the teammate: %v", got)
	}
	// and nobody takes half of it
	if got := m.pickGroup(3, 0, ping, false); got != nil {
		t.Fatalf("took half a team: %v", got)
	}

	// two kept teams
	m = setup(map[uint64]qmEntry{1: {partner: 2}, 2: {partner: 1}, 3: {partner: 4}, 4: {partner: 3}})
	if got := sorted(m.pickGroup(1, 0, ping, false)); !reflect.DeepEqual(got, []uint64{2, 3, 4}) {
		t.Fatalf("two teams: %v", got)
	}

	// a kept team's players needn't accept each other, but everyone else must
	m = setup(map[uint64]qmEntry{1: {partner: 2, tooFar: map[uint64]bool{2: true}}, 2: {partner: 1}, 3: {}, 4: {tooFar: map[uint64]bool{1: true}}, 5: {}})
	if got := m.pickGroup(1, 0, ping, false); !reflect.DeepEqual(got, []uint64{2, 3, 5}) {
		t.Fatalf("team and two others: %v", got)
	}

	// the first combination that fills the match, where taking each one
	// that fits would get stuck: 2, 3 and then 4 (too far from 2) and the
	// team (two seats for one) don't fit, but 2 and the team do
	pings = map[uint64]uint16{2: 10, 3: 20, 4: 30, 5: 40, 6: 40}
	m = setup(map[uint64]qmEntry{1: {}, 2: {}, 3: {}, 4: {tooFar: map[uint64]bool{2: true}}, 5: {partner: 6}, 6: {partner: 5}})
	if got := sorted(m.pickGroup(1, 0, ping, false)); !reflect.DeepEqual(got, []uint64{2, 5, 6}) {
		t.Fatalf("backtracking: %v", got)
	}
}

// beginTeams starts a paired room's set and returns each username's
// required team from MATCH_BEGIN.
func (f *rankedFixture) beginTeams(room *state.Room, players ...*Client) (uint64, map[string]int) {
	f.t.Helper()
	for _, p := range players {
		drainAll(p)
	}
	matchID := f.startQuickMatch(room)
	begins := drain(players[0], protocol.TypeMatchBegin)
	if len(begins) != 1 {
		f.t.Fatalf("MATCH_BEGIN count = %d", len(begins))
	}
	var begin protocol.MatchBeginPayload
	json.Unmarshal(begins[0].Data, &begin)
	teams := map[string]int{}
	for _, p := range begin.Ranked.Players {
		teams[p.Username] = p.Team
	}
	return matchID, teams
}

func TestTeammateRequestNeedsBothPlayers(t *testing.T) {
	f := newRankedFixtureWith(t, testTeamsConfig)
	p1, p2, p3, p4 := f.connect("p1", true), f.connect("p2", true), f.connect("p3", true), f.connect("p4", true)
	f.teamSession(p3, "", false)
	f.teamSession(p4, "", false)
	f.teamSession(p1, "p2", true)
	if status := lastStatusIn(drainAll(p1)); !status.Searching || status.WaitingFor != "p2" || !status.KeepTeammate {
		t.Fatalf("p1's status = %+v", status)
	}
	// p2 searches with a random teammate: p1 still waits, and p2 is told
	f.teamSession(p2, "", false)
	if p1.user.CurrentRoom != nil || p2.user.CurrentRoom != nil {
		t.Fatal("matched although p2 didn't ask for p1")
	}
	if notices := noticesIn(drainAll(p2)); !containsString(notices,
		"p1 wants you as their 2v2 teammate. To play together, search for a Quick Ranked Match with p1 as your teammate.") {
		t.Fatalf("p2's notices = %v", notices)
	}

	// p2 asks for p1 back (any casing): the team forms and gets p3 and p4
	f.hub.handleQuickMatchCancel(p2, protocol.Envelope{})
	f.teamSession(p2, "P1", false)
	room := p1.user.CurrentRoom
	if room == nil || p2.user.CurrentRoom != room || p3.user.CurrentRoom != room || p4.user.CurrentRoom != room {
		t.Fatal("the asked-for team wasn't matched")
	}
	if notices := noticesIn(drainAll(p1)); !containsString(notices, "You and p2 are searching as a team.") {
		t.Fatalf("p1's notices = %v", notices)
	}
	if !strings.Contains(room.Name, "p1 & p2") && !strings.Contains(room.Name, "p2 & p1") {
		t.Fatalf("room name %q", room.Name)
	}
	if mates := room.GetRanked().Teammates; len(mates) != 1 {
		t.Fatalf("room teammates = %v", mates)
	}

	// the set requires p1 and p2 on one team, team 1 holding slot 1
	_, teams := f.beginTeams(room, p1, p2, p3, p4)
	if teams["p1"] == 0 || teams["p1"] != teams["p2"] || teams["p3"] != teams["p4"] || teams["p3"] == teams["p1"] {
		t.Fatalf("required teams = %v", teams)
	}
	for _, seat := range room.Players {
		if seat.Slot == 1 && teams[seat.User.Username] != 1 {
			t.Fatalf("slot 1 (%s) isn't team 1: %v", seat.User.Username, teams)
		}
	}
}

func TestTeammateRequestChecks(t *testing.T) {
	f := newRankedFixtureWith(t, testTeamsConfig)
	p1, p2 := f.connect("p1", true), f.connect("p2", true)
	for _, ask := range []struct{ name, notice string }{
		{"P1", "You can't pick yourself as your teammate."},
		{"nobody", "There's no account named nobody to team up with."},
	} {
		f.teamSession(p1, ask.name, false)
		if f.hub.matchmaker.contains(p1.user.ID) {
			t.Fatalf("searching with teammate %q", ask.name)
		}
		if notices := noticesIn(drainAll(p1)); !containsString(notices, ask.notice) {
			t.Fatalf("notices for %q = %v", ask.name, notices)
		}
	}
	// keeping the teammate from the start needs a chosen teammate
	f.teamSession(p1, "", true)
	if e, _ := f.hub.matchmaker.entry(p1.user.ID); e.keepTeammate {
		t.Fatal("kept a random teammate from the form")
	}
	f.hub.handleQuickMatchCancel(p1, protocol.Envelope{})

	// asking each other for different sets: both are told, nothing forms
	f.teamSession(p1, "p2", false)
	f.teamSearch(p2, &protocol.RankedRoomRequest{Enabled: true, LadderID: "remix-2v2", BestOf: 5}, "p1", false)
	if e, _ := f.hub.matchmaker.entry(p1.user.ID); e.partner != 0 {
		t.Fatal("teamed up across set lengths")
	}
	want := "%s wants you as their teammate too, but you're searching for different sets (format, roster or set length). Search for the same one to play together."
	if notices := noticesIn(drainAll(p1)); !containsString(notices, strings.Replace(want, "%s", "p2", 1)) {
		t.Fatalf("p1's notices = %v", notices)
	}
	if notices := noticesIn(drainAll(p2)); !containsString(notices, strings.Replace(want, "%s", "p1", 1)) {
		t.Fatalf("p2's notices = %v", notices)
	}
}

func TestTeammateRequestWaitsAgainWhenTeammateCancels(t *testing.T) {
	f := newRankedFixtureWith(t, testTeamsConfig)
	p1, p2 := f.connect("p1", true), f.connect("p2", true)
	f.teamSession(p1, "p2", false)
	f.teamSession(p2, "p1", false)
	if e, _ := f.hub.matchmaker.entry(p1.user.ID); e.partner != p2.user.ID {
		t.Fatal("the team didn't form")
	}
	drainAll(p1)
	f.hub.handleQuickMatchCancel(p2, protocol.Envelope{})
	msgs := drainAll(p1)
	if notices := noticesIn(msgs); !containsString(notices,
		"p2 stopped searching. Your search waits until they search with you as their teammate again.") {
		t.Fatalf("p1's notices = %v", notices)
	}
	if status := lastStatusIn(msgs); !status.Searching || status.WaitingFor != "p2" || status.Teammate != "" {
		t.Fatalf("p1's status = %+v", status)
	}
	// two players on their own don't get p1
	p3, p4 := f.connect("p3", true), f.connect("p4", true)
	f.teamSession(p3, "", false)
	f.teamSession(p4, "", false)
	f.teamSession(f.connect("p5", true), "", false)
	if p1.user.CurrentRoom != nil {
		t.Fatal("p1 was matched without p2")
	}
}

func TestRequiredTeamsJudged(t *testing.T) {
	for _, tc := range []struct {
		name   string
		swap   bool
		status string
	}{
		{"as matched", false, "rated"},
		{"a partner and an opponent swapped", true, "disputed"},
	} {
		f := newRankedFixtureWith(t, testTeamsConfig)
		p1, p2, p3, p4 := f.connect("p1", true), f.connect("p2", true), f.connect("p3", true), f.connect("p4", true)
		f.teamSession(p1, "p2", false)
		f.teamSession(p2, "p1", false)
		f.teamSession(p3, "", false)
		f.teamSession(p4, "", false)
		room := p1.user.CurrentRoom
		matchID, required := f.beginTeams(room, p1, p2, p3, p4)
		slots := map[string]int{}
		for _, seat := range room.Players {
			slots[seat.User.Username] = seat.Slot
		}
		teams := make([]int, 4)
		for name, slot := range slots {
			teams[slot-1] = required[name]
		}
		if tc.swap {
			// a partner and an opponent trade teams, neither in slot 1 (so
			// team 1 still holds it and only the pairing is wrong)
			partner, opponent := "p2", "p3"
			if slots[partner] == 1 {
				partner = "p1"
			}
			if slots[opponent] == 1 {
				opponent = "p4"
			}
			a, b := slots[partner]-1, slots[opponent]-1
			teams[a], teams[b] = teams[b], teams[a]
		}
		for _, p := range []*Client{p1, p2, p3, p4} {
			f.hub.handleMatchResult(p, envelope(t, teamResult(matchID, teams, 1)))
		}
		res := rankedResultFor(t, p1)
		if res.Status != tc.status || (tc.status == "disputed" && res.Reason != "teams_not_as_matched") {
			t.Errorf("%s: result = %s %s (teams %v, required %v)", tc.name, res.Status, res.Reason, teams, required)
		}
	}
}

func TestRequiredTeams(t *testing.T) {
	settings := &state.RankedSettings{TeamSize: 2}
	slots := map[uint64]int{10: 1, 11: 2, 12: 3, 13: 4}
	for _, tc := range []struct {
		pairs [][2]uint64
		want  [4]int
	}{
		{nil, [4]int{}},
		{[][2]uint64{{10, 12}}, [4]int{1, 2, 1, 2}},
		{[][2]uint64{{11, 12}}, [4]int{1, 2, 2, 1}},
		{[][2]uint64{{11, 13}, {10, 12}}, [4]int{1, 2, 1, 2}},
		{[][2]uint64{{10, 99}}, [4]int{}}, // not seated
	} {
		settings.Teammates = tc.pairs
		if got := requiredTeams(settings, slots); got != tc.want {
			t.Errorf("pairs %v: teams %v, want %v", tc.pairs, got, tc.want)
		}
	}
}

func containsString(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
