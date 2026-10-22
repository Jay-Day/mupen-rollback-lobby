package ws

import (
	"encoding/json"
	"testing"

	"github.com/jay-day/rmgk-lobby/internal/protocol"
)

// 1v1 and 2v2 ladders on the same ROM
const testTeamsConfig = `{"ladders":[
	{"id":"remix","name":"Smash Remix 1v1","moduleId":"smashremix","rulesets":["remix-1v1-vanilla"],
	 "roms":[{"md5":"` + testRomMD5 + `"}]},
	{"id":"remix-2v2","name":"Smash Remix 2v2","moduleId":"smashremix","teamSize":2,
	 "rulesets":["remix-2v2-vanilla","remix-2v2-remix"],"bestOf":[3,5],"roms":[{"md5":"` + testRomMD5 + `"}]}]}`

// startTeamMatch seats four players in a 2v2 ranked room and starts it.
func startTeamMatch(t *testing.T, f *rankedFixture) ([]*Client, uint64) {
	t.Helper()
	players := []*Client{f.connect("p1", true), f.connect("p2", true), f.connect("p3", true), f.connect("p4", true)}
	room := f.createRankedRoom(players[0], &protocol.RankedRoomRequest{Enabled: true, LadderID: "remix-2v2"})
	if room == nil || room.GetRanked() == nil || room.GetRanked().TeamSize != 2 || room.MaxPlayers != 4 {
		t.Fatalf("2v2 room = %+v", room)
	}
	// three players can't start a 2v2 set
	for _, p := range players[1:3] {
		f.hub.handleRoomJoin(p, envelope(t, protocol.RoomJoinRequest{RoomID: room.ID}))
	}
	f.hub.handleRoomStart(players[0], protocol.Envelope{})
	if len(drain(players[1], protocol.TypeMatchBegin)) != 0 {
		t.Fatal("a 2v2 set started with three players")
	}
	f.hub.handleRoomJoin(players[3], envelope(t, protocol.RoomJoinRequest{RoomID: room.ID}))
	f.hub.handleRoomStart(players[0], protocol.Envelope{})
	begins := drain(players[3], protocol.TypeMatchBegin)
	if len(begins) != 1 {
		t.Fatalf("MATCH_BEGIN count = %d", len(begins))
	}
	var begin protocol.MatchBeginPayload
	json.Unmarshal(begins[0].Data, &begin)
	if begin.Ranked == nil || begin.Ranked.TeamSize != 2 || begin.Ranked.Ladder != "remix-2v2" || len(begin.Ranked.Players) != 4 {
		t.Fatalf("MATCH_BEGIN ranked = %+v", begin.Ranked)
	}
	for _, p := range players {
		drain(p, protocol.TypeMatchBegin)
	}
	return players, begin.MatchID
}

// teamResult: slots 1+3 against 2+4, winner team 1 or 2, Bo3
func teamResult(matchID uint64, teams []int, winnerTeam int) protocol.MatchResultRequest {
	score := make([]int, 4)
	for slot, team := range teams {
		if team == winnerTeam {
			score[slot] = 2
		} else {
			score[slot] = 1
		}
	}
	games := []protocol.MatchResultGame{
		{Game: 1, WinnerTeam: winnerTeam, Stage: 6}, {Game: 2, WinnerTeam: 3 - winnerTeam, Stage: 6},
		{Game: 3, WinnerTeam: winnerTeam, Stage: 6},
	}
	return protocol.MatchResultRequest{MatchID: matchID, Score: score, Teams: teams, WinnerTeam: winnerTeam,
		Games: games, RulesetHash: "abc", Integrity: "ok"}
}

func TestTeamSetRated(t *testing.T) {
	f := newRankedFixtureWith(t, testTeamsConfig)
	players, matchID := startTeamMatch(t, f)
	for i, p := range players {
		f.hub.handleMatchResult(p, envelope(t, teamResult(matchID, []int{1, 2, 1, 2}, 2)))
		if i < 3 && len(drain(players[0], protocol.TypeRankedResult)) != 0 {
			t.Fatalf("finalized after %d reports", i+1)
		}
	}
	res := rankedResultFor(t, players[0])
	if res.Status != "rated" || res.WinnerTeam != 2 || res.WinnerSlot != 0 || len(res.Players) != 4 {
		t.Fatalf("result = %+v", res)
	}
	for _, p := range res.Players {
		won := p.Slot == 2 || p.Slot == 4
		if p.Team != map[bool]int{true: 2, false: 1}[won] || won != (p.After.Rating > p.Before.Rating) || p.After.Ladder != "remix-2v2" {
			t.Fatalf("player %+v", p)
		}
	}
	// ratings moved on the 2v2 ladder only
	if r, _ := f.store.Rating(players[1].user.AccountID, "remix-2v2", "remix-2v2-vanilla"); r.SetsPlayed != 1 {
		t.Fatalf("2v2 rating = %+v", r)
	}
	if r, _ := f.store.Rating(players[1].user.AccountID, "remix", "remix-1v1-vanilla"); r.SetsPlayed != 0 {
		t.Fatalf("1v1 rating = %+v", r)
	}
	sets, _ := f.store.RecentSets("remix-2v2", "", 0, 5)
	if len(sets) != 1 || sets[0].WinnerTeam != 2 || sets[0].Players[1].Team != 2 {
		t.Fatalf("recorded = %+v", sets)
	}
}

func TestTeamSetDisputes(t *testing.T) {
	cases := []struct {
		name    string
		reports [4]protocol.MatchResultRequest
		reason  string
	}{
		{"one player names other partners", [4]protocol.MatchResultRequest{
			teamResult(0, []int{1, 2, 1, 2}, 1), teamResult(0, []int{1, 2, 1, 2}, 1),
			teamResult(0, []int{1, 1, 2, 2}, 1), teamResult(0, []int{1, 2, 1, 2}, 1)}, "reports_disagree"},
		{"three against one", [4]protocol.MatchResultRequest{
			teamResult(0, []int{1, 1, 1, 2}, 1), teamResult(0, []int{1, 1, 1, 2}, 1),
			teamResult(0, []int{1, 1, 1, 2}, 1), teamResult(0, []int{1, 1, 1, 2}, 1)}, "invalid_teams"},
		{"team 1 isn't the lowest slot's", [4]protocol.MatchResultRequest{
			teamResult(0, []int{2, 1, 2, 1}, 1), teamResult(0, []int{2, 1, 2, 1}, 1),
			teamResult(0, []int{2, 1, 2, 1}, 1), teamResult(0, []int{2, 1, 2, 1}, 1)}, "invalid_teams"},
	}
	for _, tc := range cases {
		f := newRankedFixtureWith(t, testTeamsConfig)
		players, matchID := startTeamMatch(t, f)
		for i, p := range players {
			r := tc.reports[i]
			r.MatchID = matchID
			f.hub.handleMatchResult(p, envelope(t, r))
		}
		if res := rankedResultFor(t, players[0]); res.Status != "disputed" || res.Reason != tc.reason {
			t.Errorf("%s: result = %s %s", tc.name, res.Status, res.Reason)
		}
	}
}

func TestTeamLadderInHello(t *testing.T) {
	f := newRankedFixtureWith(t, testTeamsConfig)
	ladders := f.hub.ladderInfo()
	if len(ladders) != 2 || ladders[0].TeamSize != 1 || ladders[1].TeamSize != 2 {
		t.Fatalf("ladders = %+v", ladders)
	}
	// a 1v1 request on the same ROM still makes a two-seat room
	host := f.connect("solo", true)
	room := f.createRankedRoom(host, &protocol.RankedRoomRequest{Enabled: true})
	if room.GetRanked().LadderID != "remix" || room.MaxPlayers != 2 {
		t.Fatalf("1v1 room = %+v", room.GetRanked())
	}
}
