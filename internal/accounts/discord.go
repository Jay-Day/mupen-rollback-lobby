package accounts

import (
	"database/sql"
	"errors"
	"strings"
	"time"
)

// Discord links: an optional, easier way to sign in. Every account still has
// a password; a link only lets the player sign in through Discord instead.

// ErrDiscordInUse is returned when a Discord account is already linked to a
// different RMG-K account.
var ErrDiscordInUse = errors.New("discord account linked to another account")

// DiscordLink is an account's linked Discord account.
type DiscordLink struct {
	AccountID   int64
	DiscordID   string
	Username    string
	Name        string // display name
	LinkedAt    time.Time
	LastLoginAt time.Time
}

// LinkDiscord links (or re-links) an account to a Discord account.
func (s *Store) LinkDiscord(accountID int64, discordID, username, name string) error {
	now := s.now().Unix()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var owner int64
	err = tx.QueryRow(`SELECT account_id FROM discord_links WHERE discord_id = ?`, discordID).Scan(&owner)
	if err == nil && owner != accountID {
		return ErrDiscordInUse
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO discord_links (account_id, discord_id, discord_username, discord_name, linked_at, last_login_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(account_id) DO UPDATE SET discord_id = excluded.discord_id,
			discord_username = excluded.discord_username, discord_name = excluded.discord_name,
			linked_at = excluded.linked_at, last_login_at = excluded.last_login_at`,
		accountID, discordID, clampName(username), clampName(name), now, now); err != nil {
		return err
	}
	return tx.Commit()
}

// DiscordLogin signs in the account linked to a Discord account, refreshing
// the stored Discord names. ErrNotFound when nothing is linked.
func (s *Store) DiscordLogin(discordID, username, name string) (Account, error) {
	var (
		account  Account
		created  int64
		disabled bool
	)
	err := s.db.QueryRow(`SELECT a.id, a.username, a.created_at, a.disabled
		FROM discord_links d JOIN accounts a ON a.id = d.account_id WHERE d.discord_id = ?`, discordID).
		Scan(&account.ID, &account.Username, &created, &disabled)
	if errors.Is(err, sql.ErrNoRows) {
		return Account{}, ErrNotFound
	}
	if err != nil {
		return Account{}, err
	}
	if disabled {
		return Account{}, ErrAccountDisabled
	}
	if _, err := s.db.Exec(`UPDATE discord_links SET discord_username = ?, discord_name = ?, last_login_at = ?
		WHERE discord_id = ?`, clampName(username), clampName(name), s.now().Unix(), discordID); err != nil {
		return Account{}, err
	}
	account.CreatedAt = time.Unix(created, 0)
	return account, nil
}

// DiscordLinkOf returns an account's Discord link, if any.
func (s *Store) DiscordLinkOf(accountID int64) (DiscordLink, bool, error) {
	var (
		link             DiscordLink
		linked, lastSeen int64
	)
	err := s.db.QueryRow(`SELECT account_id, discord_id, discord_username, discord_name, linked_at, last_login_at
		FROM discord_links WHERE account_id = ?`, accountID).
		Scan(&link.AccountID, &link.DiscordID, &link.Username, &link.Name, &linked, &lastSeen)
	if errors.Is(err, sql.ErrNoRows) {
		return DiscordLink{}, false, nil
	}
	if err != nil {
		return DiscordLink{}, false, err
	}
	link.LinkedAt, link.LastLoginAt = time.Unix(linked, 0), time.Unix(lastSeen, 0)
	return link, true, nil
}

// UnlinkDiscord removes an account's Discord link (a no-op when none).
func (s *Store) UnlinkDiscord(accountID int64) error {
	_, err := s.db.Exec(`DELETE FROM discord_links WHERE account_id = ?`, accountID)
	return err
}

func clampName(name string) string {
	runes := []rune(strings.TrimSpace(name))
	if len(runes) > 64 {
		runes = runes[:64]
	}
	return string(runes)
}
