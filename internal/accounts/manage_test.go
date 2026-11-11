package accounts

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestChangePassword(t *testing.T) {
	s := openTestStore(t)
	acct, _ := s.Register("Ace", "first password")
	token, _ := s.CreateSession(acct.ID)

	if err := s.ChangePassword(acct.ID, "wrong password", "second password"); !errors.Is(err, ErrBadCredentials) {
		t.Fatalf("wrong current password err = %v", err)
	}
	if err := s.ChangePassword(acct.ID, "first password", "short"); !errors.Is(err, ErrWeakPassword) {
		t.Fatalf("weak password err = %v", err)
	}
	if err := s.ChangePassword(acct.ID, "first password", "second password"); err != nil {
		t.Fatalf("ChangePassword: %v", err)
	}
	if _, err := s.Login("Ace", "first password"); !errors.Is(err, ErrBadCredentials) {
		t.Fatal("old password still works")
	}
	if _, err := s.Login("Ace", "second password"); err != nil {
		t.Fatalf("new password: %v", err)
	}
	if _, err := s.SessionAccount(token); !errors.Is(err, ErrInvalidSession) {
		t.Fatal("saved login survived the password change")
	}
}

func TestPasswordResetCode(t *testing.T) {
	s := openTestStore(t)
	now := time.Unix(1_800_000_000, 0)
	s.now = func() time.Time { return now }
	acct, _ := s.Register("Ace", "forgotten password")
	token, _ := s.CreateSession(acct.ID)

	if _, _, err := s.CreatePasswordReset("ghost", "mod"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown account err = %v", err)
	}
	code, got, err := s.CreatePasswordReset("ace", "mod")
	if err != nil || got.ID != acct.ID || len(code) != 14 {
		t.Fatalf("CreatePasswordReset = %q %+v %v", code, got, err)
	}
	if _, err := s.ResetPassword("Ace", "AAAA-AAAA-AAAA", "new password"); !errors.Is(err, ErrBadResetCode) {
		t.Fatalf("wrong code err = %v", err)
	}
	// typed sloppily: lower case, no dashes
	sloppy := strings.ToLower(strings.ReplaceAll(code, "-", ""))
	if _, err := s.ResetPassword("Ace", sloppy, "new password"); err != nil {
		t.Fatalf("ResetPassword: %v", err)
	}
	if _, err := s.Login("Ace", "new password"); err != nil {
		t.Fatalf("login with new password: %v", err)
	}
	if _, err := s.SessionAccount(token); !errors.Is(err, ErrInvalidSession) {
		t.Fatal("saved login survived the reset")
	}
	if _, err := s.ResetPassword("Ace", code, "another password"); !errors.Is(err, ErrBadResetCode) {
		t.Fatal("reset code worked twice")
	}

	code, _, _ = s.CreatePasswordReset("Ace", "mod")
	now = now.Add(ResetCodeTTL + time.Minute)
	if _, err := s.ResetPassword("Ace", code, "late password"); !errors.Is(err, ErrBadResetCode) {
		t.Fatal("expired reset code worked")
	}
}

func TestResolveDisputedSet(t *testing.T) {
	s := openTestStore(t)
	a, _ := s.Register("Ace", "password123")
	b, _ := s.Register("Bee", "password123")
	players := []SetPlayer{{AccountID: a.ID, Slot: 1}, {AccountID: b.ID, Slot: 2}}
	disputed, _, err := s.RecordSetWithID(SetRecord{Ladder: "remix", MatchID: 1, BootID: "b",
		Status: SetDisputed, Reason: "reports_disagree", Players: players})
	if err != nil {
		t.Fatal(err)
	}
	voided, _, _ := s.RecordSetWithID(SetRecord{Ladder: "remix", MatchID: 2, BootID: "b",
		Status: SetDisputed, Reason: "integrity: desync", Players: players})

	list, err := s.DisputedSets(10)
	if err != nil || len(list) != 2 || list[0].ID != disputed {
		t.Fatalf("DisputedSets = %+v, %v", list, err)
	}

	if _, err := s.ResolveSet(disputed, 999, "x"); err == nil {
		t.Fatal("resolved for someone who didn't play")
	}
	outcomes, err := s.ResolveSet(disputed, b.ID, "resolved by mod")
	if err != nil {
		t.Fatalf("ResolveSet: %v", err)
	}
	for _, o := range outcomes {
		if (o.AccountID == b.ID) != (o.After.Rating > o.Before.Rating) {
			t.Fatalf("rating moved the wrong way: %+v", o)
		}
	}
	set, _ := s.SetByID(disputed)
	if set.Status != SetRated || set.WinnerID != b.ID || set.Reason != "resolved by mod" {
		t.Fatalf("resolved set = %+v", set)
	}
	if r, _ := s.Rating(b.ID, "remix", ""); r.SetsPlayed != 1 {
		t.Fatalf("winner sets played = %d", r.SetsPlayed)
	}
	if _, err := s.ResolveSet(disputed, a.ID, "again"); !errors.Is(err, ErrNotDisputed) {
		t.Fatalf("second resolution err = %v", err)
	}

	if _, err := s.ResolveSet(voided, 0, "voided by mod"); err != nil {
		t.Fatalf("void: %v", err)
	}
	if set, _ := s.SetByID(voided); set.Status != SetVoided || set.WinnerID != 0 {
		t.Fatalf("voided set = %+v", set)
	}
	if r, _ := s.Rating(a.ID, "remix", ""); r.SetsPlayed != 1 {
		t.Fatalf("voiding changed sets played: %d", r.SetsPlayed)
	}
}
