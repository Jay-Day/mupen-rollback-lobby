package accounts

import (
	"crypto/rand"
	"crypto/subtle"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// ResetCodeTTL is how long a moderator-issued password reset code works.
const ResetCodeTTL = 24 * time.Hour

var (
	ErrBadResetCode = errors.New("reset code is wrong or expired")
	ErrNotDisputed  = errors.New("set is not disputed")
)

// setPassword stores a new password and signs the account out everywhere.
func setPassword(tx *sql.Tx, accountID int64, password string) error {
	hash, err := hashPassword(password)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE accounts SET pw_hash = ? WHERE id = ?`, hash, accountID); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM sessions WHERE account_id = ?`, accountID); err != nil {
		return err
	}
	_, err = tx.Exec(`DELETE FROM password_resets WHERE account_id = ?`, accountID)
	return err
}

// ChangePassword replaces an account's password after checking the current
// one. Every remember-me token of the account stops working.
func (s *Store) ChangePassword(accountID int64, current, next string) error {
	if !validPassword(next) {
		return ErrWeakPassword
	}
	var hash string
	err := s.db.QueryRow(`SELECT pw_hash FROM accounts WHERE id = ?`, accountID).Scan(&hash)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	ok, err := verifyPassword(hash, current)
	if err != nil {
		return err
	}
	if !ok {
		return ErrBadCredentials
	}

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := setPassword(tx, accountID, next); err != nil {
		return err
	}
	return tx.Commit()
}

// resetCodeAlphabet avoids look-alike characters (0/O, 1/I/L).
const resetCodeAlphabet = "ABCDEFGHJKMNPQRSTUVWXYZ23456789"

func newResetCode() (string, error) {
	raw := make([]byte, 12)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	var b strings.Builder
	for i, v := range raw {
		if i > 0 && i%4 == 0 {
			b.WriteByte('-')
		}
		b.WriteByte(resetCodeAlphabet[int(v)%len(resetCodeAlphabet)])
	}
	return b.String(), nil
}

// normalizeResetCode accepts codes typed in any case, with or without dashes.
func normalizeResetCode(code string) string {
	return strings.Map(func(r rune) rune {
		if r == '-' || r == ' ' {
			return -1
		}
		return r
	}, strings.ToUpper(strings.TrimSpace(code)))
}

// CreatePasswordReset issues a one-time code a moderator hands to the
// account's owner. A new code replaces the old one, emailed or not.
func (s *Store) CreatePasswordReset(username, createdBy string) (string, Account, error) {
	account, err := s.AccountByName(username)
	if err != nil {
		return "", Account{}, err
	}
	code, err := s.issuePasswordReset(account.ID, createdBy, ResetCodeTTL)
	if err != nil {
		return "", Account{}, err
	}
	return code, account, nil
}

// issuePasswordReset stores a new reset code for the account, replacing any
// earlier one, and returns it.
func (s *Store) issuePasswordReset(accountID int64, createdBy string, ttl time.Duration) (string, error) {
	code, err := newResetCode()
	if err != nil {
		return "", err
	}
	_, err = s.db.Exec(`INSERT INTO password_resets (account_id, code_hash, expires_at, created_by)
		VALUES (?, ?, ?, ?)
		ON CONFLICT (account_id) DO UPDATE SET code_hash = excluded.code_hash,
			expires_at = excluded.expires_at, created_by = excluded.created_by`,
		accountID, hashToken(normalizeResetCode(code)), s.now().Add(ttl).Unix(), createdBy)
	if err != nil {
		return "", err
	}
	return code, nil
}

// ResetPassword sets a new password with a reset code and signs the account
// out everywhere. The code works once.
func (s *Store) ResetPassword(username, code, next string) (Account, error) {
	if !validPassword(next) {
		return Account{}, ErrWeakPassword
	}
	var (
		account  Account
		created  int64
		disabled bool
		codeHash string
		expires  int64
	)
	err := s.db.QueryRow(`SELECT a.id, a.username, a.created_at, a.disabled, r.code_hash, r.expires_at
		FROM accounts a JOIN password_resets r ON r.account_id = a.id WHERE a.username = ?`,
		username).Scan(&account.ID, &account.Username, &created, &disabled, &codeHash, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return Account{}, ErrBadResetCode
	}
	if err != nil {
		return Account{}, err
	}
	given := hashToken(normalizeResetCode(code))
	if subtle.ConstantTimeCompare([]byte(given), []byte(codeHash)) != 1 || expires < s.now().Unix() {
		return Account{}, ErrBadResetCode
	}
	if disabled {
		return Account{}, ErrAccountDisabled
	}

	tx, err := s.db.Begin()
	if err != nil {
		return Account{}, err
	}
	defer tx.Rollback()
	if err := setPassword(tx, account.ID, next); err != nil {
		return Account{}, err
	}
	if err := tx.Commit(); err != nil {
		return Account{}, err
	}
	account.CreatedAt = time.Unix(created, 0)
	return account, nil
}

// DisputedSets returns the sets waiting for a moderator, oldest first.
func (s *Store) DisputedSets(limit int) ([]SetSummary, error) {
	sets, err := s.querySets(`st.status = ?`, []any{string(SetDisputed)}, limit, 0)
	for i, j := 0, len(sets)-1; i < j; i, j = i+1, j-1 {
		sets[i], sets[j] = sets[j], sets[i]
	}
	return sets, err
}

// DisputedCount is how many sets wait for a moderator.
func (s *Store) DisputedCount() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM sets WHERE status = ?`, string(SetDisputed)).Scan(&n)
	return n, err
}

// ResolveSet settles a disputed set: winnerID rates it like an agreed set
// (from the players' current ratings), 0 voids it. note is kept as the reason.
func (s *Store) ResolveSet(setID, winnerID int64, note string) ([]PlayerOutcome, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var ladder, ruleset, status, resultJSON string
	err = tx.QueryRow(`SELECT ladder, ruleset_id, status, result_json FROM sets WHERE id = ?`, setID).Scan(
		&ladder, &ruleset, &status, &resultJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if SetStatus(status) != SetDisputed {
		return nil, ErrNotDisputed
	}

	rows, err := tx.Query(`SELECT account_id, team FROM set_players WHERE set_id = ? ORDER BY slot`, setID)
	if err != nil {
		return nil, err
	}
	var players []setParticipant
	for rows.Next() {
		var p setParticipant
		if err := rows.Scan(&p.AccountID, &p.Team); err != nil {
			rows.Close()
			return nil, err
		}
		players = append(players, p)
	}
	rows.Close()

	// in a team set, the winner's team wins
	newStatus := SetVoided
	var winner any
	winnerTeam := 0
	if winnerID != 0 {
		found := false
		for _, p := range players {
			if p.AccountID == winnerID {
				found = true
				winnerTeam = p.Team
			}
		}
		if !found {
			return nil, errors.New("the winner must have played the set")
		}
		newStatus = SetRated
		if winnerTeam == 0 {
			winner = winnerID
		}
	}
	outcomes, err := s.rateSet(tx, ladder, ruleset, players, winnerID, winnerTeam,
		reportedGames(resultJSON, winnerTeam), winnerID != 0, s.now())
	if err != nil {
		return nil, err
	}
	for _, o := range outcomes {
		_, err := tx.Exec(`UPDATE set_players SET rating_before = ?, deviation_before = ?,
			rating_after = ?, deviation_after = ? WHERE set_id = ? AND account_id = ?`,
			o.Before.Rating, o.Before.Deviation, o.After.Rating, o.After.Deviation, setID, o.AccountID)
		if err != nil {
			return nil, err
		}
	}
	if _, err := tx.Exec(`UPDATE sets SET status = ?, winner_id = ?, winner_team = ?, reason = ? WHERE id = ?`,
		string(newStatus), winner, winnerTeam, note, setID); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return outcomes, nil
}
