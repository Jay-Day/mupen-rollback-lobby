package accounts

import (
	"fmt"
	"path/filepath"
	"testing"
)

// result builds stored reports where slot 1 plays c1 and slot 2 plays c2.
func result(c1, c2 int) string {
	return fmt.Sprintf(`{"x":{"games":[{"players":[{"slot":1,"character":%d},{"slot":2,"character":%d}]}]}}`, c1, c2)
}

func seedFilterSets(t *testing.T, s *Store) (a, b, c Account) {
	t.Helper()
	a, _ = s.Register("Ace", "password123")
	b, _ = s.Register("Bee", "password123")
	c, _ = s.Register("Cid", "password123")
	sets := []struct {
		p1, p2  Account
		c1, c2  int
		ruleset string
		winner  Account
		status  SetStatus
	}{
		{a, b, 29, 0, testVanilla, a, SetRated}, // Ace (Falco) beats Bee (Mario)
		{a, c, 5, 7, testRemix, c, SetRated},    // Cid (Falcon) beats Ace (Link)
		{b, c, 0, 7, testVanilla, b, SetRated},  // Bee (Mario) beats Cid (Falcon)
		{a, b, 29, 0, testVanilla, Account{}, SetVoided},
	}
	for i, set := range sets {
		_, err := s.RecordSet(SetRecord{Ladder: testLadder, MatchID: uint64(i + 1), BootID: "b", Status: set.status,
			WinnerID: set.winner.ID, RulesetID: set.ruleset, ResultJSON: result(set.c1, set.c2),
			Players: []SetPlayer{{AccountID: set.p1.ID, Slot: 1}, {AccountID: set.p2.ID, Slot: 2}}})
		if err != nil {
			t.Fatal(err)
		}
	}
	return a, b, c
}

func TestFindSetsFilters(t *testing.T) {
	s := openTestStore(t)
	a, b, c := seedFilterSets(t, s)
	count := func(f SetFilter) int {
		t.Helper()
		sets, total, err := s.FindSets(f)
		if err != nil {
			t.Fatal(err)
		}
		if len(sets) != total && f.Limit == 0 {
			t.Fatalf("%+v: %d sets, total %d", f, len(sets), total)
		}
		return total
	}
	cases := []struct {
		name string
		f    SetFilter
		want int
	}{
		{"everything", SetFilter{}, 4},
		{"vanilla roster", SetFilter{Ruleset: testVanilla}, 3},
		{"Ace's sets", SetFilter{AccountID: a.ID}, 3},
		{"Ace vs Cid", SetFilter{AccountID: a.ID, OpponentID: c.ID}, 1},
		{"Ace's wins", SetFilter{AccountID: a.ID, Result: "win"}, 1},
		{"Ace's losses (voided sets are neither)", SetFilter{AccountID: a.ID, Result: "loss"}, 1},
		{"Mario by anyone", SetFilter{Character: 0, HasCharacter: true}, 3},
		{"Mario by Ace", SetFilter{AccountID: a.ID, Character: 0, HasCharacter: true}, 0},
		{"Falco by Ace on vanilla", SetFilter{AccountID: a.ID, Character: 29, HasCharacter: true, Ruleset: testVanilla}, 2},
		{"Bee's Mario wins", SetFilter{AccountID: b.ID, Character: 0, HasCharacter: true, Result: "win"}, 1},
	}
	for _, tc := range cases {
		if got := count(tc.f); got != tc.want {
			t.Errorf("%s: %d sets, want %d", tc.name, got, tc.want)
		}
	}

	// paging: newest first, total unaffected
	page, total, _ := s.FindSets(SetFilter{Limit: 3, Offset: 3})
	if total != 4 || len(page) != 1 || page[0].Status != SetRated {
		t.Fatalf("second page = %d sets of %d", len(page), total)
	}

	uses, _ := s.PlayerCharacters(a.ID)
	if len(uses) != 2 || uses[0].Character != 29 || uses[0].Sets != 2 || uses[1].Character != 5 {
		t.Fatalf("Ace's characters = %+v", uses)
	}
	roster, _ := s.RosterCharacters()
	if got := roster[[2]string{testLadder, testVanilla}]; len(got) != 3 || got[0] != 0 || got[1] != 7 || got[2] != 29 {
		t.Fatalf("vanilla characters = %v", got)
	}
}

// Sets recorded before set_characters existed are filled in by migration 6.
func TestMigrationBackfillsSetCharacters(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	a, _, _ := seedFilterSets(t, s)
	// back to a version 5 database: no set_characters, team or region columns
	if _, err := s.db.Exec(`DROP TABLE set_characters; DROP INDEX sets_recent;
		ALTER TABLE set_players DROP COLUMN team; ALTER TABLE sets DROP COLUMN winner_team;
		ALTER TABLE set_players DROP COLUMN region; DROP TABLE discord_links;
		DROP TABLE email_confirmations; ALTER TABLE accounts DROP COLUMN email;
		PRAGMA user_version = 5`); err != nil {
		t.Fatal(err)
	}
	s.Close()

	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, total, _ := s.FindSets(SetFilter{AccountID: a.ID, Character: 29, HasCharacter: true}); total != 2 {
		t.Fatalf("Falco sets after backfill = %d", total)
	}
}
