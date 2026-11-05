package accounts

import (
	"database/sql"
	"encoding/json"
	"errors"
	"sort"
	"time"

	"github.com/jay-day/rmgk-lobby/internal/rating"
)

// Rating a set's result: Glicko-2 for 1v1 (one result per set), OpenSkill
// for team sets (every player has a team, 1 or 2), where every game of the
// set is rated in order.

// setParticipant is a player of a set as rating needs it.
type setParticipant struct {
	AccountID int64
	Team      int // 0 in 1v1
}

// rateSet loads the players' ratings on the set's ladder roster and, when
// rate is true, applies the result and saves the new ratings. It returns
// each player's before and after (equal when not rated).
//
// gameWinners lists a team set's game winners (team 1 or 2) in order; when
// it's empty the set counts as one game won by winnerTeam.
func (s *Store) rateSet(tx *sql.Tx, ladder, ruleset string, players []setParticipant,
	winnerID int64, winnerTeam int, gameWinners []int, rate bool, now time.Time) ([]PlayerOutcome, error) {
	outcomes := make([]PlayerOutcome, len(players))
	current := make([]rating.Rating, len(players))
	for i, p := range players {
		lr, err := s.loadRating(tx, p.AccountID, ladder, ruleset)
		if err != nil {
			return nil, err
		}
		current[i] = lr.Rating
		outcomes[i] = PlayerOutcome{AccountID: p.AccountID, Before: lr.Rating, After: lr.Rating}
	}
	if !rate {
		return outcomes, nil
	}

	if winnerTeam != 0 {
		// two teams; the winning team is ranked first
		var teams [2][]rating.Rating
		var index [2][]int
		for i, p := range players {
			if p.Team != 1 && p.Team != 2 {
				return nil, errors.New("every player of a team set needs team 1 or 2")
			}
			teams[p.Team-1] = append(teams[p.Team-1], current[i])
			index[p.Team-1] = append(index[p.Team-1], i)
		}
		if len(teams[0]) == 0 || len(teams[0]) != len(teams[1]) || (winnerTeam != 1 && winnerTeam != 2) {
			return nil, errors.New("a rated team set needs two teams of the same size and a winning team")
		}
		games := gameWinners
		if len(games) == 0 {
			games = []int{winnerTeam}
		}
		ratings := [][]rating.Rating{teams[0], teams[1]}
		for _, won := range games {
			if won != 1 && won != 2 {
				return nil, errors.New("a game of a team set has no winning team")
			}
			ranks := []int{1, 1}
			ranks[won-1] = 0
			ratings = rating.UpdateTeams(ratings, ranks)
		}
		for t := range teams {
			for j, i := range index[t] {
				outcomes[i].After = ratings[t][j]
			}
		}
	} else {
		if len(players) != 2 || (players[0].AccountID != winnerID && players[1].AccountID != winnerID) {
			return nil, errors.New("a rated set needs exactly two players and a winner who played it")
		}
		for i, p := range players {
			score := 0.0
			if p.AccountID == winnerID {
				score = 1
			}
			outcomes[i].After = rating.Update(current[i], current[1-i], score)
		}
	}
	for i, p := range players {
		if err := saveRatedSet(tx, p.AccountID, ladder, ruleset, outcomes[i].After, now); err != nil {
			return nil, err
		}
	}
	return outcomes, nil
}

// reportedGames takes a disputed team set's game winners from a stored
// report that names winnerTeam as the set's winner (reports are sorted by
// reporter so the choice is stable). Nil when none does, or in 1v1.
func reportedGames(resultJSON string, winnerTeam int) []int {
	if winnerTeam == 0 || resultJSON == "" {
		return nil
	}
	var reports map[string]struct {
		WinnerTeam int `json:"winnerTeam"`
		Games      []struct {
			WinnerTeam int `json:"winnerTeam"`
		} `json:"games"`
	}
	if json.Unmarshal([]byte(resultJSON), &reports) != nil {
		return nil
	}
	names := make([]string, 0, len(reports))
	for name := range reports {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		r := reports[name]
		if r.WinnerTeam != winnerTeam || len(r.Games) == 0 {
			continue
		}
		games := make([]int, 0, len(r.Games))
		valid := true
		for _, g := range r.Games {
			valid = valid && (g.WinnerTeam == 1 || g.WinnerTeam == 2)
			games = append(games, g.WinnerTeam)
		}
		if valid {
			return games
		}
	}
	return nil
}
