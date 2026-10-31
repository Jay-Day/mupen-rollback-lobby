package accounts

import (
	"database/sql"
	"encoding/json"
	"sort"
	"strings"
)

// Characters each player used in a set, so set lists can be filtered by
// character. Taken from the set's MATCH_RESULT reports when it's recorded
// (migration 6 fills it in for older sets).

// CharactersBySlot pulls each slot's characters (first-use order) out of a
// set's stored reports. The reports agree on rated sets, so the first one
// that has games is used.
func CharactersBySlot(resultJSON string) map[int][]int {
	out := map[int][]int{}
	if resultJSON == "" {
		return out
	}
	var reports map[string]struct {
		Games []struct {
			Players []struct {
				Slot      int `json:"slot"`
				Character int `json:"character"`
			} `json:"players"`
		} `json:"games"`
	}
	if json.Unmarshal([]byte(resultJSON), &reports) != nil {
		return out
	}
	keys := make([]string, 0, len(reports))
	for k := range reports {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		for _, game := range reports[k].Games {
			for _, p := range game.Players {
				if p.Character < 0 {
					continue
				}
				seen := false
				for _, c := range out[p.Slot] {
					seen = seen || c == p.Character
				}
				if !seen {
					out[p.Slot] = append(out[p.Slot], p.Character)
				}
			}
		}
		if len(out) > 0 {
			break
		}
	}
	return out
}

// insertSetCharacters records which characters each player of a set used.
func insertSetCharacters(tx execer, setID int64, slotAccounts map[int]int64, resultJSON string) error {
	for slot, characters := range CharactersBySlot(resultJSON) {
		accountID, ok := slotAccounts[slot]
		if !ok {
			continue
		}
		for _, c := range characters {
			if _, err := tx.Exec(`INSERT OR IGNORE INTO set_characters (set_id, account_id, character) VALUES (?, ?, ?)`,
				setID, accountID, c); err != nil {
				return err
			}
		}
	}
	return nil
}

// backfillSetCharacters fills set_characters for sets recorded before it
// existed (migration 6).
func backfillSetCharacters(tx *sql.Tx) error {
	rows, err := tx.Query(`SELECT id, result_json FROM sets WHERE result_json != ''`)
	if err != nil {
		return err
	}
	type setRow struct {
		id     int64
		result string
	}
	var sets []setRow
	for rows.Next() {
		var r setRow
		if err := rows.Scan(&r.id, &r.result); err != nil {
			rows.Close()
			return err
		}
		sets = append(sets, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, set := range sets {
		slots := map[int]int64{}
		prows, err := tx.Query(`SELECT slot, account_id FROM set_players WHERE set_id = ?`, set.id)
		if err != nil {
			return err
		}
		for prows.Next() {
			var slot int
			var account int64
			if err := prows.Scan(&slot, &account); err != nil {
				prows.Close()
				return err
			}
			slots[slot] = account
		}
		prows.Close()
		if err := insertSetCharacters(tx, set.id, slots, set.result); err != nil {
			return err
		}
	}
	return nil
}

// SetFilter selects sets for FindSets. Zero values don't filter.
type SetFilter struct {
	Ladder, Ruleset string
	// AccountID: sets this account played. OpponentID (with AccountID):
	// only against this account.
	AccountID, OpponentID int64
	// Character: sets where this character was played, by AccountID when set.
	Character    int
	HasCharacter bool
	// Result "win" or "loss": rated sets AccountID won or lost.
	Result        string
	Offset, Limit int
}

// FindSets returns the sets matching f, newest first, and how many match in
// total.
func (s *Store) FindSets(f SetFilter) ([]SetSummary, int, error) {
	if f.Limit <= 0 || f.Limit > 200 {
		f.Limit = 50
	}
	if f.Offset < 0 {
		f.Offset = 0
	}
	conds := []string{`(? = '' OR st.ladder = ?)`, `(? = '' OR st.ruleset_id = ?)`}
	args := []any{f.Ladder, f.Ladder, f.Ruleset, f.Ruleset}
	if f.AccountID != 0 {
		conds = append(conds, `st.id IN (SELECT set_id FROM set_players WHERE account_id = ?)`)
		args = append(args, f.AccountID)
		if f.OpponentID != 0 {
			// against: in a team set, on the other team
			conds = append(conds, `EXISTS (SELECT 1 FROM set_players me JOIN set_players them ON them.set_id = me.set_id
				WHERE me.set_id = st.id AND me.account_id = ? AND them.account_id = ? AND (me.team = 0 OR me.team != them.team))`)
			args = append(args, f.AccountID, f.OpponentID)
		}
		switch f.Result {
		case "win", "loss":
			won := `EXISTS (SELECT 1 FROM set_players sp WHERE sp.set_id = st.id AND sp.account_id = ? AND ` + wonSet + `)`
			if f.Result == "loss" {
				won = "NOT " + won
			}
			conds = append(conds, `st.status = 'rated' AND `+won)
			args = append(args, f.AccountID)
		}
	}
	if f.HasCharacter {
		if f.AccountID != 0 {
			conds = append(conds, `st.id IN (SELECT set_id FROM set_characters WHERE character = ? AND account_id = ?)`)
			args = append(args, f.Character, f.AccountID)
		} else {
			conds = append(conds, `st.id IN (SELECT set_id FROM set_characters WHERE character = ?)`)
			args = append(args, f.Character)
		}
	}
	where := strings.Join(conds, " AND ")

	var total int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM sets st WHERE `+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	sets, err := s.querySets(where, args, f.Limit, f.Offset)
	return sets, total, err
}

// CharacterUse is how many sets a player used a character in on one roster.
type CharacterUse struct {
	Ladder, Ruleset string
	Character       int
	Sets            int
}

// PlayerCharacters counts the sets an account played each character in,
// most used first.
func (s *Store) PlayerCharacters(accountID int64) ([]CharacterUse, error) {
	rows, err := s.db.Query(`SELECT st.ladder, st.ruleset_id, sc.character, COUNT(*) AS n
		FROM set_characters sc JOIN sets st ON st.id = sc.set_id
		WHERE sc.account_id = ? GROUP BY st.ladder, st.ruleset_id, sc.character ORDER BY n DESC, sc.character`, accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CharacterUse
	for rows.Next() {
		var u CharacterUse
		if err := rows.Scan(&u.Ladder, &u.Ruleset, &u.Character, &u.Sets); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// CharacterShare is how often a player picks a character on one roster.
type CharacterShare struct {
	Character int
	Sets      int // sets they played it in
	Percent   int // of their sets on the roster with characters recorded
}

// RosterCharacterShares returns, for every player of one roster, the
// characters they played there, most played first. A set where they used
// several characters counts for each.
func (s *Store) RosterCharacterShares(ladder, ruleset string) (map[int64][]CharacterShare, error) {
	rows, err := s.db.Query(`
		WITH played AS (
			SELECT sc.account_id, sc.set_id, sc.character
			FROM set_characters sc JOIN sets st ON st.id = sc.set_id
			WHERE st.ladder = ? AND st.ruleset_id = ?),
		totals AS (SELECT account_id, COUNT(DISTINCT set_id) AS total FROM played GROUP BY account_id)
		SELECT p.account_id, p.character, COUNT(*) AS n, t.total
		FROM played p JOIN totals t ON t.account_id = p.account_id
		GROUP BY p.account_id, p.character
		ORDER BY p.account_id, n DESC, p.character`, ladder, ruleset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64][]CharacterShare{}
	for rows.Next() {
		var (
			id    int64
			c     CharacterShare
			total int
		)
		if err := rows.Scan(&id, &c.Character, &c.Sets, &total); err != nil {
			return nil, err
		}
		if total > 0 {
			c.Percent = (c.Sets*100 + total/2) / total
		}
		out[id] = append(out[id], c)
	}
	return out, rows.Err()
}

// RosterCharacters lists the characters that have been played on each
// roster, keyed by ladder and ruleset.
func (s *Store) RosterCharacters() (map[[2]string][]int, error) {
	rows, err := s.db.Query(`SELECT DISTINCT st.ladder, st.ruleset_id, sc.character
		FROM set_characters sc JOIN sets st ON st.id = sc.set_id ORDER BY sc.character`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[[2]string][]int{}
	for rows.Next() {
		var ladder, ruleset string
		var c int
		if err := rows.Scan(&ladder, &ruleset, &c); err != nil {
			return nil, err
		}
		key := [2]string{ladder, ruleset}
		out[key] = append(out[key], c)
	}
	return out, rows.Err()
}
