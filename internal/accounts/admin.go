package accounts

import (
	"database/sql"
	"errors"
	"strings"
	"time"
)

// AccountStatus is an account as moderators see it, disabled ones included.
type AccountStatus struct {
	Account
	Disabled bool
	// remember-me logins that still work
	Sessions int
	// a reset code was issued and is still valid
	ResetPending bool
}

const accountStatusQuery = `SELECT a.id, a.username, a.created_at, a.disabled,
		(SELECT COUNT(*) FROM sessions s WHERE s.account_id = a.id AND s.expires_at >= ?),
		EXISTS (SELECT 1 FROM password_resets r WHERE r.account_id = a.id AND r.expires_at >= ?)
	FROM accounts a`

func scanAccountStatus(scan func(...any) error) (AccountStatus, error) {
	var (
		status  AccountStatus
		created int64
	)
	err := scan(&status.ID, &status.Username, &created, &status.Disabled, &status.Sessions, &status.ResetPending)
	status.CreatedAt = time.Unix(created, 0)
	return status, err
}

// AccountStatusByName looks up an account (case-insensitive), disabled or not.
func (s *Store) AccountStatusByName(name string) (AccountStatus, error) {
	now := s.now().Unix()
	status, err := scanAccountStatus(s.db.QueryRow(accountStatusQuery+` WHERE a.username = ?`,
		now, now, strings.TrimSpace(name)).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return AccountStatus{}, ErrNotFound
	}
	return status, err
}

// SearchAccounts lists accounts whose name contains query, by name.
func (s *Store) SearchAccounts(query string, limit int) ([]AccountStatus, error) {
	if limit <= 0 || limit > 100 {
		limit = 25
	}
	escaped := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(strings.TrimSpace(query))
	now := s.now().Unix()
	rows, err := s.db.Query(accountStatusQuery+` WHERE a.username LIKE ? ESCAPE '\' ORDER BY a.username LIMIT ?`,
		now, now, "%"+escaped+"%", limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AccountStatus{}
	for rows.Next() {
		status, err := scanAccountStatus(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, status)
	}
	return out, rows.Err()
}

// SetDisabled disables or re-enables an account. A disabled account can't
// log in, leaves the leaderboard, and loses its saved logins and reset code.
func (s *Store) SetDisabled(accountID int64, disabled bool) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`UPDATE accounts SET disabled = ? WHERE id = ?`, disabled, accountID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	if disabled {
		if _, err := tx.Exec(`DELETE FROM sessions WHERE account_id = ?`, accountID); err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM password_resets WHERE account_id = ?`, accountID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// AdminLogEntry is one moderator action.
type AdminLogEntry struct {
	ID     int64
	Time   time.Time
	Admin  string
	Action string
	Detail string
}

// AddAdminLog records a moderator action.
func (s *Store) AddAdminLog(admin, action, detail string) error {
	_, err := s.db.Exec(`INSERT INTO admin_log (created_at, admin, action, detail) VALUES (?, ?, ?, ?)`,
		s.now().Unix(), admin, action, detail)
	return err
}

// AdminLog returns the newest moderator actions first.
func (s *Store) AdminLog(limit int) ([]AdminLogEntry, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.db.Query(`SELECT id, created_at, admin, action, detail FROM admin_log
		ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AdminLogEntry{}
	for rows.Next() {
		var (
			entry   AdminLogEntry
			created int64
		)
		if err := rows.Scan(&entry.ID, &created, &entry.Admin, &entry.Action, &entry.Detail); err != nil {
			return nil, err
		}
		entry.Time = time.Unix(created, 0)
		out = append(out, entry)
	}
	return out, rows.Err()
}
