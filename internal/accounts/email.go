package accounts

import (
	"crypto/subtle"
	"database/sql"
	"errors"
	"net/mail"
	"strings"
	"time"
	"unicode"
)

// An account can have one confirmed email address. Its only use is emailing
// the owner a password reset code. A new address takes effect once the code
// sent to it comes back.

// EmailCodeTTL is how long an emailed code (address confirmation or password
// reset) works.
const EmailCodeTTL = time.Hour

// MaxEmailLen is the longest address accepted (the SMTP path limit).
const MaxEmailLen = 254

// emailResetCreatedBy marks emailed reset codes in password_resets.
const emailResetCreatedBy = "email"

var (
	ErrBadEmail      = errors.New("that isn't a valid email address")
	ErrBadEmailCode  = errors.New("confirmation code is wrong or expired")
	ErrNoEmail       = errors.New("the account has no email address")
	ErrEmailMismatch = errors.New("that isn't the account's email address")
)

// NormalizeEmail checks that s is a plain address such as ace@example.com
// (no display name, no spaces) and returns it with the domain lower-cased.
func NormalizeEmail(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" || len(s) > MaxEmailLen {
		return "", ErrBadEmail
	}
	for _, r := range s {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return "", ErrBadEmail
		}
	}
	addr, err := mail.ParseAddress(s)
	if err != nil || addr.Name != "" || addr.Address != s {
		return "", ErrBadEmail
	}
	at := strings.LastIndex(s, "@")
	local, domain := s[:at], strings.ToLower(s[at+1:])
	if local == "" || !strings.Contains(domain, ".") || strings.HasPrefix(domain, ".") ||
		strings.HasSuffix(domain, ".") || strings.Contains(domain, "..") {
		return "", ErrBadEmail
	}
	return local + "@" + domain, nil
}

// AccountEmail is the account's confirmed address, "" when it has none.
func (s *Store) AccountEmail(accountID int64) (string, error) {
	var email string
	err := s.db.QueryRow(`SELECT email FROM accounts WHERE id = ?`, accountID).Scan(&email)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return email, err
}

// StartEmailChange records email as the account's address-to-be and returns
// the code to send to it. The address takes effect in ConfirmEmailChange. A
// new request replaces an unconfirmed one.
func (s *Store) StartEmailChange(accountID int64, email string) (code, normalized string, err error) {
	if normalized, err = NormalizeEmail(email); err != nil {
		return "", "", err
	}
	if code, err = newResetCode(); err != nil {
		return "", "", err
	}
	_, err = s.db.Exec(`INSERT INTO email_confirmations (account_id, email, code_hash, expires_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT (account_id) DO UPDATE SET email = excluded.email,
			code_hash = excluded.code_hash, expires_at = excluded.expires_at`,
		accountID, normalized, hashToken(normalizeResetCode(code)), s.now().Add(EmailCodeTTL).Unix())
	if err != nil {
		return "", "", err
	}
	return code, normalized, nil
}

// ConfirmEmailChange makes the pending address the account's email when code
// matches, and returns it. The code works once.
func (s *Store) ConfirmEmailChange(accountID int64, code string) (string, error) {
	var (
		email, hash string
		expires     int64
	)
	err := s.db.QueryRow(`SELECT email, code_hash, expires_at FROM email_confirmations WHERE account_id = ?`,
		accountID).Scan(&email, &hash, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrBadEmailCode
	}
	if err != nil {
		return "", err
	}
	given := hashToken(normalizeResetCode(code))
	if subtle.ConstantTimeCompare([]byte(given), []byte(hash)) != 1 || expires < s.now().Unix() {
		return "", ErrBadEmailCode
	}

	tx, err := s.db.Begin()
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE accounts SET email = ? WHERE id = ?`, email, accountID); err != nil {
		return "", err
	}
	if _, err := tx.Exec(`DELETE FROM email_confirmations WHERE account_id = ?`, accountID); err != nil {
		return "", err
	}
	return email, tx.Commit()
}

// RemoveEmail forgets the account's address and any unconfirmed one.
func (s *Store) RemoveEmail(accountID int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE accounts SET email = '' WHERE id = ?`, accountID); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM email_confirmations WHERE account_id = ?`, accountID); err != nil {
		return err
	}
	return tx.Commit()
}

// CreateEmailPasswordReset issues a reset code for the account's owner to be
// emailed to its confirmed address (valid EmailCodeTTL). The requester has
// to know that address: given must match it (ignoring case), so nobody can
// send reset emails to another player's inbox with just their name. It
// returns ErrNotFound for unknown or disabled accounts, ErrNoEmail when the
// account has no address and ErrEmailMismatch when given is a different one.
// A new code replaces the old one.
func (s *Store) CreateEmailPasswordReset(username, given string) (code string, account Account, email string, err error) {
	if account, err = s.AccountByName(username); err != nil {
		return "", Account{}, "", err
	}
	if email, err = s.AccountEmail(account.ID); err != nil {
		return "", Account{}, "", err
	}
	if email == "" {
		return "", Account{}, "", ErrNoEmail
	}
	if !strings.EqualFold(strings.TrimSpace(given), email) {
		return "", Account{}, "", ErrEmailMismatch
	}
	if code, err = s.issuePasswordReset(account.ID, emailResetCreatedBy, EmailCodeTTL); err != nil {
		return "", Account{}, "", err
	}
	return code, account, email, nil
}
