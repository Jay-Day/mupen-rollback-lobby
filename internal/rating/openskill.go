package rating

import "math"

// Team games use OpenSkill: the Weng-Lin Bayesian approximation with the
// Plackett-Luce model (Weng & Lin, JMLR 12, 2011), the default model of the
// reference openskill libraries, with their default parameters. Every game
// of a set is rated (see accounts.rateSet). A halved beta would settle
// ratings faster, but in simulated 2v2 sets it ranked players worse and made
// small rating gaps look decisive, so the defaults stay. A team's
// strength is the sum of its players' skills; after a game every player
// moves by their own share of the surprise, more when their own skill is
// less certain.
//
// Ratings live on the Glicko-2 display scale: OpenSkill's default skill
// (mu 25, sigma 25/3) is 1500 ±350, one skill point is 42 rating points.
// So tiers, placement, the deviation floor and inactivity widening are the
// same for team ladders as for 1v1.
const (
	skillMu    = 25.0
	skillSigma = 25.0 / 3
	skillBeta  = skillSigma / 2
	skillTau   = skillMu / 300 // dynamics: keeps sigma from collapsing
	skillKappa = 0.0001
	// SkillScale is rating points per skill point (350 / (25/3) = 42).
	SkillScale = DefaultDeviation / skillSigma
)

// Skill converts a display rating to OpenSkill's mu and sigma.
func Skill(r Rating) (mu, sigma float64) {
	return skillMu + (r.Rating-DefaultRating)/SkillScale, r.Deviation / SkillScale
}

// FromSkill converts OpenSkill's mu and sigma to a display rating. Volatility
// stays at the default so inactivity widens team ratings like 1v1 ones.
func FromSkill(mu, sigma float64) Rating {
	return Rating{
		Rating:     DefaultRating + (mu-skillMu)*SkillScale,
		Deviation:  clamp(sigma*SkillScale, MinDeviation, MaxDeviation),
		Volatility: DefaultVolatility,
	}
}

// UpdateTeams rates one game between teams. ranks[i] is team i's place,
// lower is better (0 = won); equal ranks are a draw. It returns every
// player's new rating in the same shape as teams.
func UpdateTeams(teams [][]Rating, ranks []int) [][]Rating {
	type player struct{ mu, sigma float64 }
	type team struct {
		players         []player
		mu, sigmaSquare float64
		rank            int
	}
	ts := make([]team, len(teams))
	for i, members := range teams {
		ts[i].rank = ranks[i]
		for _, r := range members {
			mu, sigma := Skill(r)
			sigma = math.Sqrt(sigma*sigma + skillTau*skillTau)
			ts[i].players = append(ts[i].players, player{mu, sigma})
			ts[i].mu += mu
			ts[i].sigmaSquare += sigma * sigma
		}
	}

	c := 0.0
	for _, t := range ts {
		c += t.sigmaSquare + skillBeta*skillBeta
	}
	c = math.Sqrt(c)

	// sumQ[q]: teams placed at q's rank or worse; a[q]: teams sharing q's rank
	sumQ := make([]float64, len(ts))
	a := make([]float64, len(ts))
	for q := range ts {
		for i := range ts {
			if ts[i].rank >= ts[q].rank {
				sumQ[q] += math.Exp(ts[i].mu / c)
			}
			if ts[i].rank == ts[q].rank {
				a[q]++
			}
		}
	}

	out := make([][]Rating, len(ts))
	for i, ti := range ts {
		omega, delta := 0.0, 0.0
		iMuOverC := math.Exp(ti.mu / c)
		for q, tq := range ts {
			p := iMuOverC / sumQ[q]
			if tq.rank <= ti.rank {
				delta += p * (1 - p) / a[q]
				if q == i {
					omega += (1 - p) / a[q]
				} else {
					omega -= p / a[q]
				}
			}
		}
		omega *= ti.sigmaSquare / c
		delta *= ti.sigmaSquare / (c * c)
		delta *= math.Sqrt(ti.sigmaSquare) / c // gamma

		for _, pl := range ti.players {
			share := pl.sigma * pl.sigma / ti.sigmaSquare
			mu := pl.mu + share*omega
			sigma := pl.sigma * math.Sqrt(math.Max(1-share*delta, skillKappa))
			out[i] = append(out[i], FromSkill(mu, sigma))
		}
	}
	return out
}

// ExpectedTeamWin is the chance that team a beats team b, as the reference
// libraries predict it for two teams: Phi((mu_a - mu_b) / sqrt(2 beta^2 +
// sigma_a^2 + sigma_b^2)), team mu and sigma^2 summed over its players.
func ExpectedTeamWin(a, b []Rating) float64 {
	sum := func(players []Rating) (mu, sigmaSquare float64) {
		for _, r := range players {
			m, s := Skill(r)
			mu += m
			sigmaSquare += s * s
		}
		return mu, sigmaSquare
	}
	muA, sA := sum(a)
	muB, sB := sum(b)
	x := (muA - muB) / math.Sqrt(2*skillBeta*skillBeta+sA+sB)
	return 0.5 * math.Erfc(-x/math.Sqrt2)
}
