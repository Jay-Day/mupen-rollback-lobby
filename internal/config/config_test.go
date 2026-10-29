package config

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
		`{"minClientVersion": "0.9.15",}`,
		`[]`,
	} {
		if _, _, err := Parse([]byte(data)); err == nil {
			t.Errorf("Parse(%s) succeeded, want error", data)
		}
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
