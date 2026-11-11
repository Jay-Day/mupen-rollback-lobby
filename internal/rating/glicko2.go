// Package rating implements Glicko-2 ratings and the tier bands built on top
// of them.
//
// Ratings are updated once per ranked set (a whole best-of-N counts as one
// win or loss), immediately after the set, the way Lichess applies Glicko-2
// per game instead of in batched rating periods. Inactivity is modelled by
// growing the rating deviation for every elapsed rating period (one day).
package rating

import (
	"math"
	"time"
)

const (
	DefaultRating     = 1500.0
	DefaultDeviation  = 350.0
	DefaultVolatility = 0.06

	// MinDeviation keeps ratings responsive for very active players.
	MinDeviation = 45.0
	// MaxDeviation is the deviation of a brand new player; inactivity never
	// pushes a player past it.
	MaxDeviation = 350.0

	// Tau constrains volatility changes (Glickman suggests 0.3-1.2).
	Tau = 0.5

	// RatingPeriod is how much inactivity counts as one Glicko-2 period.
	RatingPeriod = 24 * time.Hour

	glickoScale = 173.7178
	convergence = 0.000001
)

// Rating is one player's Glicko-2 state on the public (1500-based) scale.
type Rating struct {
	Rating     float64
	Deviation  float64
	Volatility float64
}

// Default returns the rating of a player with no sets.
func Default() Rating {
	return Rating{Rating: DefaultRating, Deviation: DefaultDeviation, Volatility: DefaultVolatility}
}

// AfterInactivity returns r with its deviation grown for the rating periods
// elapsed between lastPlayed and now. A zero lastPlayed (never played) or a
// non-positive interval returns r unchanged.
func (r Rating) AfterInactivity(lastPlayed, now time.Time) Rating {
	if lastPlayed.IsZero() || !now.After(lastPlayed) {
		return r
	}
	periods := now.Sub(lastPlayed).Hours() / RatingPeriod.Hours()
	phi := r.Deviation / glickoScale
	phi = math.Sqrt(phi*phi + r.Volatility*r.Volatility*periods)
	r.Deviation = math.Min(phi*glickoScale, MaxDeviation)
	return r
}

// Conservative is the rating the player is very likely above (rating minus
// two deviations). Tiers use it so a lucky streak can't promote a new player.
func (r Rating) Conservative() float64 {
	return r.Rating - 2*r.Deviation
}

// Update returns player's new rating after one result against opponent.
// score is 1 for a win, 0 for a loss (0.5 for a draw).
func Update(player, opponent Rating, score float64) Rating {
	mu := (player.Rating - DefaultRating) / glickoScale
	phi := player.Deviation / glickoScale
	sigma := player.Volatility

	muJ := (opponent.Rating - DefaultRating) / glickoScale
	phiJ := opponent.Deviation / glickoScale

	gJ := g(phiJ)
	e := expected(mu, muJ, gJ)
	v := 1 / (gJ * gJ * e * (1 - e))
	delta := v * gJ * (score - e)

	sigmaNew := newVolatility(phi, sigma, v, delta)

	phiStar := math.Sqrt(phi*phi + sigmaNew*sigmaNew)
	phiNew := 1 / math.Sqrt(1/(phiStar*phiStar)+1/v)
	muNew := mu + phiNew*phiNew*gJ*(score-e)

	return Rating{
		Rating:     muNew*glickoScale + DefaultRating,
		Deviation:  clamp(phiNew*glickoScale, MinDeviation, MaxDeviation),
		Volatility: sigmaNew,
	}
}

// ExpectedScore is the probability that player beats opponent.
func ExpectedScore(player, opponent Rating) float64 {
	mu := (player.Rating - DefaultRating) / glickoScale
	muJ := (opponent.Rating - DefaultRating) / glickoScale
	return expected(mu, muJ, g(opponent.Deviation/glickoScale))
}

func g(phi float64) float64 {
	return 1 / math.Sqrt(1+3*phi*phi/(math.Pi*math.Pi))
}

func expected(mu, muJ, gJ float64) float64 {
	return 1 / (1 + math.Exp(-gJ*(mu-muJ)))
}

// newVolatility is step 5 of Glickman's Glicko-2 paper (Illinois algorithm).
func newVolatility(phi, sigma, v, delta float64) float64 {
	a := math.Log(sigma * sigma)
	f := func(x float64) float64 {
		ex := math.Exp(x)
		num := ex * (delta*delta - phi*phi - v - ex)
		den := 2 * (phi*phi + v + ex) * (phi*phi + v + ex)
		return num/den - (x-a)/(Tau*Tau)
	}

	A := a
	var B float64
	if delta*delta > phi*phi+v {
		B = math.Log(delta*delta - phi*phi - v)
	} else {
		k := 1.0
		for f(a-k*Tau) < 0 {
			k++
		}
		B = a - k*Tau
	}

	fA, fB := f(A), f(B)
	for i := 0; i < 100 && math.Abs(B-A) > convergence; i++ {
		C := A + (A-B)*fA/(fB-fA)
		fC := f(C)
		if fC*fB <= 0 {
			A, fA = B, fB
		} else {
			fA /= 2
		}
		B, fB = C, fC
	}
	return math.Exp(A / 2)
}

func clamp(v, lo, hi float64) float64 {
	return math.Max(lo, math.Min(hi, v))
}
