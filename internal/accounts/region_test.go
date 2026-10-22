package accounts

import (
	"testing"
	"time"
)

func TestHomeRegions(t *testing.T) {
	s := openTestStore(t)
	a, _ := s.Register("Ace", "password123")
	b, _ := s.Register("Bee", "password123")
	c, _ := s.Register("Cid", "password123")
	now := time.Unix(1_700_000_000, 0)
	s.SetClock(func() time.Time { return now })
	match := uint64(0)
	play := func(regionA, regionB string) {
		t.Helper()
		match++
		now = now.Add(time.Hour)
		if _, err := s.RecordSet(SetRecord{Ladder: testLadder, MatchID: match, BootID: "b", Status: SetRated,
			RulesetID: testVanilla, WinnerID: a.ID, Players: []SetPlayer{
				{AccountID: a.ID, Slot: 1, GamesWon: 2, Region: regionA}, {AccountID: b.ID, Slot: 2, Region: regionB}}}); err != nil {
			t.Fatal(err)
		}
	}
	// Ace: an old stretch in Asia, then mostly NA East with a recent trip to Europe
	for i := 0; i < 5; i++ {
		play("asia-east", "us-west")
	}
	for i := 0; i < 12; i++ {
		play("us-east", "us-west")
	}
	for i := 0; i < 8; i++ {
		play("eu-west", "eu-west")
	}
	homes, err := s.HomeRegions()
	if err != nil {
		t.Fatal(err)
	}
	if homes[a.ID] != "us-east" {
		t.Fatalf("Ace's home = %q, want us-east (12 of the last 20)", homes[a.ID])
	}
	// Bee: 12 us-west then 8 eu-west within the last 20 (older sets don't count)
	if homes[b.ID] != "us-west" {
		t.Fatalf("Bee's home = %q", homes[b.ID])
	}
	if _, ok := homes[c.ID]; ok {
		t.Fatal("a player without sets has a home region")
	}

	// a tie goes to the most recent region
	d, _ := s.Register("Dee", "password123")
	for i, region := range []string{"eu-west", "eu-west", "us-west", "us-west"} {
		now = now.Add(time.Hour)
		if _, err := s.RecordSet(SetRecord{Ladder: testLadder, MatchID: uint64(100 + i), BootID: "b", Status: SetRated,
			RulesetID: testVanilla, WinnerID: d.ID, Players: []SetPlayer{
				{AccountID: d.ID, Slot: 1, GamesWon: 2, Region: region}, {AccountID: c.ID, Slot: 2}}}); err != nil {
			t.Fatal(err)
		}
	}
	homes, _ = s.HomeRegions()
	if homes[d.ID] != "us-west" {
		t.Fatalf("tie went to %q", homes[d.ID])
	}
	if _, ok := homes[c.ID]; ok {
		t.Fatal("sets without a recorded region gave a home region")
	}
}
