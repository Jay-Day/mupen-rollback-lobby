package accounts

import (
	"database/sql"
	"math"
	"path/filepath"
	"testing"
	"time"
)

const (
	testLadder  = "smashremix-1v1"
	testVanilla = "remix-1v1-vanilla"
	testRemix   = "remix-1v1-remix"
)

func TestRostersAreRatedSeparately(t *testing.T) {
	s := openTestStore(t)
	a, _ := s.Register("Ace", "password123")
	b, _ := s.Register("Bee", "password123")
	set := func(match uint64, ruleset string, winner int64) {
		t.Helper()
		_, err := s.RecordSet(SetRecord{Ladder: testLadder, MatchID: match, BootID: "boot", Status: SetRated,
			WinnerID: winner, RulesetID: ruleset,
			Players: []SetPlayer{{AccountID: a.ID, Slot: 1, GamesWon: 2}, {AccountID: b.ID, Slot: 2, GamesWon: 1}}})
		if err != nil {
			t.Fatal(err)
		}
	}
	set(1, testVanilla, a.ID)
	set(2, testVanilla, a.ID)
	set(3, testRemix, b.ID)

	vanilla, _ := s.Rating(a.ID, testLadder, testVanilla)
	remix, _ := s.Rating(a.ID, testLadder, testRemix)
	if vanilla.SetsPlayed != 2 || vanilla.Rating.Rating <= 1500 {
		t.Fatalf("vanilla = %+v", vanilla)
	}
	if remix.SetsPlayed != 1 || remix.Rating.Rating >= 1500 {
		t.Fatalf("remix = %+v, want one loss from the default rating", remix)
	}

	board, err := s.Leaderboard(testLadder, testVanilla)
	if err != nil || len(board) != 2 {
		t.Fatalf("vanilla board = %+v, %v", board, err)
	}
	for _, e := range board {
		if e.AccountID == a.ID && (e.Wins != 2 || e.Losses != 0) || e.AccountID == b.ID && (e.Wins != 0 || e.Losses != 2) {
			t.Fatalf("vanilla record counts the other roster: %+v", e)
		}
	}
	remixBoard, _ := s.Leaderboard(testLadder, testRemix)
	for _, e := range remixBoard {
		if e.AccountID == a.ID && (e.Wins != 0 || e.Losses != 1) {
			t.Fatalf("remix record = %+v", e)
		}
	}

	ratings, _ := s.Ratings(a.ID)
	if len(ratings) != 2 || ratings[0].Ruleset != testRemix || ratings[1].Ruleset != testVanilla {
		t.Fatalf("ratings = %+v", ratings)
	}
	history, _ := s.RatingHistory(a.ID)
	if len(history) != 3 || history[2].Ruleset != testRemix {
		t.Fatalf("history = %+v", history)
	}
}

// A database from before migration 5 (one rating per ladder) gets one rating
// per roster, equal to rating its history roster by roster.
func TestMigrationRebuildsRatingsByRoster(t *testing.T) {
	type played struct {
		ruleset  string
		winner   int // 0 = Ace, 1 = Bee
		status   SetStatus
		daysLate int
	}
	history := []played{
		{testVanilla, 0, SetRated, 0}, {testRemix, 1, SetRated, 1}, {testVanilla, 0, SetRated, 2},
		{testVanilla, 1, SetVoided, 3}, {testRemix, 1, SetRated, 40}, {testVanilla, 1, SetRated, 41},
	}
	start := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	// expected: the current code recording the same history
	want := openTestStore(t)
	wa, _ := want.Register("Ace", "password123")
	wb, _ := want.Register("Bee", "password123")
	for i, h := range history {
		at := start.Add(time.Duration(h.daysLate) * 24 * time.Hour)
		want.SetClock(func() time.Time { return at })
		winner := []int64{wa.ID, wb.ID}[h.winner]
		if h.status != SetRated {
			winner = 0
		}
		if _, err := want.RecordSet(SetRecord{Ladder: testLadder, MatchID: uint64(i), BootID: "b", Status: h.status,
			WinnerID: winner, RulesetID: h.ruleset,
			Players: []SetPlayer{{AccountID: wa.ID, Slot: 1}, {AccountID: wb.ID, Slot: 2}}}); err != nil {
			t.Fatal(err)
		}
	}

	// the old database: migrations 1-4, a shared rating, the same sets
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range migrations[:4] {
		if _, err := db.Exec(m); err != nil {
			t.Fatal(err)
		}
	}
	db.Exec(`PRAGMA user_version = 4`)
	db.Exec(`INSERT INTO accounts (id, username, pw_hash, created_at) VALUES (1, 'Ace', 'x', 0), (2, 'Bee', 'x', 0)`)
	db.Exec(`INSERT INTO ratings (account_id, ladder, rating, deviation, volatility, sets_played, last_played)
		VALUES (1, ?, 1600, 80, 0.06, 5, 0), (2, ?, 1400, 80, 0.06, 5, 0)`, testLadder, testLadder)
	for i, h := range history {
		at := start.Add(time.Duration(h.daysLate) * 24 * time.Hour).Unix()
		var winner any
		if h.status == SetRated {
			winner = h.winner + 1
		}
		if _, err := db.Exec(`INSERT INTO sets (id, ladder, match_id, boot_id, status, winner_id, created_at, ruleset_id)
			VALUES (?, ?, ?, 'b', ?, ?, ?, ?)`, i+1, testLadder, i, string(h.status), winner, at, h.ruleset); err != nil {
			t.Fatal(err)
		}
		for slot := 1; slot <= 2; slot++ {
			db.Exec(`INSERT INTO set_players (set_id, account_id, slot, games_won, rating_before, deviation_before,
				rating_after, deviation_after) VALUES (?, ?, ?, 0, 1500, 350, 1500, 350)`, i+1, slot, slot)
		}
	}
	db.Close()

	got, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer got.Close()
	last := start.Add(41 * 24 * time.Hour)
	got.SetClock(func() time.Time { return last })
	want.SetClock(func() time.Time { return last })

	close := func(x, y float64) bool { return math.Abs(x-y) < 1e-6 }
	for _, ruleset := range []string{testVanilla, testRemix} {
		for i, id := range []int64{1, 2} {
			g, _ := got.Rating(id, testLadder, ruleset)
			w, _ := want.Rating([]int64{wa.ID, wb.ID}[i], testLadder, ruleset)
			if g.SetsPlayed != w.SetsPlayed || !close(g.Rating.Rating, w.Rating.Rating) ||
				!close(g.Rating.Deviation, w.Rating.Deviation) || !close(g.Rating.Volatility, w.Rating.Volatility) {
				t.Errorf("%s player %d: got %+v, want %+v", ruleset, id, g, w)
			}
		}
	}
	// the shared pre-migration rating is gone
	if old, _ := got.Rating(1, testLadder, ""); old.SetsPlayed != 0 {
		t.Errorf("roster-less rating survived: %+v", old)
	}
	// set history before/after values match too
	gh, _ := got.RatingHistory(1)
	wh, _ := want.RatingHistory(wa.ID)
	if len(gh) != len(wh) {
		t.Fatalf("history lengths %d vs %d", len(gh), len(wh))
	}
	for i := range gh {
		if gh[i].Ruleset != wh[i].Ruleset || !close(gh[i].Rating, wh[i].Rating) {
			t.Errorf("history[%d] = %+v, want %+v", i, gh[i], wh[i])
		}
	}
}
