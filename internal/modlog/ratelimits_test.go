package modlog

import (
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/jay-day/rmgk-lobby/internal/ratelimit"
)

func TestRateLimitRecordsRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "modlog.db")
	l, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	older := ratelimit.ActionRecord{IP: "203.0.113.7", FirstTrip: at, LastTrip: at.Add(time.Minute),
		Refused: map[ratelimit.ActionKind]int{ratelimit.ActionRoomCreate: 40}, TotalRefused: 40,
		Usernames: []string{"Ace", "Bee"}, AutoBans: 1, AutoBannedAt: at.Add(time.Minute), BannedUntil: at.Add(time.Hour),
		Allowed: map[ratelimit.ActionKind]int{ratelimit.ActionRoomCreate: 5}}
	newer := ratelimit.ActionRecord{IP: "2001:db8::9", FirstTrip: at, LastTrip: at.Add(2 * time.Minute),
		Refused: map[ratelimit.ActionKind]int{ratelimit.ActionSpectate: 1}, TotalRefused: 1}
	if !l.SaveRateLimits([]ratelimit.ActionRecord{older, newer}, nil) {
		t.Fatal("queue full")
	}
	l.Flush()
	got, err := l.RateLimits()
	if err != nil {
		t.Fatal(err)
	}
	want := older
	want.Allowed = nil // the current windows aren't kept
	if len(got) != 2 || got[0].IP != newer.IP || !reflect.DeepEqual(got[1], want) {
		t.Fatalf("read back %+v", got)
	}

	// an update and a removal in one write, and they survive reopening
	newer.TotalRefused = 9
	if !l.SaveRateLimits([]ratelimit.ActionRecord{newer}, []string{older.IP}) {
		t.Fatal("queue full")
	}
	l.Close()
	if l, err = Open(path); err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if got, err = l.RateLimits(); err != nil || len(got) != 1 || got[0].IP != newer.IP || got[0].TotalRefused != 9 {
		t.Fatalf("after update and reopen: %+v (%v)", got, err)
	}

	var nilLog *Logger
	if !nilLog.SaveRateLimits([]ratelimit.ActionRecord{newer}, nil) {
		t.Fatal("a nil log should accept and drop")
	}
	if recs, err := nilLog.RateLimits(); recs != nil || err != nil {
		t.Fatal("a nil log holds nothing")
	}
}
