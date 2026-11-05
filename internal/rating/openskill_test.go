package rating

import (
	"math"
	"testing"
)

// skill builds a display rating from OpenSkill mu and sigma.
func skill(mu, sigma float64) Rating {
	return Rating{Rating: DefaultRating + (mu-skillMu)*SkillScale, Deviation: sigma * SkillScale, Volatility: DefaultVolatility}
}

// Expected values come from the reference implementation (Python openskill
// 6.2.0, PlackettLuce() with default parameters).
func TestUpdateTeamsMatchesReference(t *testing.T) {
	type mus [][][2]float64
	cases := []struct {
		name  string
		teams mus
		ranks []int
		want  mus
	}{
		{"default 2v2", mus{{{25, 25.0 / 3}, {25, 25.0 / 3}}, {{25, 25.0 / 3}, {25, 25.0 / 3}}}, []int{0, 1},
			mus{{{26.964294622, 8.177962604}, {26.964294622, 8.177962604}}, {{23.035705378, 8.177962604}, {23.035705378, 8.177962604}}}},
		{"settled vs new partner", mus{{{30, 2}, {25, 25.0 / 3}}, {{27, 3}, {26, 4}}}, []int{0, 1},
			mus{{{30.158592043, 1.996179285}, {27.748837126, 7.92326633}}, {{26.643511474, 2.990225859}, {25.366456397, 3.974932887}}}},
		{"upset", mus{{{20, 3}, {21, 3}}, {{32, 2.5}, {30, 2.5}}}, []int{0, 1},
			mus{{{21.037921159, 2.994142941}, {22.037921159, 2.994142941}}, {{31.278976897, 2.498005235}, {29.278976897, 2.498005235}}}},
		{"expected win", mus{{{32, 2.5}, {30, 2.5}}, {{20, 3}, {21, 3}}}, []int{0, 1},
			mus{{{32.053567173, 2.498005235}, {30.053567173, 2.498005235}}, {{19.92288943, 2.994142941}, {20.92288943, 2.994142941}}}},
		{"draw", mus{{{28, 4}, {22, 5}}, {{25, 3}, {24, 3}}}, []int{0, 0},
			mus{{{27.957352717, 3.944137852}, {21.933374027, 4.889467758}}, {{25.023997191, 2.985358896}, {24.023997191, 2.985358896}}}},
	}
	for _, tc := range cases {
		teams := make([][]Rating, len(tc.teams))
		for i, team := range tc.teams {
			for _, p := range team {
				teams[i] = append(teams[i], skill(p[0], p[1]))
			}
		}
		got := UpdateTeams(teams, tc.ranks)
		for i, team := range tc.want {
			for j, want := range team {
				mu, sigma := Skill(got[i][j])
				if math.Abs(mu-want[0]) > 1e-6 || math.Abs(sigma-want[1]) > 1e-6 {
					t.Errorf("%s: team %d player %d = (%.9f, %.9f), want (%.9f, %.9f)", tc.name, i, j, mu, sigma, want[0], want[1])
				}
			}
		}
	}
}

func TestTeamScaleMatchesGlicko(t *testing.T) {
	mu, sigma := Skill(Default())
	if mu != 25 || math.Abs(sigma-25.0/3) > 1e-12 {
		t.Fatalf("default rating = mu %v sigma %v", mu, sigma)
	}
	if r := FromSkill(25, 25.0/3); r.Rating != 1500 || r.Deviation != 350 || r.Volatility != DefaultVolatility {
		t.Fatalf("default skill = %+v", r)
	}
	// the floor applies to team ratings too
	if r := FromSkill(25, 0.1); r.Deviation != MinDeviation {
		t.Fatalf("deviation floor = %v", r.Deviation)
	}
}

func TestExpectedTeamWinMatchesReference(t *testing.T) {
	a := []Rating{skill(30, 2), skill(25, 25.0/3)}
	b := []Rating{skill(27, 3), skill(26, 4)}
	if got := ExpectedTeamWin(a, b); math.Abs(got-0.5687974682602307) > 1e-9 {
		t.Fatalf("win chance = %v, want 0.5687974682602307", got)
	}
	if got := ExpectedTeamWin(a, a); math.Abs(got-0.5) > 1e-12 {
		t.Fatalf("mirror = %v", got)
	}
}

func TestTeamPolicy(t *testing.T) {
	tiers := DefaultTiers().Normalized()
	r := Rating{Rating: 1700, Deviation: 150}
	// 1v1: 1700 - 300 = 1400 (Gold), still in placement at ±150
	if p := tiers.Place(r); p.Tier != "Gold" || !p.Provisional {
		t.Fatalf("1v1 placement = %+v", p)
	}
	// 2v2: 1700 - 150 = 1550 (Platinum), placed below ±175
	if p := tiers.PlaceWith(r, TeamPolicy); p.Tier != "Platinum" || p.Provisional {
		t.Fatalf("2v2 placement = %+v", p)
	}
}
