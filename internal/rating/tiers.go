package rating

import "sort"

// ProvisionalDeviation is the deviation above which a player is still
// provisional (roughly their first 10-15 sets).
const ProvisionalDeviation = 110.0

// Tier is one named band of conservative rating.
type Tier struct {
	Name string `json:"name"`
	// MinConservative is the lowest conservative rating in this tier.
	MinConservative float64 `json:"minConservative"`
}

// Tiers is an ordered set of bands, lowest first. It is server
// configuration so bands can be retuned without a client release.
type Tiers struct {
	Bands []Tier `json:"bands"`
}

// DefaultTiers is used when the ranked config doesn't define its own bands.
func DefaultTiers() Tiers {
	return Tiers{Bands: []Tier{
		{Name: "Bronze", MinConservative: -1e9},
		{Name: "Silver", MinConservative: 1150},
		{Name: "Gold", MinConservative: 1300},
		{Name: "Platinum", MinConservative: 1450},
		{Name: "Diamond", MinConservative: 1600},
		{Name: "Master", MinConservative: 1750},
		{Name: "Grandmaster", MinConservative: 1900},
	}}
}

// Normalized returns t sorted by MinConservative, falling back to the
// defaults when empty. The lowest band always starts at -inf.
func (t Tiers) Normalized() Tiers {
	if len(t.Bands) == 0 {
		return DefaultTiers()
	}
	bands := append([]Tier(nil), t.Bands...)
	sort.Slice(bands, func(i, j int) bool { return bands[i].MinConservative < bands[j].MinConservative })
	bands[0].MinConservative = -1e9
	return Tiers{Bands: bands}
}

// Placement is a player's displayed standing.
type Placement struct {
	Tier        string
	TierIndex   int // 0 = lowest band
	Provisional bool
}

// Policy is how a ladder turns a rating into placement and a tier.
type Policy struct {
	// PlacementDeviation: players stay in placement while their deviation
	// is above it.
	PlacementDeviation float64
	// TierDeviations: tiers come from rating - TierDeviations * deviation.
	TierDeviations float64
}

// SoloPolicy is 1v1's: placement until ±110, tiers from rating - 2±.
var SoloPolicy = Policy{PlacementDeviation: ProvisionalDeviation, TierDeviations: 2}

// TeamPolicy is 2v2's. A team result says less about each player, so
// OpenSkill's ± shrinks more slowly than Glicko-2's: placement ends at ±175
// (about 15-20 sets, against 10-15 for 1v1's ±110) and tiers subtract one ±,
// since a settled 2v2 ± is about twice a 1v1 one.
var TeamPolicy = Policy{PlacementDeviation: 175, TierDeviations: 1}

// Place returns the tier for r under the 1v1 policy. Tiers must be
// normalized.
func (t Tiers) Place(r Rating) Placement {
	return t.PlaceWith(r, SoloPolicy)
}

// PlaceWith returns the tier for r under a ladder's policy.
func (t Tiers) PlaceWith(r Rating, p Policy) Placement {
	conservative := r.Rating - p.TierDeviations*r.Deviation
	index := 0
	for i, band := range t.Bands {
		if conservative >= band.MinConservative {
			index = i
		}
	}
	return Placement{
		Tier:        t.Bands[index].Name,
		TierIndex:   index,
		Provisional: r.Deviation > p.PlacementDeviation,
	}
}
