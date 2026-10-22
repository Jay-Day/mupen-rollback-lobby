package accounts

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestNormalizeEmail(t *testing.T) {
	for in, want := range map[string]string{
		"ace@example.com":          "ace@example.com",
		"  Ace.Smith@Example.COM ": "Ace.Smith@example.com",
		"a+lobby@mail.example.org": "a+lobby@mail.example.org",
	} {
		if got, err := NormalizeEmail(in); err != nil || got != want {
			t.Errorf("NormalizeEmail(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{
		"", "ace", "ace@", "@example.com", "ace@localhost", "ace@example..com", "ace@.example.com",
		"Ace <ace@example.com>", "ace @example.com", "ace@example.com\r\nBcc: x@example.com",
		strings.Repeat("a", MaxEmailLen) + "@example.com",
	} {
		if got, err := NormalizeEmail(bad); !errors.Is(err, ErrBadEmail) {
			t.Errorf("NormalizeEmail(%q) = %q, %v; want ErrBadEmail", bad, got, err)
		}
	}
}

func TestEmailChangeNeedsTheEmailedCode(t *testing.T) {
	s := openTestStore(t)
	now := time.Unix(1_800_000_000, 0)
	s.now = func() time.Time { return now }
	acct, _ := s.Register("Ace", "first password")

	if email, err := s.AccountEmail(acct.ID); err != nil || email != "" {
		t.Fatalf("new account email = %q, %v", email, err)
	}
	if _, _, err := s.StartEmailChange(acct.ID, "not an address"); !errors.Is(err, ErrBadEmail) {
		t.Fatalf("bad address err = %v", err)
	}
	code, normalized, err := s.StartEmailChange(acct.ID, "Ace@Example.com")
	if err != nil || normalized != "Ace@example.com" || len(code) != 14 {
		t.Fatalf("StartEmailChange = %q %q %v", code, normalized, err)
	}
	// nothing changes until the code comes back
	if email, _ := s.AccountEmail(acct.ID); email != "" {
		t.Fatalf("unconfirmed address already active: %q", email)
	}
	if _, err := s.ConfirmEmailChange(acct.ID, "AAAA-AAAA-AAAA"); !errors.Is(err, ErrBadEmailCode) {
		t.Fatalf("wrong code err = %v", err)
	}
	if email, err := s.ConfirmEmailChange(acct.ID, strings.ToLower(code)); err != nil || email != "Ace@example.com" {
		t.Fatalf("ConfirmEmailChange = %q, %v", email, err)
	}
	if email, _ := s.AccountEmail(acct.ID); email != "Ace@example.com" {
		t.Fatalf("confirmed address = %q", email)
	}
	if _, err := s.ConfirmEmailChange(acct.ID, code); !errors.Is(err, ErrBadEmailCode) {
		t.Fatal("confirmation code worked twice")
	}

	// a change to a new address keeps the old one until it's confirmed
	code, _, _ = s.StartEmailChange(acct.ID, "new@example.com")
	now = now.Add(EmailCodeTTL + time.Minute)
	if _, err := s.ConfirmEmailChange(acct.ID, code); !errors.Is(err, ErrBadEmailCode) {
		t.Fatal("expired confirmation code worked")
	}
	if email, _ := s.AccountEmail(acct.ID); email != "Ace@example.com" {
		t.Fatalf("address after an expired change = %q", email)
	}

	if err := s.RemoveEmail(acct.ID); err != nil {
		t.Fatal(err)
	}
	if email, _ := s.AccountEmail(acct.ID); email != "" {
		t.Fatalf("address after removal = %q", email)
	}
}

func TestEmailPasswordReset(t *testing.T) {
	s := openTestStore(t)
	now := time.Unix(1_800_000_000, 0)
	s.now = func() time.Time { return now }
	acct, _ := s.Register("Ace", "forgotten password")
	s.Register("Bee", "another password")

	if _, _, _, err := s.CreateEmailPasswordReset("ghost", "ghost@example.com"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown account err = %v", err)
	}
	if _, _, _, err := s.CreateEmailPasswordReset("Bee", "bee@example.com"); !errors.Is(err, ErrNoEmail) {
		t.Fatalf("account without email err = %v", err)
	}

	confirm, _, _ := s.StartEmailChange(acct.ID, "ace@example.com")
	if _, err := s.ConfirmEmailChange(acct.ID, confirm); err != nil {
		t.Fatal(err)
	}
	modCode, _, _ := s.CreatePasswordReset("Ace", "mod")
	// the requester must know the account's address
	if _, _, _, err := s.CreateEmailPasswordReset("Ace", "someone@example.com"); !errors.Is(err, ErrEmailMismatch) {
		t.Fatalf("wrong address err = %v", err)
	}
	// and a wrong address leaves the moderator's code working
	if _, err := s.ResetPassword("Ace", modCode, "unused password"); err != nil {
		t.Fatalf("moderator code after a wrong-address request: %v", err)
	}
	modCode, _, _ = s.CreatePasswordReset("Ace", "mod")
	code, got, email, err := s.CreateEmailPasswordReset("ace", " ACE@Example.com ")
	if err != nil || got.ID != acct.ID || email != "ace@example.com" || len(code) != 14 {
		t.Fatalf("CreateEmailPasswordReset = %q %+v %q %v", code, got, email, err)
	}
	if _, err := s.ResetPassword("Ace", modCode, "new password"); !errors.Is(err, ErrBadResetCode) {
		t.Fatal("an older moderator code still works after an emailed one")
	}
	if _, err := s.ResetPassword("Ace", code, "new password"); err != nil {
		t.Fatalf("ResetPassword with the emailed code: %v", err)
	}
	if _, err := s.Login("Ace", "new password"); err != nil {
		t.Fatalf("login with new password: %v", err)
	}

	// emailed codes expire sooner than a moderator's
	code, _, _, _ = s.CreateEmailPasswordReset("Ace", "ace@example.com")
	now = now.Add(EmailCodeTTL + time.Minute)
	if _, err := s.ResetPassword("Ace", code, "late password"); !errors.Is(err, ErrBadResetCode) {
		t.Fatal("expired emailed code worked")
	}

	if err := s.SetDisabled(acct.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := s.CreateEmailPasswordReset("Ace", "ace@example.com"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("disabled account err = %v", err)
	}
}
