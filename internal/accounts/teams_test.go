package accounts

import (
	"math"
	"testing"

	"github.com/jay-day/rmgk-lobby/internal/rating"
)

const testTeamLadder = "smashremix-2v2"

func TestTeamSetsAreRatedPerPlayer(t *testing.T) {
	s := openTestStore(t)
	var p [4]Account
	for i, name := range []string{"Ace", "Bee", "Cid", "Dee"} {
		p[i], _ = s.Register(name, "password123")
	}
	// P1+P3 (Ace, Cid) against P2+P4 (Bee, Dee); team 1 wins 2-1
	team := func(status SetStatus, winnerTeam int, match uint64) SetRecord {
		return SetRecord{Ladder: testTeamLadder, MatchID: match, BootID: "b", Status: status, WinnerTeam: winnerTeam,
			RulesetID: testVanilla,
			Players: []SetPlayer{
				{AccountID: p[0].ID, Slot: 1, Team: 1, GamesWon: 2}, {AccountID: p[1].ID, Slot: 2, Team: 2, GamesWon: 1},
				{AccountID: p[2].ID, Slot: 3, Team: 1, GamesWon: 2}, {AccountID: p[3].ID, Slot: 4, Team: 2, GamesWon: 1},
			}}
	}
	outcomes, err := s.RecordSet(team(SetRated, 1, 1))
	if err != nil {
		t.Fatal(err)
	}
	// equal new players: winners gain and losers lose the same, as OpenSkill says
	want := rating.UpdateTeams([][]rating.Rating{{rating.Default(), rating.Default()}, {rating.Default(), rating.Default()}}, []int{0, 1})
	for i, o := range outcomes {
		w := want[1][0]
		if i == 0 || i == 2 {
			w = want[0][0]
		}
		if math.Abs(o.After.Rating-w.Rating) > 1e-9 || math.Abs(o.After.Deviation-w.Deviation) > 1e-9 {
			t.Fatalf("player %d after = %+v, want %+v", i, o.After, w)
		}
	}
	if r, _ := s.Rating(p[0].ID, testTeamLadder, testVanilla); r.SetsPlayed != 1 || r.Rating.Rating <= 1500 {
		t.Fatalf("Ace's 2v2 rating = %+v", r)
	}
	if r, _ := s.Rating(p[0].ID, testLadder, testVanilla); r.SetsPlayed != 0 {
		t.Fatalf("2v2 set changed the 1v1 rating: %+v", r)
	}

	board, _ := s.Leaderboard(testTeamLadder, testVanilla)
	for _, e := range board {
		won := e.AccountID == p[0].ID || e.AccountID == p[2].ID
		if won && (e.Wins != 1 || e.Losses != 0) || !won && (e.Wins != 0 || e.Losses != 1) {
			t.Fatalf("record of %d = %d-%d", e.AccountID, e.Wins, e.Losses)
		}
	}

	count := func(f SetFilter) int {
		t.Helper()
		_, total, err := s.FindSets(f)
		if err != nil {
			t.Fatal(err)
		}
		return total
	}
	if count(SetFilter{AccountID: p[2].ID, Result: "win"}) != 1 || count(SetFilter{AccountID: p[3].ID, Result: "loss"}) != 1 ||
		count(SetFilter{AccountID: p[3].ID, Result: "win"}) != 0 {
		t.Fatal("team wins and losses filter wrongly")
	}
	if count(SetFilter{AccountID: p[0].ID, OpponentID: p[1].ID}) != 1 || count(SetFilter{AccountID: p[0].ID, OpponentID: p[2].ID}) != 0 {
		t.Fatal("a teammate counts as an opponent")
	}
	sets, _, _ := s.FindSets(SetFilter{AccountID: p[0].ID})
	if len(sets) != 1 || sets[0].WinnerTeam != 1 || sets[0].Players[2].Team != 1 || sets[0].Players[3].Team != 2 {
		t.Fatalf("set summary = %+v", sets)
	}

	// a disputed team set: the moderator names any player of the winning team
	disputedID, _, err := s.RecordSetWithID(team(SetDisputed, 0, 2))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ResolveSet(disputedID, p[3].ID, "team 2 confirmed"); err != nil {
		t.Fatal(err)
	}
	resolved, _ := s.SetByID(disputedID)
	if resolved.Status != SetRated || resolved.WinnerTeam != 2 || resolved.WinnerID != 0 {
		t.Fatalf("resolved = %+v", resolved)
	}
	if r, _ := s.Rating(p[1].ID, testTeamLadder, testVanilla); r.SetsPlayed != 2 {
		t.Fatalf("Bee's sets after the resolved set = %d", r.SetsPlayed)
	}

	// malformed team results are refused
	bad := team(SetRated, 1, 3)
	bad.Players[1].Team = 1
	if _, err := s.RecordSet(bad); err == nil {
		t.Fatal("accepted three players on one team")
	}
}

// Every game of a team set is rated, in order.
func TestTeamSetRatesEveryGame(t *testing.T) {
	s := openTestStore(t)
	var ids [4]int64
	for i, name := range []string{"Ace", "Bee", "Cid", "Dee"} {
		a, _ := s.Register(name, "password123")
		ids[i] = a.ID
	}
	outcomes, err := s.RecordSet(SetRecord{Ladder: testTeamLadder, MatchID: 1, BootID: "b", Status: SetRated,
		WinnerTeam: 1, GameWinners: []int{1, 2, 1}, RulesetID: testVanilla,
		Players: []SetPlayer{{AccountID: ids[0], Slot: 1, Team: 1}, {AccountID: ids[1], Slot: 2, Team: 2},
			{AccountID: ids[2], Slot: 3, Team: 1}, {AccountID: ids[3], Slot: 4, Team: 2}}})
	if err != nil {
		t.Fatal(err)
	}
	d := rating.Default()
	want := [][]rating.Rating{{d, d}, {d, d}}
	for _, ranks := range [][]int{{0, 1}, {1, 0}, {0, 1}} {
		want = rating.UpdateTeams(want, ranks)
	}
	if math.Abs(outcomes[0].After.Rating-want[0][0].Rating) > 1e-9 || math.Abs(outcomes[1].After.Rating-want[1][0].Rating) > 1e-9 {
		t.Fatalf("after three games = %+v / %+v, want %+v / %+v", outcomes[0].After, outcomes[1].After, want[0][0], want[1][0])
	}
	// three games are more information than one
	single := rating.UpdateTeams([][]rating.Rating{{d, d}, {d, d}}, []int{0, 1})
	if outcomes[0].After.Deviation >= single[0][0].Deviation {
		t.Fatal("three rated games left the deviation no lower than one")
	}
}
