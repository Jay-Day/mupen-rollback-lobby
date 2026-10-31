package accounts

import (
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/jay-day/rmgk-lobby/internal/rating"
)

// Read-only queries behind the public rankings page.

// ErrNotFound is returned when a looked-up account doesn't exist.
var ErrNotFound = errors.New("not found")

// LeaderboardEntry is one ranked player on a ladder.
type LeaderboardEntry struct {
	AccountID  int64
	Username   string
	Rating     rating.Rating // inactivity applied
	SetsPlayed int
	Wins       int
	Losses     int
	LastPlayed time.Time
}

// wonSet is true when the set_players row sp won the rated set st: the 1v1
// winner, or a player of the winning team.
const wonSet = `(COALESCE(st.winner_id, 0) = sp.account_id OR (st.winner_team > 0 AND sp.team = st.winner_team))`

// Leaderboard returns every enabled account that has played a rated set on
// one roster of a ladder, unsorted (callers rank by whatever the page shows).
func (s *Store) Leaderboard(ladder, ruleset string) ([]LeaderboardEntry, error) {
	rows, err := s.db.Query(`
		WITH record AS (
			SELECT sp.account_id,
				SUM(`+wonSet+`) AS wins,
				SUM(NOT (`+wonSet+`)) AS losses
			FROM sets st JOIN set_players sp ON sp.set_id = st.id
			WHERE st.ladder = ? AND st.ruleset_id = ? AND st.status = 'rated'
			GROUP BY sp.account_id)
		SELECT a.id, a.username, r.rating, r.deviation, r.volatility, r.sets_played, r.last_played,
			COALESCE(rec.wins, 0), COALESCE(rec.losses, 0)
		FROM ratings r JOIN accounts a ON a.id = r.account_id
			LEFT JOIN record rec ON rec.account_id = a.id
		WHERE r.ladder = ? AND r.ruleset = ? AND r.sets_played > 0 AND a.disabled = 0`,
		ladder, ruleset, ladder, ruleset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	now := s.now()
	var out []LeaderboardEntry
	for rows.Next() {
		var (
			e    LeaderboardEntry
			last int64
		)
		if err := rows.Scan(&e.AccountID, &e.Username, &e.Rating.Rating, &e.Rating.Deviation,
			&e.Rating.Volatility, &e.SetsPlayed, &last, &e.Wins, &e.Losses); err != nil {
			return nil, err
		}
		if last > 0 {
			e.LastPlayed = time.Unix(last, 0)
			e.Rating = e.Rating.AfterInactivity(e.LastPlayed, now)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// homeRegionSets is how many recent sets decide a player's home region.
const homeRegionSets = 20

// HomeRegions returns each player's home region: the lobby region of most of
// their last homeRegionSets ranked sets (the most recent wins a tie), so a
// trip or a VPN doesn't move them. Players without a recorded region are
// left out.
// The result is cached until the next recorded set; callers must not modify it.
func (s *Store) HomeRegions() (map[int64]string, error) {
	s.homesMu.Lock()
	defer s.homesMu.Unlock()
	if s.homes != nil {
		return s.homes, nil
	}
	homes, err := s.queryHomeRegions()
	if err == nil {
		s.homes = homes
	}
	return homes, err
}

func (s *Store) queryHomeRegions() (map[int64]string, error) {
	rows, err := s.db.Query(`
		SELECT account_id, region FROM (
			SELECT sp.account_id, sp.region,
				ROW_NUMBER() OVER (PARTITION BY sp.account_id ORDER BY st.created_at DESC, st.id DESC) AS n
			FROM set_players sp JOIN sets st ON st.id = sp.set_id
			WHERE sp.region <> '')
		WHERE n <= ? ORDER BY account_id, n`, homeRegionSets)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type tally struct {
		counts map[string]int
		order  []string // regions by first (most recent) appearance
	}
	tallies := map[int64]*tally{}
	for rows.Next() {
		var (
			id     int64
			region string
		)
		if err := rows.Scan(&id, &region); err != nil {
			return nil, err
		}
		t := tallies[id]
		if t == nil {
			t = &tally{counts: map[string]int{}}
			tallies[id] = t
		}
		if t.counts[region] == 0 {
			t.order = append(t.order, region)
		}
		t.counts[region]++
	}
	out := make(map[int64]string, len(tallies))
	for id, t := range tallies {
		best := ""
		for _, region := range t.order {
			if best == "" || t.counts[region] > t.counts[best] {
				best = region
			}
		}
		out[id] = best
	}
	return out, rows.Err()
}

// AccountByName looks an account up by username (case-insensitive).
func (s *Store) AccountByName(name string) (Account, error) {
	var (
		account  Account
		created  int64
		disabled bool
	)
	err := s.db.QueryRow(`SELECT id, username, created_at, disabled FROM accounts WHERE username = ?`,
		strings.TrimSpace(name)).Scan(&account.ID, &account.Username, &created, &disabled)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && disabled) {
		return Account{}, ErrNotFound
	}
	if err != nil {
		return Account{}, err
	}
	account.CreatedAt = time.Unix(created, 0)
	return account, nil
}

// Ratings returns an account's rating on every ladder roster it has one for.
func (s *Store) Ratings(accountID int64) ([]LadderRating, error) {
	rows, err := s.db.Query(`SELECT ladder, ruleset FROM ratings WHERE account_id = ? ORDER BY ladder, ruleset`, accountID)
	if err != nil {
		return nil, err
	}
	type board struct{ ladder, ruleset string }
	var boards []board
	for rows.Next() {
		var b board
		if err := rows.Scan(&b.ladder, &b.ruleset); err != nil {
			rows.Close()
			return nil, err
		}
		boards = append(boards, b)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := make([]LadderRating, 0, len(boards))
	for _, b := range boards {
		lr, err := s.loadRating(s.db, accountID, b.ladder, b.ruleset)
		if err != nil {
			return nil, err
		}
		out = append(out, lr)
	}
	return out, nil
}

// SetSummary is one recorded set with its players.
type SetSummary struct {
	ID         int64
	Ladder     string
	Status     SetStatus
	Reason     string
	WinnerID   int64 // 1v1
	WinnerTeam int   // team sets: the winning team (1 or 2)
	CreatedAt  time.Time
	ResultJSON string
	Players    []SetSummaryPlayer
	// What it was played with, and whether a replay is stored.
	RulesetID     string
	ModuleVersion string
	RomMD5        string
	HasReplay     bool
}

// SetSummaryPlayer is a participant of a recorded set.
type SetSummaryPlayer struct {
	AccountID int64
	Username  string
	Slot      int
	GamesWon  int
	Team      int           // 1 or 2 in a team set
	Before    rating.Rating // only Rating/Deviation are stored
	After     rating.Rating
}

// RecentSets returns the newest sets, newest first. ladder "" means every
// ladder; accountID 0 means every player.
func (s *Store) RecentSets(ladder, ruleset string, accountID int64, limit int) ([]SetSummary, error) {
	sets, _, err := s.FindSets(SetFilter{Ladder: ladder, Ruleset: ruleset, AccountID: accountID, Limit: limit})
	return sets, err
}

// querySets loads sets matching where (newest first) with their players.
func (s *Store) querySets(where string, args []any, limit, offset int) ([]SetSummary, error) {
	query := `SELECT st.id, st.ladder, st.status, st.reason, COALESCE(st.winner_id, 0), st.winner_team, st.created_at,
			st.result_json, st.ruleset_id, st.module_version, st.rom_md5,
			EXISTS (SELECT 1 FROM replays rp WHERE rp.set_id = st.id)
		FROM sets st WHERE ` + where + ` ORDER BY st.created_at DESC, st.id DESC LIMIT ? OFFSET ?`
	args = append(append([]any{}, args...), limit, offset)

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	var (
		out   []SetSummary
		index = map[int64]int{}
	)
	for rows.Next() {
		var (
			set     SetSummary
			status  string
			created int64
		)
		if err := rows.Scan(&set.ID, &set.Ladder, &status, &set.Reason, &set.WinnerID, &set.WinnerTeam, &created, &set.ResultJSON,
			&set.RulesetID, &set.ModuleVersion, &set.RomMD5, &set.HasReplay); err != nil {
			rows.Close()
			return nil, err
		}
		set.Status = SetStatus(status)
		set.CreatedAt = time.Unix(created, 0)
		index[set.ID] = len(out)
		out = append(out, set)
	}
	rows.Close()
	if err := rows.Err(); err != nil || len(out) == 0 {
		return out, err
	}

	ids := make([]string, 0, len(out))
	idArgs := make([]any, 0, len(out))
	for _, set := range out {
		ids = append(ids, "?")
		idArgs = append(idArgs, set.ID)
	}
	prows, err := s.db.Query(`SELECT sp.set_id, sp.account_id, a.username, sp.slot, sp.games_won, sp.team,
			sp.rating_before, sp.deviation_before, sp.rating_after, sp.deviation_after
		FROM set_players sp JOIN accounts a ON a.id = sp.account_id
		WHERE sp.set_id IN (`+strings.Join(ids, ",")+`) ORDER BY sp.slot`, idArgs...)
	if err != nil {
		return nil, err
	}
	defer prows.Close()
	for prows.Next() {
		var (
			setID int64
			p     SetSummaryPlayer
		)
		if err := prows.Scan(&setID, &p.AccountID, &p.Username, &p.Slot, &p.GamesWon, &p.Team,
			&p.Before.Rating, &p.Before.Deviation, &p.After.Rating, &p.After.Deviation); err != nil {
			return nil, err
		}
		if i, ok := index[setID]; ok {
			out[i].Players = append(out[i].Players, p)
		}
	}
	return out, prows.Err()
}

// HistoryPoint is a player's rating after one rated set.
type HistoryPoint struct {
	Ladder, Ruleset string
	Time            time.Time
	Rating          float64
}

// RatingHistory returns an account's rating after each of its rated sets,
// oldest first, on every ladder roster.
func (s *Store) RatingHistory(accountID int64) ([]HistoryPoint, error) {
	rows, err := s.db.Query(`SELECT st.ladder, st.ruleset_id, st.created_at, sp.rating_after
		FROM sets st JOIN set_players sp ON sp.set_id = st.id
		WHERE sp.account_id = ? AND st.status = 'rated'
		ORDER BY st.created_at, st.id`, accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []HistoryPoint
	for rows.Next() {
		var (
			p    HistoryPoint
			unix int64
		)
		if err := rows.Scan(&p.Ladder, &p.Ruleset, &unix, &p.Rating); err != nil {
			return nil, err
		}
		p.Time = time.Unix(unix, 0)
		out = append(out, p)
	}
	return out, rows.Err()
}
