package accounts

import (
	"database/sql"
	"time"

	"github.com/jay-day/rmgk-lobby/internal/rating"
)

// rebuildRatingsByRoster recomputes every rating from the set history, one
// rating per (ladder, roster), when ratings stop being shared between a
// ladder's rosters. Sets are replayed oldest first, exactly as RecordSet
// would have rated them, and each set's before/after values are rewritten so
// the rating history matches.
func rebuildRatingsByRoster(tx *sql.Tx) error {
	type setRow struct {
		id                  int64
		ladder, ruleset     string
		status              SetStatus
		winner, createdUnix int64
	}
	rows, err := tx.Query(`SELECT id, ladder, ruleset_id, status, COALESCE(winner_id, 0), created_at
		FROM sets ORDER BY created_at, id`)
	if err != nil {
		return err
	}
	var sets []setRow
	for rows.Next() {
		var r setRow
		var status string
		if err := rows.Scan(&r.id, &r.ladder, &r.ruleset, &status, &r.winner, &r.createdUnix); err != nil {
			rows.Close()
			return err
		}
		r.status = SetStatus(status)
		sets = append(sets, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	type key struct {
		account         int64
		ladder, ruleset string
	}
	type state struct {
		r          rating.Rating
		setsPlayed int
		lastPlayed int64
	}
	ratings := map[key]*state{}
	current := func(k key, at time.Time) rating.Rating {
		st := ratings[k]
		if st == nil {
			return rating.Default()
		}
		return st.r.AfterInactivity(time.Unix(st.lastPlayed, 0), at)
	}

	for _, set := range sets {
		players, err := setPlayerIDs(tx, set.id)
		if err != nil {
			return err
		}
		at := time.Unix(set.createdUnix, 0)
		before := make([]rating.Rating, len(players))
		after := make([]rating.Rating, len(players))
		for i, id := range players {
			before[i] = current(key{id, set.ladder, set.ruleset}, at)
			after[i] = before[i]
		}
		if set.status == SetRated && len(players) == 2 {
			for i, id := range players {
				score := 0.0
				if id == set.winner {
					score = 1
				}
				after[i] = rating.Update(before[i], before[1-i], score)
			}
			for i, id := range players {
				k := key{id, set.ladder, set.ruleset}
				st := ratings[k]
				if st == nil {
					st = &state{}
					ratings[k] = st
				}
				st.r, st.lastPlayed = after[i], set.createdUnix
				st.setsPlayed++
			}
		}
		for i, id := range players {
			if _, err := tx.Exec(`UPDATE set_players SET rating_before = ?, deviation_before = ?,
				rating_after = ?, deviation_after = ? WHERE set_id = ? AND account_id = ?`,
				before[i].Rating, before[i].Deviation, after[i].Rating, after[i].Deviation, set.id, id); err != nil {
				return err
			}
		}
	}

	for k, st := range ratings {
		if _, err := tx.Exec(`INSERT INTO ratings (account_id, ladder, ruleset, rating, deviation, volatility,
			sets_played, last_played) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			k.account, k.ladder, k.ruleset, st.r.Rating, st.r.Deviation, st.r.Volatility,
			st.setsPlayed, st.lastPlayed); err != nil {
			return err
		}
	}
	return nil
}

// setPlayerIDs lists a set's players in slot order.
func setPlayerIDs(tx *sql.Tx, setID int64) ([]int64, error) {
	rows, err := tx.Query(`SELECT account_id FROM set_players WHERE set_id = ? ORDER BY slot`, setID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
