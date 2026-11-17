package config

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jay-day/rmgk-lobby/internal/ratelimit"
)

func TestParseKeepsDefaultsForMissingKeys(t *testing.T) {
	s, unknown, err := Parse([]byte(`{"minClientVersion": "0.9.15"}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(unknown) != 0 {
		t.Fatalf("unexpected unknown keys %v", unknown)
	}
	if got := s.MinClient().String(); got != "0.9.15" {
		t.Fatalf("MinClient = %s, want 0.9.15", got)
	}
	d := Defaults()
	if s.LiveReplayKeyframeSeconds != d.LiveReplayKeyframeSeconds || s.ConnectAttempts != d.ConnectAttempts {
		t.Fatalf("omitted keys lost their defaults: %+v", s)
	}
	if !strings.Contains(s.OutdatedMessage(), "0.9.15") {
		t.Fatalf("default message doesn't name the version: %q", s.OutdatedMessage())
	}
}

func TestParseEmptyIsDefaults(t *testing.T) {
	s, _, err := Parse([]byte("  \n"))
	if err != nil {
		t.Fatal(err)
	}
	if s.MinClientVersion != Defaults().MinClientVersion {
		t.Fatalf("empty file changed minClientVersion to %q", s.MinClientVersion)
	}
}

func TestParseReportsUnknownKeys(t *testing.T) {
	_, unknown, err := Parse([]byte(`{"minClientVerison": "0.9.15", "liveReplayKeyframeSeconds": 20}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(unknown) != 1 || unknown[0] != "minClientVerison" {
		t.Fatalf("unknown = %v, want [minClientVerison]", unknown)
	}
}

func TestParseRejectsInvalid(t *testing.T) {
	for _, data := range []string{
		`{"minClientVersion": "0.9.15-beta"}`,
		`{"minClientVersion": ""}`,
		`{"liveReplayKeyframeSeconds": 2}`,
		`{"liveReplayKeyframeSeconds": 3600}`,
		`{"quickMatchWarmupSeconds": 0}`,
		`{"maxConnectionsPerIP": 0}`,
		`{"connectAttempts": "5"}`,
		`{"chatLogDays": -1}`,
		`{"connectionLogDays": 4000}`,
		`{"gameLogDays": "365"}`,
		`{"adminAllowedIPs": "203.0.113.7"}`,
		`{"adminAllowedIPs": ["203.0.113.7", "my-laptop"]}`,
		`{"adminAllowedIPs": ["203.0.113.0/33"]}`,
		`{"adminAllowedIPs": ["fe80::1%eth0"]}`,
		`{"minClientVersion": "0.9.15",}`,
		`[]`,
	} {
		if _, _, err := Parse([]byte(data)); err == nil {
			t.Errorf("Parse(%s) succeeded, want error", data)
		}
	}
}

func TestLogRetentionSettings(t *testing.T) {
	d := Defaults()
	if d.ChatLogDays != 90 || d.ConnectionLogDays != 90 || d.GameLogDays != 365 {
		t.Fatalf("log defaults = %d %d %d", d.ChatLogDays, d.ConnectionLogDays, d.GameLogDays)
	}
	s, unknown, err := Parse([]byte(`{"chatLogDays": 0, "connectionLogDays": 30, "gameLogDays": 3650}`))
	if err != nil || len(unknown) != 0 {
		t.Fatalf("Parse: %v %v", err, unknown)
	}
	if s.ChatLogDays != 0 || s.ConnectionLogDays != 30 || s.GameLogDays != 3650 {
		t.Fatalf("parsed = %+v", s)
	}
}

func TestAdminAllowedIPs(t *testing.T) {
	// empty: everyone (the listen address is the only limit)
	for _, data := range []string{`{}`, `{"adminAllowedIPs": []}`, `{"adminAllowedIPs": null}`} {
		s, _, err := Parse([]byte(data))
		if err != nil {
			t.Fatalf("Parse(%s): %v", data, err)
		}
		if !s.AdminIPAllowed("198.51.100.1") || len(s.AdminAllowed()) != 0 || s.AdminAllowedIPs == nil {
			t.Fatalf("%s restricted the site: %+v", data, s.AdminAllowed())
		}
	}
	var nilStore *Store
	if !nilStore.Get().AdminIPAllowed("198.51.100.1") {
		t.Fatal("built-in defaults restricted the site")
	}

	s, _, err := Parse([]byte(`{"adminAllowedIPs": ["203.0.113.7", " 198.51.100.0/24 ", "2001:db8::/32", "10.1.2.3/8"]}`))
	if err != nil {
		t.Fatal(err)
	}
	for ip, want := range map[string]bool{
		"203.0.113.7":         true,
		"203.0.113.8":         false,
		"198.51.100.200":      true,
		"198.51.101.1":        false,
		"2001:db8:5::9":       true,
		"2001:db9::1":         false,
		"10.200.0.1":          true, // 10.1.2.3/8 means 10.0.0.0/8
		"::ffff:203.0.113.7":  true, // IPv4-mapped is the same address
		"127.0.0.1":           true, // loopback, always
		"127.9.9.9":           true,
		"::1":                 true,
		"not-an-ip":           false,
		"":                    false,
		"203.0.113.7:8081":    false, // an address with a port isn't an IP
		"[2001:db8::1]":       false,
		"fe80::1%eth0":        false,
		"2001:db8::1%eth0":    true, // the zone doesn't matter for the range
		"::ffff:198.51.100.9": true,
	} {
		if got := s.AdminIPAllowed(ip); got != want {
			t.Errorf("AdminIPAllowed(%q) = %v, want %v", ip, got, want)
		}
	}
}

func TestReloadLogsListsReadably(t *testing.T) {
	a, _, _ := Parse([]byte(`{}`))
	b, _, _ := Parse([]byte(`{"adminAllowedIPs": ["203.0.113.7", "10.0.0.0/8"], "chatLogDays": 30}`))
	changes := diff(a, b)
	got := map[string]string{}
	for i := 0; i+1 < len(changes); i += 2 {
		got[changes[i].(string)] = changes[i+1].(string)
	}
	if got["adminAllowedIPs"] != `["203.0.113.7","10.0.0.0/8"] (was [])` {
		t.Fatalf("adminAllowedIPs logged as %q", got["adminAllowedIPs"])
	}
	if got["chatLogDays"] != "30 (was 90)" {
		t.Fatalf("chatLogDays logged as %q", got["chatLogDays"])
	}
	if len(got) != 2 {
		t.Fatalf("changes = %v", changes)
	}
}

func TestOutdatedMessageOverride(t *testing.T) {
	s, _, err := Parse([]byte(`{"outdatedClientMessage": "Update at example.com"}`))
	if err != nil {
		t.Fatal(err)
	}
	if s.OutdatedMessage() != "Update at example.com" {
		t.Fatalf("OutdatedMessage = %q", s.OutdatedMessage())
	}
}

func TestNilStoreGivesDefaults(t *testing.T) {
	var st *Store
	if st.Get().MinClientVersion != Defaults().MinClientVersion {
		t.Fatal("nil store didn't return defaults")
	}
}

func TestStoreReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lobby.json")
	st, err := Open(path)
	if err != nil {
		t.Fatalf("missing file should load defaults: %v", err)
	}
	if st.Get().MinClientVersion != "0.9.13b" {
		t.Fatalf("defaults: minClientVersion = %q", st.Get().MinClientVersion)
	}

	write(t, path, `{"minClientVersion": "0.9.15", "liveReplayKeyframeSeconds": 20}`)
	st.Reload()
	if got := st.Get(); got.MinClientVersion != "0.9.15" || got.LiveReplayKeyframeSeconds != 20 {
		t.Fatalf("after reload: %+v", got)
	}

	// A broken edit keeps the last good settings.
	write(t, path, `{"minClientVersion": `)
	st.Reload()
	if st.Get().MinClientVersion != "0.9.15" {
		t.Fatalf("invalid file replaced settings: %+v", st.Get())
	}

	// Deleting the file restores the defaults.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	st.Reload()
	if st.Get().MinClientVersion != "0.9.13b" {
		t.Fatalf("after delete: minClientVersion = %q", st.Get().MinClientVersion)
	}
}

func TestOpenRejectsInvalidFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lobby.json")
	write(t, path, `{"minClientVersion": "latest"}`)
	if _, err := Open(path); err == nil {
		t.Fatal("Open accepted an invalid file")
	}
}

func TestWatchFileSeesCreateEditAndDelete(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lobby.json")
	changed := make(chan struct{}, 8)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go WatchFile(ctx, path, 10*time.Millisecond, func() { changed <- struct{}{} })

	expect := func(what string) {
		t.Helper()
		select {
		case <-changed:
		case <-time.After(2 * time.Second):
			t.Fatalf("no change reported after %s", what)
		}
	}
	time.Sleep(30 * time.Millisecond) // let the watcher take its first stamp
	write(t, path, `{}`)
	expect("create")
	write(t, path, `{"minClientVersion": "0.9.15"}`)
	expect("edit")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	expect("delete")
}

func TestReleaseVersionOrder(t *testing.T) {
	order := []string{"0.9", "0.9.13", "0.9.13a", "0.9.13b", "v0.9.14", "0.10.0", "1.0.0"}
	for i := 1; i < len(order); i++ {
		a, okA := ParseReleaseVersion(order[i-1])
		b, okB := ParseReleaseVersion(order[i])
		if !okA || !okB {
			t.Fatalf("parse failed for %s or %s", order[i-1], order[i])
		}
		if !a.Less(b) || b.Less(a) {
			t.Fatalf("expected %s < %s", order[i-1], order[i])
		}
	}
}

func write(t *testing.T, path, data string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestRateLimitDefaults(t *testing.T) {
	want := ratelimit.DefaultActionLimits()
	for _, s := range []*Settings{Defaults(), (*Store)(nil).Get()} {
		got := s.ActionLimits()
		if !reflect.DeepEqual(got.Rates, want.Rates) || got.BanStrikes != 40 || got.BanWindow != 10*time.Minute ||
			got.BanDuration != time.Hour || got.ReportTTL != 7*24*time.Hour || got.MaxIPs != ratelimit.DefaultActionMaxIPs {
			t.Fatalf("default limits = %+v, want %+v", got, want)
		}
		if s.RoomListMinInterval() != 250*time.Millisecond {
			t.Fatalf("room list interval = %v", s.RoomListMinInterval())
		}
	}
	// Parse builds the same from an empty file
	s, _, err := Parse(nil)
	if err != nil || !reflect.DeepEqual(s.ActionLimits(), Defaults().ActionLimits()) {
		t.Fatalf("parsed defaults differ: %+v (%v)", s.ActionLimits(), err)
	}
}

func TestRateLimitSettings(t *testing.T) {
	s, unknown, err := Parse([]byte(`{"roomCreateLimit": 2, "roomCreateWindowSeconds": 30, "spectateLimit": 0,
		"lobbyUpdateWindowSeconds": 10, "abuseBanStrikes": 0, "abuseBanWindowSeconds": 120, "abuseBanSeconds": 600,
		"abuseReportDays": 3, "roomListMinIntervalMs": 0}`))
	if err != nil || len(unknown) != 0 {
		t.Fatalf("parse: %v %v", err, unknown)
	}
	l := s.ActionLimits()
	if r := l.Rates[ratelimit.ActionRoomCreate]; r.Count != 2 || r.Window != 30*time.Second {
		t.Fatalf("room_create = %+v", r)
	}
	if r := l.Rates[ratelimit.ActionSpectate]; r.Count != 0 {
		t.Fatalf("spectate 0 should lift the limit, got %+v", r)
	}
	if r := l.Rates[ratelimit.ActionLobbyUpdate]; r.Count != 30 || r.Window != 10*time.Second {
		t.Fatalf("lobby_update = %+v", r)
	}
	if l.BanStrikes != 0 || l.BanWindow != 2*time.Minute || l.BanDuration != 10*time.Minute || l.ReportTTL != 3*24*time.Hour {
		t.Fatalf("ban settings = %+v", l)
	}
	if s.RoomListMinInterval() != 0 {
		t.Fatal("roomListMinIntervalMs 0 should turn coalescing off")
	}
	// a reload logs each change with its old value
	changes := diff(Defaults(), s)
	got := map[string]string{}
	for i := 0; i+1 < len(changes); i += 2 {
		got[changes[i].(string)] = changes[i+1].(string)
	}
	if got["roomCreateLimit"] != "2 (was 5)" || got["roomListMinIntervalMs"] != "0 (was 250)" || got["abuseBanStrikes"] != "0 (was 40)" {
		t.Fatalf("changes logged as %v", got)
	}
}

func TestRateLimitSettingsRejected(t *testing.T) {
	for _, data := range []string{
		`{"roomCreateLimit": -1}`,
		`{"roomJoinLimit": 1001}`,
		`{"matchStartWindowSeconds": 0}`,
		`{"spectateWindowSeconds": 3601}`,
		`{"abuseBanStrikes": -1}`,
		`{"abuseBanStrikes": 1001}`,
		`{"abuseBanWindowSeconds": 0}`,
		`{"abuseBanSeconds": 0}`,
		`{"abuseBanSeconds": 2592001}`,
		`{"abuseReportDays": 0}`,
		`{"abuseReportDays": 366}`,
		`{"roomListMinIntervalMs": -1}`,
		`{"roomListMinIntervalMs": 5001}`,
	} {
		if _, _, err := Parse([]byte(data)); err == nil {
			t.Errorf("Parse(%s) accepted it", data)
		}
	}
}

func TestAdminLoginAndLogSizeSettings(t *testing.T) {
	d := Defaults()
	if n, ban := d.AdminLoginPolicy(); n != 5 || ban != 0 {
		t.Fatalf("default login policy = %d, %v", n, ban)
	}
	if d.ModLogMaxBytes() != 1024<<20 {
		t.Fatalf("default modLogMaxMB = %d bytes", d.ModLogMaxBytes())
	}
	s, unknown, err := Parse([]byte(`{"adminLoginMaxFailures": 10, "adminLoginBanSeconds": 3600, "modLogMaxMB": 0}`))
	if err != nil || len(unknown) != 0 {
		t.Fatalf("Parse: %v %v", err, unknown)
	}
	if n, ban := s.AdminLoginPolicy(); n != 10 || ban != time.Hour || s.ModLogMaxBytes() != 0 {
		t.Fatalf("parsed = %d %v %d", n, ban, s.ModLogMaxBytes())
	}
	for _, data := range []string{
		`{"adminLoginMaxFailures": 0}`,
		`{"adminLoginMaxFailures": 1001}`,
		`{"adminLoginBanSeconds": -1}`,
		`{"modLogMaxMB": -5}`,
		`{"modLogMaxMB": "1024"}`,
	} {
		if _, _, err := Parse([]byte(data)); err == nil {
			t.Errorf("Parse(%s) succeeded, want error", data)
		}
	}
}
