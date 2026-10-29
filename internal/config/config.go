// Package config holds the lobby settings an operator can change while the
// server runs. They live in a JSON file (RMGK_CONFIG_PATH, default lobby.json)
// that the server re-reads whenever it changes, so raising the minimum client
// version or retuning live replays needs neither a rebuild nor a restart.
// Keys left out of the file keep their defaults.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"sync/atomic"
	"time"

	"github.com/jay-day/rmgk-lobby/internal/ratelimit"
)

// Settings is one complete set of lobby options. Treat a *Settings as
// read-only: a reload swaps in a new value instead of changing the old one.
type Settings struct {
	// Oldest RMG-K release allowed to log in, e.g. "0.9.13b". Development
	// builds (vdev-*) are always allowed.
	MinClientVersion string `json:"minClientVersion"`
	// Shown to clients older than MinClientVersion. Empty uses a message that
	// names the required version.
	OutdatedClientMessage string `json:"outdatedClientMessage"`

	// How often a live replay broadcaster uploads a savestate keyframe. Late
	// viewers start from the latest one, so a shorter interval means less
	// fast-forward for them and more upload for the broadcaster. Clients get
	// it in MATCH_BEGIN, so a change applies from the next match.
	LiveReplayKeyframeSeconds int `json:"liveReplayKeyframeSeconds"`

	// Time between a quick match pairing and its auto-start, for both
	// clients to measure ping and publish their rollback settings.
	QuickMatchWarmupSeconds int `json:"quickMatchWarmupSeconds"`

	// Per-IP connect limits: more than ConnectAttempts within
	// ConnectWindowSeconds bans the IP for ConnectBanSeconds, and one IP may
	// hold at most MaxConnectionsPerIP connections at once.
	ConnectAttempts      int `json:"connectAttempts"`
	ConnectWindowSeconds int `json:"connectWindowSeconds"`
	ConnectBanSeconds    int `json:"connectBanSeconds"`
	MaxConnectionsPerIP  int `json:"maxConnectionsPerIP"`

	minClient ReleaseVersion
}

// Bounds for the keyframe interval. Clients clamp to the same range.
const (
	MinLiveReplayKeyframeSeconds = 10
	MaxLiveReplayKeyframeSeconds = 600
)

// Defaults returns the settings used when the file is missing.
func Defaults() *Settings {
	limits := ratelimit.DefaultLimits()
	s := &Settings{
		MinClientVersion:          "0.9.13b",
		LiveReplayKeyframeSeconds: 30,
		QuickMatchWarmupSeconds:   5,
		ConnectAttempts:           limits.Attempts,
		ConnectWindowSeconds:      int(limits.Window / time.Second),
		ConnectBanSeconds:         int(limits.BanDuration / time.Second),
		MaxConnectionsPerIP:       limits.MaxConcurrent,
	}
	s.minClient, _ = ParseReleaseVersion(s.MinClientVersion)
	return s
}

var defaults = Defaults()

// Parse reads settings from JSON on top of the defaults. Unknown keys don't
// fail the load (an older server can read a newer file) but are returned so
// the caller can log them, since they're usually typos.
func Parse(data []byte) (*Settings, []string, error) {
	s := Defaults()
	if len(bytes.TrimSpace(data)) > 0 {
		if err := json.Unmarshal(data, s); err != nil {
			return nil, nil, err
		}
	}
	if err := s.validate(); err != nil {
		return nil, nil, err
	}
	unknown, err := unknownKeys(data)
	if err != nil {
		return nil, nil, err
	}
	return s, unknown, nil
}

func (s *Settings) validate() error {
	v, ok := ParseReleaseVersion(s.MinClientVersion)
	if !ok {
		return fmt.Errorf("minClientVersion %q is not a release version like 0.9.13b", s.MinClientVersion)
	}
	s.minClient = v
	if s.LiveReplayKeyframeSeconds < MinLiveReplayKeyframeSeconds ||
		s.LiveReplayKeyframeSeconds > MaxLiveReplayKeyframeSeconds {
		return fmt.Errorf("liveReplayKeyframeSeconds must be %d-%d, got %d",
			MinLiveReplayKeyframeSeconds, MaxLiveReplayKeyframeSeconds, s.LiveReplayKeyframeSeconds)
	}
	if s.QuickMatchWarmupSeconds < 2 || s.QuickMatchWarmupSeconds > 60 {
		return fmt.Errorf("quickMatchWarmupSeconds must be 2-60, got %d", s.QuickMatchWarmupSeconds)
	}
	for _, f := range []struct {
		name  string
		value int
	}{
		{"connectAttempts", s.ConnectAttempts},
		{"connectWindowSeconds", s.ConnectWindowSeconds},
		{"connectBanSeconds", s.ConnectBanSeconds},
		{"maxConnectionsPerIP", s.MaxConnectionsPerIP},
	} {
		if f.value < 1 {
			return fmt.Errorf("%s must be at least 1, got %d", f.name, f.value)
		}
	}
	return nil
}

// unknownKeys lists the top-level keys in data that Settings doesn't have.
func unknownKeys(data []byte) ([]string, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, nil
	}
	var given, known map[string]json.RawMessage
	if err := json.Unmarshal(data, &given); err != nil {
		return nil, err
	}
	b, _ := json.Marshal(defaults)
	_ = json.Unmarshal(b, &known)
	var unknown []string
	for k := range given {
		if _, ok := known[k]; !ok {
			unknown = append(unknown, k)
		}
	}
	sort.Strings(unknown)
	return unknown, nil
}

// MinClient is MinClientVersion, parsed.
func (s *Settings) MinClient() ReleaseVersion { return s.minClient }

// OutdatedMessage is the HELLO_FAIL text for clients below MinClientVersion.
func (s *Settings) OutdatedMessage() string {
	if s.OutdatedClientMessage != "" {
		return s.OutdatedClientMessage
	}
	return fmt.Sprintf("Your RMG-K client is out of date. This lobby requires RMG-K %s or any vdev build.",
		s.MinClientVersion)
}

func (s *Settings) QuickMatchWarmup() time.Duration {
	return time.Duration(s.QuickMatchWarmupSeconds) * time.Second
}

func (s *Settings) ConnectLimits() ratelimit.Limits {
	return ratelimit.Limits{
		Attempts:      s.ConnectAttempts,
		Window:        time.Duration(s.ConnectWindowSeconds) * time.Second,
		BanDuration:   time.Duration(s.ConnectBanSeconds) * time.Second,
		MaxConcurrent: s.MaxConnectionsPerIP,
	}
}

// Store holds the current settings and reloads them from their file.
type Store struct {
	path string
	cur  atomic.Pointer[Settings]
}

// Open loads the settings file. A missing file gives the defaults; an
// unreadable or invalid one is an error, so a bad edit stops the server at
// startup instead of running it on settings nobody chose.
func Open(path string) (*Store, error) {
	s, unknown, err := load(path)
	if err != nil {
		return nil, err
	}
	logUnknown(path, unknown)
	st := &Store{path: path}
	st.cur.Store(s)
	return st, nil
}

// Get returns the current settings. A nil Store returns the defaults, so code
// and tests that never configure one keep the built-in behaviour.
func (st *Store) Get() *Settings {
	if st == nil {
		return defaults
	}
	return st.cur.Load()
}

// Reload re-reads the file. An invalid file is logged and the previous
// settings stay in effect; a deleted one restores the defaults.
func (st *Store) Reload() {
	s, unknown, err := load(st.path)
	if err != nil {
		slog.Error("lobby config reload failed; keeping previous settings", "path", st.path, "err", err)
		return
	}
	logUnknown(st.path, unknown)
	old := st.cur.Swap(s)
	changes := diff(old, s)
	if len(changes) == 0 {
		slog.Info("lobby config reloaded; no changes", "path", st.path)
		return
	}
	slog.Info("lobby config reloaded", append([]any{"path", st.path}, changes...)...)
}

func load(path string) (*Settings, []string, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Defaults(), nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	return Parse(data)
}

func logUnknown(path string, unknown []string) {
	if len(unknown) > 0 {
		slog.Warn("lobby config has unknown keys; ignoring them", "path", path, "keys", unknown)
	}
}

// diff returns slog key/value pairs, "key", "new (was old)", for each setting
// that differs between a and b.
func diff(a, b *Settings) []any {
	var am, bm map[string]any
	ab, _ := json.Marshal(a)
	bb, _ := json.Marshal(b)
	_ = json.Unmarshal(ab, &am)
	_ = json.Unmarshal(bb, &bm)
	keys := make([]string, 0, len(bm))
	for k := range bm {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var out []any
	for _, k := range keys {
		if fmt.Sprint(am[k]) != fmt.Sprint(bm[k]) {
			out = append(out, k, fmt.Sprintf("%v (was %v)", bm[k], am[k]))
		}
	}
	return out
}
