package rating

import (
	"math"
	"testing"
	"time"
)

func TestUpdateWinAndLossAreSymmetric(t *testing.T) {
	a := Rating{Rating: 1500, Deviation: 200, Volatility: 0.06}
	b := Rating{Rating: 1500, Deviation: 200, Volatility: 0.06}

	winner := Update(a, b, 1)
	loser := Update(b, a, 0)

	if winner.Rating <= 1500 || loser.Rating >= 1500 {
		t.Fatalf("winner %.1f, loser %.1f", winner.Rating, loser.Rating)
	}
	if math.Abs((winner.Rating-1500)-(1500-loser.Rating)) > 1e-6 {
		t.Fatalf("asymmetric change: +%.4f / -%.4f", winner.Rating-1500, 1500-loser.Rating)
	}
	if winner.Deviation >= 200 || loser.Deviation >= 200 {
		t.Fatalf("deviation did not shrink: %.1f %.1f", winner.Deviation, loser.Deviation)
	}
}

func TestUpdateMatchesGlickmanSingleGame(t *testing.T) {
	// One game of Glickman's example: 1500/200 beats 1400/30.
	got := Update(Rating{1500, 200, 0.06}, Rating{1400, 30, 0.06}, 1)
	// Reference values from an independent implementation of the paper's steps.
	if math.Abs(got.Rating-1563.6) > 0.5 {
		t.Fatalf("rating = %.2f, want ~1563.6", got.Rating)
	}
	if math.Abs(got.Deviation-175.4) > 0.5 {
		t.Fatalf("deviation = %.2f, want ~175.4", got.Deviation)
	}
	if math.Abs(got.Volatility-0.06) > 0.0005 {
		t.Fatalf("volatility = %.5f, want ~0.06", got.Volatility)
	}
}

func TestUpsetMovesMoreThanExpectedWin(t *testing.T) {
	strong := Rating{1800, 80, 0.06}
	weak := Rating{1400, 80, 0.06}
	expected := Update(strong, weak, 1).Rating - strong.Rating
	upset := Update(weak, strong, 1).Rating - weak.Rating
	if upset <= expected {
		t.Fatalf("upset gain %.1f <= expected-win gain %.1f", upset, expected)
	}
}

func TestDeviationClamped(t *testing.T) {
	r := Rating{1500, 46, 0.03}
	for i := 0; i < 200; i++ {
		r = Update(r, Rating{1500, 46, 0.03}, float64(i%2))
	}
	if r.Deviation < MinDeviation {
		t.Fatalf("deviation %.2f below minimum", r.Deviation)
	}
}

func TestInactivityGrowsDeviation(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	r := Rating{1600, 50, 0.06}
	if got := r.AfterInactivity(time.Time{}, start); got != r {
		t.Fatal("never-played rating changed")
	}
	month := r.AfterInactivity(start, start.Add(30*24*time.Hour))
	year := r.AfterInactivity(start, start.Add(365*24*time.Hour))
	if !(month.Deviation > r.Deviation && year.Deviation > month.Deviation) {
		t.Fatalf("deviation 0d %.1f, 30d %.1f, 365d %.1f", r.Deviation, month.Deviation, year.Deviation)
	}
	if forever := r.AfterInactivity(start, start.Add(100*365*24*time.Hour)); forever.Deviation != MaxDeviation {
		t.Fatalf("deviation %.1f not capped", forever.Deviation)
	}
	if month.Rating != r.Rating {
		t.Fatal("inactivity changed the rating itself")
	}
}

func TestTierPlacement(t *testing.T) {
	tiers := DefaultTiers().Normalized()

	fresh := tiers.Place(Default())
	if fresh.TierIndex != 0 || !fresh.Provisional {
		t.Fatalf("new player placement = %+v", fresh)
	}

	settled := tiers.Place(Rating{1800, 60, 0.06}) // conservative 1680
	if settled.Tier != "Diamond" || settled.Provisional {
		t.Fatalf("settled placement = %+v", settled)
	}

	custom := Tiers{Bands: []Tier{{"High", 1000}, {"Low", 0}}}.Normalized()
	if custom.Bands[0].Name != "Low" || custom.Place(Rating{1300, 100, 0.06}).Tier != "High" {
		t.Fatalf("custom tiers not sorted: %+v", custom.Bands)
	}
}
