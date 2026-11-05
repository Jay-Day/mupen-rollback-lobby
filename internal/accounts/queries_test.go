package accounts

import (
	"errors"
	"testing"
)

func TestLeaderboardAndRecentSets(t *testing.T) {
	s := openTestStore(t)
	a, _ := s.Register("Alice", "password123")
	b, _ := s.Register("bob", "password123")
	c, _ := s.Register("carol", "password123") // never plays

	record := func(match uint64, winner int64, status SetStatus) {
		t.Helper()
		_, err := s.RecordSet(SetRecord{
			Ladder: "remix", MatchID: match, BootID: "boot", Status: status, WinnerID: winner,
			ResultJSON: `{"x":1}`,
			Players:    []SetPlayer{{AccountID: a.ID, Slot: 1, GamesWon: 2}, {AccountID: b.ID, Slot: 2, GamesWon: 1}},
		})
		if err != nil {
			t.Fatalf("RecordSet: %v", err)
		}
	}
	record(1, a.ID, SetRated)
	record(2, a.ID, SetRated)
	record(3, b.ID, SetRated)
	record(4, 0, SetVoided)

	board, err := s.Leaderboard("remix", "")
	if err != nil {
		t.Fatalf("Leaderboard: %v", err)
	}
	if len(board) != 2 {
		t.Fatalf("leaderboard has %d players, want 2 (carol never played)", len(board))
	}
	for _, e := range board {
		switch e.Username {
		case "Alice":
			if e.Wins != 2 || e.Losses != 1 || e.SetsPlayed != 3 {
				t.Fatalf("Alice = %+v", e)
			}
		case "bob":
			if e.Wins != 1 || e.Losses != 2 {
				t.Fatalf("bob = %+v", e)
			}
		default:
			t.Fatalf("unexpected player %q", e.Username)
		}
	}

	sets, err := s.RecentSets("remix", "", 0, 10)
	if err != nil {
		t.Fatalf("RecentSets: %v", err)
	}
	if len(sets) != 4 || sets[0].Status != SetVoided || len(sets[0].Players) != 2 {
		t.Fatalf("recent sets = %+v", sets)
	}
	if sets[1].WinnerID != b.ID || sets[1].Players[0].Username != "Alice" {
		t.Fatalf("set 3 = %+v", sets[1])
	}

	mine, _ := s.RecentSets("", "", c.ID, 10)
	if len(mine) != 0 {
		t.Fatalf("carol has %d sets", len(mine))
	}

	if acct, err := s.AccountByName("ALICE"); err != nil || acct.ID != a.ID {
		t.Fatalf("AccountByName = %+v, %v", acct, err)
	}
	if _, err := s.AccountByName("nobody"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing account err = %v", err)
	}
	ratings, err := s.Ratings(a.ID)
	if err != nil || len(ratings) != 1 || ratings[0].SetsPlayed != 3 {
		t.Fatalf("Ratings = %+v, %v", ratings, err)
	}
}
