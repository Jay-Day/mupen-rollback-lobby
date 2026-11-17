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
	"net/netip"
	"os"
	"sort"
	"strings"
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

	// Per-IP limits on the lobby requests that reach every connected player
	// (or cost the server bulk data): one IP may make each kind *Limit times
	// per *WindowSeconds; 0 lifts that limit. See README "Rate limits".
	RoomCreateLimit          int `json:"roomCreateLimit"`
	RoomCreateWindowSeconds  int `json:"roomCreateWindowSeconds"`
	MatchStartLimit          int `json:"matchStartLimit"`
	MatchStartWindowSeconds  int `json:"matchStartWindowSeconds"`
	RoomJoinLimit            int `json:"roomJoinLimit"`
	RoomJoinWindowSeconds    int `json:"roomJoinWindowSeconds"`
	SpectateLimit            int `json:"spectateLimit"`
	SpectateWindowSeconds    int `json:"spectateWindowSeconds"`
	RoomUpdateLimit          int `json:"roomUpdateLimit"`
	RoomUpdateWindowSeconds  int `json:"roomUpdateWindowSeconds"`
	LobbyUpdateLimit         int `json:"lobbyUpdateLimit"`
	LobbyUpdateWindowSeconds int `json:"lobbyUpdateWindowSeconds"`

	// Every refused request is a strike: AbuseBanStrikes of them within
	// AbuseBanWindowSeconds ban the IP for AbuseBanSeconds (0 strikes turns
	// the automatic ban off). The moderators' Rate limits report keeps an IP
	// for AbuseReportDays after its last refused request.
	AbuseBanStrikes       int `json:"abuseBanStrikes"`
	AbuseBanWindowSeconds int `json:"abuseBanWindowSeconds"`
	AbuseBanSeconds       int `json:"abuseBanSeconds"`
	AbuseReportDays       int `json:"abuseReportDays"`

	// The full room list goes to every player at most once per this many
	// milliseconds; changes in between go out together, once, at the end of
	// the interval. 0 sends a list for every change.
	RoomListMinIntervalMs int `json:"roomListMinIntervalMs"`

	// How many days the moderators' log keeps chat, connections (with IPs)
	// and games; 0 keeps them forever. Older rows are pruned every few hours.
	ChatLogDays       int `json:"chatLogDays"`
	ConnectionLogDays int `json:"connectionLogDays"`
	GameLogDays       int `json:"gameLogDays"`

	// Addresses (IPs and CIDR ranges) that may reach the moderator website.
	// Empty: no restriction beyond where the site listens (--admin-addr).
	// Loopback is always allowed.
	AdminAllowedIPs []string `json:"adminAllowedIPs"`

	// AdminLoginMaxFailures wrong moderator passwords in a row from one IP
	// (the site and the desktop client's /login together) ban it for
	// AdminLoginBanSeconds; 0 seconds is permanent. Loopback is locked out
	// for 15 minutes instead of banned.
	AdminLoginMaxFailures int `json:"adminLoginMaxFailures"`
	AdminLoginBanSeconds  int `json:"adminLoginBanSeconds"`

	// The moderators' log may hold at most this many megabytes of data; past
	// it the oldest chat and connections are pruned first (the audit log
	// never). 0: no cap, only the retention days.
	ModLogMaxMB int `json:"modLogMaxMB"`

	minClient    ReleaseVersion
	adminAllowed []netip.Prefix
	actionLimits ratelimit.ActionLimits // built from the keys above; read only
}

// Bounds for the request limits.
const (
	MaxActionLimit           = 1000 // the limiter keeps at most this many requests per window
	MaxActionWindowSeconds   = 3600
	MaxAbuseBanStrikes       = 1000
	MaxAbuseBanWindowSeconds = 24 * 3600
	MaxAbuseBanSeconds       = 30 * 24 * 3600
	MaxAbuseReportDays       = 365
	MaxRoomListMinIntervalMs = 5000
)

// Bounds for the keyframe interval. Clients clamp to the same range.
const (
	MinLiveReplayKeyframeSeconds = 10
	MaxLiveReplayKeyframeSeconds = 600
)

// MaxLogDays bounds the log retention settings (about 10 years).
const MaxLogDays = 3650

// maxAdminAllowedIPs bounds adminAllowedIPs, which is checked on every request.
const maxAdminAllowedIPs = 256

// Bounds for the moderator login guard and the log's size cap.
const (
	MaxAdminLoginMaxFailures = 1000
	MaxAdminLoginBanSeconds  = 10 * 365 * 24 * 3600 // about 10 years; 0 is permanent
	MaxModLogMaxMB           = 1 << 20              // 1 TiB
)

// Defaults returns the settings used when the file is missing.
func Defaults() *Settings {
	limits := ratelimit.DefaultLimits()
	actions := ratelimit.DefaultActionLimits()
	count := func(k ratelimit.ActionKind) int { return actions.Rates[k].Count }
	window := func(k ratelimit.ActionKind) int { return int(actions.Rates[k].Window / time.Second) }
	s := &Settings{
		MinClientVersion:          "0.9.13b",
		LiveReplayKeyframeSeconds: 30,
		QuickMatchWarmupSeconds:   5,
		ConnectAttempts:           limits.Attempts,
		ConnectWindowSeconds:      int(limits.Window / time.Second),
		ConnectBanSeconds:         int(limits.BanDuration / time.Second),
		MaxConnectionsPerIP:       limits.MaxConcurrent,
		RoomCreateLimit:           count(ratelimit.ActionRoomCreate),
		RoomCreateWindowSeconds:   window(ratelimit.ActionRoomCreate),
		MatchStartLimit:           count(ratelimit.ActionMatchStart),
		MatchStartWindowSeconds:   window(ratelimit.ActionMatchStart),
		RoomJoinLimit:             count(ratelimit.ActionRoomJoin),
		RoomJoinWindowSeconds:     window(ratelimit.ActionRoomJoin),
		SpectateLimit:             count(ratelimit.ActionSpectate),
		SpectateWindowSeconds:     window(ratelimit.ActionSpectate),
		RoomUpdateLimit:           count(ratelimit.ActionRoomUpdate),
		RoomUpdateWindowSeconds:   window(ratelimit.ActionRoomUpdate),
		LobbyUpdateLimit:          count(ratelimit.ActionLobbyUpdate),
		LobbyUpdateWindowSeconds:  window(ratelimit.ActionLobbyUpdate),
		AbuseBanStrikes:           actions.BanStrikes,
		AbuseBanWindowSeconds:     int(actions.BanWindow / time.Second),
		AbuseBanSeconds:           int(actions.BanDuration / time.Second),
		AbuseReportDays:           int(actions.ReportTTL / (24 * time.Hour)),
		RoomListMinIntervalMs:     250,
		ChatLogDays:               90,
		ConnectionLogDays:         90,
		GameLogDays:               365,
		AdminAllowedIPs:           []string{},
		AdminLoginMaxFailures:     5,
		AdminLoginBanSeconds:      0,
		ModLogMaxMB:               1024,
	}
	s.minClient, _ = ParseReleaseVersion(s.MinClientVersion)
	s.actionLimits = s.buildActionLimits()
	return s
}

// buildActionLimits turns the request-limit keys into the limiter's form.
func (s *Settings) buildActionLimits() ratelimit.ActionLimits {
	rate := func(count, windowSeconds int) ratelimit.Rate {
		return ratelimit.Rate{Count: count, Window: time.Duration(windowSeconds) * time.Second}
	}
	return ratelimit.ActionLimits{
		Rates: map[ratelimit.ActionKind]ratelimit.Rate{
			ratelimit.ActionRoomCreate:  rate(s.RoomCreateLimit, s.RoomCreateWindowSeconds),
			ratelimit.ActionMatchStart:  rate(s.MatchStartLimit, s.MatchStartWindowSeconds),
			ratelimit.ActionRoomJoin:    rate(s.RoomJoinLimit, s.RoomJoinWindowSeconds),
			ratelimit.ActionSpectate:    rate(s.SpectateLimit, s.SpectateWindowSeconds),
			ratelimit.ActionRoomUpdate:  rate(s.RoomUpdateLimit, s.RoomUpdateWindowSeconds),
			ratelimit.ActionLobbyUpdate: rate(s.LobbyUpdateLimit, s.LobbyUpdateWindowSeconds),
		},
		BanStrikes:  s.AbuseBanStrikes,
		BanWindow:   time.Duration(s.AbuseBanWindowSeconds) * time.Second,
		BanDuration: time.Duration(s.AbuseBanSeconds) * time.Second,
		ReportTTL:   time.Duration(s.AbuseReportDays) * 24 * time.Hour,
		MaxIPs:      ratelimit.DefaultActionMaxIPs,
	}
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
	for _, f := range []struct {
		name     string
		value    int
		min, max int
		unit     string
	}{
		{"roomCreateLimit", s.RoomCreateLimit, 0, MaxActionLimit, " (0 lifts the limit)"},
		{"matchStartLimit", s.MatchStartLimit, 0, MaxActionLimit, " (0 lifts the limit)"},
		{"roomJoinLimit", s.RoomJoinLimit, 0, MaxActionLimit, " (0 lifts the limit)"},
		{"spectateLimit", s.SpectateLimit, 0, MaxActionLimit, " (0 lifts the limit)"},
		{"roomUpdateLimit", s.RoomUpdateLimit, 0, MaxActionLimit, " (0 lifts the limit)"},
		{"lobbyUpdateLimit", s.LobbyUpdateLimit, 0, MaxActionLimit, " (0 lifts the limit)"},
		{"roomCreateWindowSeconds", s.RoomCreateWindowSeconds, 1, MaxActionWindowSeconds, ""},
		{"matchStartWindowSeconds", s.MatchStartWindowSeconds, 1, MaxActionWindowSeconds, ""},
		{"roomJoinWindowSeconds", s.RoomJoinWindowSeconds, 1, MaxActionWindowSeconds, ""},
		{"spectateWindowSeconds", s.SpectateWindowSeconds, 1, MaxActionWindowSeconds, ""},
		{"roomUpdateWindowSeconds", s.RoomUpdateWindowSeconds, 1, MaxActionWindowSeconds, ""},
		{"lobbyUpdateWindowSeconds", s.LobbyUpdateWindowSeconds, 1, MaxActionWindowSeconds, ""},
		{"abuseBanStrikes", s.AbuseBanStrikes, 0, MaxAbuseBanStrikes, " (0 turns the automatic ban off)"},
		{"abuseBanWindowSeconds", s.AbuseBanWindowSeconds, 1, MaxAbuseBanWindowSeconds, ""},
		// at least 1: a ban without a duration would be permanent
		{"abuseBanSeconds", s.AbuseBanSeconds, 1, MaxAbuseBanSeconds, ""},
		{"abuseReportDays", s.AbuseReportDays, 1, MaxAbuseReportDays, ""},
		{"roomListMinIntervalMs", s.RoomListMinIntervalMs, 0, MaxRoomListMinIntervalMs, " (0 sends every change at once)"},
	} {
		if f.value < f.min || f.value > f.max {
			return fmt.Errorf("%s must be %d-%d%s, got %d", f.name, f.min, f.max, f.unit, f.value)
		}
	}
	s.actionLimits = s.buildActionLimits()
	for _, f := range []struct {
		name  string
		value int
	}{
		{"chatLogDays", s.ChatLogDays},
		{"connectionLogDays", s.ConnectionLogDays},
		{"gameLogDays", s.GameLogDays},
	} {
		if f.value < 0 || f.value > MaxLogDays {
			return fmt.Errorf("%s must be 0 (keep forever) to %d, got %d", f.name, MaxLogDays, f.value)
		}
	}
	for _, f := range []struct {
		name     string
		value    int
		min, max int
		unit     string
	}{
		{"adminLoginMaxFailures", s.AdminLoginMaxFailures, 1, MaxAdminLoginMaxFailures, ""},
		{"adminLoginBanSeconds", s.AdminLoginBanSeconds, 0, MaxAdminLoginBanSeconds, " (0 bans for good)"},
		{"modLogMaxMB", s.ModLogMaxMB, 0, MaxModLogMaxMB, " (0 sets no cap)"},
	} {
		if f.value < f.min || f.value > f.max {
			return fmt.Errorf("%s must be %d-%d%s, got %d", f.name, f.min, f.max, f.unit, f.value)
		}
	}
	if s.AdminAllowedIPs == nil { // "adminAllowedIPs": null
		s.AdminAllowedIPs = []string{}
	}
	if len(s.AdminAllowedIPs) > maxAdminAllowedIPs {
		return fmt.Errorf("adminAllowedIPs may list at most %d entries, got %d", maxAdminAllowedIPs, len(s.AdminAllowedIPs))
	}
	s.adminAllowed = make([]netip.Prefix, 0, len(s.AdminAllowedIPs))
	for _, entry := range s.AdminAllowedIPs {
		p, err := parseIPOrPrefix(entry)
		if err != nil {
			return fmt.Errorf("adminAllowedIPs: %q is not an IP address or CIDR range like 203.0.113.0/24", entry)
		}
		s.adminAllowed = append(s.adminAllowed, p)
	}
	return nil
}

// parseIPOrPrefix reads "203.0.113.7", "2001:db8::1" or "203.0.113.0/24".
func parseIPOrPrefix(s string) (netip.Prefix, error) {
	s = strings.TrimSpace(s)
	if strings.Contains(s, "/") {
		p, err := netip.ParsePrefix(s)
		if err != nil {
			return netip.Prefix{}, err
		}
		if p.Addr().Is4In6() {
			return netip.Prefix{}, errors.New("IPv4-mapped prefix")
		}
		return p.Masked(), nil
	}
	a, err := netip.ParseAddr(s)
	if err != nil {
		return netip.Prefix{}, err
	}
	if a.Zone() != "" {
		return netip.Prefix{}, errors.New("zoned address")
	}
	a = a.Unmap()
	return netip.PrefixFrom(a, a.BitLen()), nil
}

// AdminLoginPolicy is adminLoginMaxFailures and adminLoginBanSeconds: how
// many failures ban an IP, and for how long (0: permanent).
func (s *Settings) AdminLoginPolicy() (maxFailures int, ban time.Duration) {
	return s.AdminLoginMaxFailures, time.Duration(s.AdminLoginBanSeconds) * time.Second
}

// ModLogMaxBytes is modLogMaxMB in bytes (0: no cap).
func (s *Settings) ModLogMaxBytes() int64 { return int64(s.ModLogMaxMB) << 20 }

// AdminAllowed is adminAllowedIPs, parsed (empty: no restriction).
func (s *Settings) AdminAllowed() []netip.Prefix { return s.adminAllowed }

// AdminIPAllowed reports whether ip may reach the moderator website: always
// for loopback, otherwise when adminAllowedIPs is empty or lists it.
func (s *Settings) AdminIPAllowed(ip string) bool {
	a, err := netip.ParseAddr(ip)
	if err != nil {
		return false
	}
	a = a.Unmap().WithZone("")
	if a.IsLoopback() || len(s.adminAllowed) == 0 {
		return true
	}
	for _, p := range s.adminAllowed {
		if p.Contains(a) {
			return true
		}
	}
	return false
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

// ActionLimits are the per-IP request limits, for ratelimit.ActionLimiter.
// The same value (and map) comes back on every call; treat it as read only.
func (s *Settings) ActionLimits() ratelimit.ActionLimits { return s.actionLimits }

// RoomListMinInterval is roomListMinIntervalMs (0: every change goes out).
func (s *Settings) RoomListMinInterval() time.Duration {
	return time.Duration(s.RoomListMinIntervalMs) * time.Millisecond
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
		if was, now := logValue(am[k]), logValue(bm[k]); was != now {
			out = append(out, k, now+" (was "+was+")")
		}
	}
	return out
}

// logValue renders one setting for the reload log line: lists as JSON
// (["203.0.113.7","10.0.0.0/8"], [] when empty), everything else as is.
func logValue(v any) string {
	switch v.(type) {
	case []any, map[string]any:
		b, _ := json.Marshal(v)
		return string(b)
	}
	return fmt.Sprint(v)
}
