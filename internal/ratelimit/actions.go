package ratelimit

import (
	"context"
	"sort"
	"sync"
	"time"
)

// ActionKind names one per-IP budget of lobby requests. Each kind has its own
// sliding window, so one IP running out of room creations can still join rooms.
type ActionKind string

// The budgets the lobby uses. A kind with no Rate in ActionLimits.Rates is
// unlimited, so the set can grow without touching this package.
const (
	ActionRoomCreate  ActionKind = "room_create"  // ROOM_CREATE
	ActionMatchStart  ActionKind = "match_start"  // ROOM_START, MATCH_ADMIT
	ActionRoomJoin    ActionKind = "room_join"    // ROOM_JOIN, QUICK_MATCH_JOIN
	ActionSpectate    ActionKind = "spectate"     // SPECTATE_START
	ActionRoomUpdate  ActionKind = "room_update"  // room settings and seat swaps (room-scoped)
	ActionLobbyUpdate ActionKind = "lobby_update" // other lobby-wide fan-out: away toggles, kicks, ...
)

// Rate allows Count requests per sliding Window. Count or Window <= 0 makes
// the kind unlimited.
type Rate struct {
	Count  int
	Window time.Duration
}

// ActionLimits are the thresholds an ActionLimiter enforces.
type ActionLimits struct {
	// Per-kind budgets. Read only: the limiter never modifies the map, so a
	// caller may hand out the same map on every call.
	Rates map[ActionKind]Rate

	// Every refused request is a strike. BanStrikes strikes within BanWindow
	// make Allow report a ban once; 0 disables the automatic ban. BanDuration
	// is how long the caller should ban for; the limiter only uses it to avoid
	// reporting the same IP twice while that ban runs.
	BanStrikes  int
	BanWindow   time.Duration
	BanDuration time.Duration

	// The moderator report keeps an IP's record for ReportTTL after its last
	// refused request, and tracks at most MaxIPs addresses in all.
	ReportTTL time.Duration
	MaxIPs    int
}

// Defaults, chosen so a real client never trips them: see the audit notes in
// the hub's wiring for what each kind covers and why.
const (
	DefaultActionBanStrikes  = 40
	DefaultActionBanWindow   = 10 * time.Minute
	DefaultActionBanDuration = time.Hour
	DefaultActionReportTTL   = 7 * 24 * time.Hour
	DefaultActionMaxIPs      = 5000

	// maxTrackedEvents caps Rate.Count and BanStrikes, which bound the
	// timestamps kept per IP. Larger values are treated as this.
	maxTrackedEvents = 1000
	// maxUsernames is how many distinct recent usernames a record keeps.
	maxUsernames = 8
)

var defaultRates = map[ActionKind]Rate{
	ActionRoomCreate:  {Count: 5, Window: time.Minute},
	ActionMatchStart:  {Count: 10, Window: time.Minute},
	ActionRoomJoin:    {Count: 15, Window: time.Minute},
	ActionSpectate:    {Count: 10, Window: time.Minute},
	ActionRoomUpdate:  {Count: 40, Window: 10 * time.Second},
	ActionLobbyUpdate: {Count: 30, Window: time.Minute},
}

// DefaultActionLimits returns the built-in thresholds. Each call returns a
// fresh copy of the rates.
func DefaultActionLimits() ActionLimits {
	rates := make(map[ActionKind]Rate, len(defaultRates))
	for k, r := range defaultRates {
		rates[k] = r
	}
	return ActionLimits{
		Rates:       rates,
		BanStrikes:  DefaultActionBanStrikes,
		BanWindow:   DefaultActionBanWindow,
		BanDuration: DefaultActionBanDuration,
		ReportTTL:   DefaultActionReportTTL,
		MaxIPs:      DefaultActionMaxIPs,
	}
}

var builtinActionLimits = DefaultActionLimits()

// ActionRecord is one IP's entry in the moderator report: an IP gets one the
// first time a request from it is refused. It is plain data, so it can be
// stored as JSON and loaded back with Import. Zero times mean "never".
type ActionRecord struct {
	IP        string    `json:"ip"`
	FirstTrip time.Time `json:"firstTrip"`
	LastTrip  time.Time `json:"lastTrip"`
	// Refused requests per kind since the record was created.
	Refused      map[ActionKind]int `json:"refused"`
	TotalRefused int                `json:"totalRefused"`
	// Allowed requests per kind still inside that kind's window when the
	// snapshot was taken. Informational: Import ignores it.
	Allowed map[ActionKind]int `json:"allowed,omitempty"`
	// Distinct usernames seen from this IP, most recent first.
	Usernames []string `json:"usernames,omitempty"`
	// Automatic bans reported for this IP, and the latest one.
	AutoBans     int       `json:"autoBans,omitempty"`
	AutoBannedAt time.Time `json:"autoBannedAt"`
	BannedUntil  time.Time `json:"bannedUntil"`
}

// ActionLimiter enforces per-IP budgets on lobby requests that are expensive
// for the server, usually because they fan out to every connected client. It
// is keyed by IP, so the budget is shared by all of an IP's connections and
// survives reconnects. It is safe for concurrent use.
type ActionLimiter struct {
	mu             sync.Mutex
	limits         func() ActionLimits // nil = DefaultActionLimits
	now            func() time.Time
	exemptLoopback bool
	ips            map[string]*actionEntry
}

type actionEntry struct {
	windows   map[ActionKind][]time.Time // allowed requests, oldest first, at most Rate.Count
	strikes   []time.Time                // refused requests inside BanWindow, oldest first
	lastSeen  time.Time
	usernames []string // distinct, most recent first, at most maxUsernames
	rec       *actionReport
}

// actionReport is the persistent part of an entry, created on the first trip.
type actionReport struct {
	firstTrip, lastTrip time.Time
	refused             map[ActionKind]int
	totalRefused        int
	autoBans            int
	autoBannedAt        time.Time
	bannedUntil         time.Time
}

// NewActionLimiter returns a limiter using the default limits. Loopback
// addresses are exempt, like the connect gate, so local multi-instance testing
// is never throttled; SetExemptLoopback(false) turns that off.
func NewActionLimiter() *ActionLimiter {
	return &ActionLimiter{
		now:            time.Now,
		exemptLoopback: true,
		ips:            make(map[string]*actionEntry),
	}
}

// SetLimits makes the limiter read its thresholds from f on every check, so
// they can change while the server runs. f must not call back into the
// limiter, and should return a precomputed value: it runs on every Allow.
func (l *ActionLimiter) SetLimits(f func() ActionLimits) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.limits = f
}

// SetExemptLoopback controls whether loopback addresses bypass the limiter.
func (l *ActionLimiter) SetExemptLoopback(exempt bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.exemptLoopback = exempt
}

// SetClock replaces the time source (for tests).
func (l *ActionLimiter) SetClock(now func() time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.now = now
}

// currentLimits must be called with mu held.
func (l *ActionLimiter) currentLimits() ActionLimits {
	if l.limits == nil {
		return builtinActionLimits
	}
	return l.limits()
}

// Allow records a request of the given kind from ip and reports whether it may
// proceed. When it may not, retryAfter is how long until the oldest request in
// the window expires, and the refusal counts as a strike. ban is true exactly
// once when the IP collects BanStrikes strikes within BanWindow: the caller
// should then ban the IP for BanDuration and close its connections. username
// is only recorded for the moderator report ("" is ignored).
func (l *ActionLimiter) Allow(ip string, kind ActionKind, username string) (ok bool, retryAfter time.Duration, ban bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.exemptLoopback && isLoopback(ip) {
		return true, 0, false
	}
	limits := l.currentLimits()
	rate, limited := limits.Rates[kind]
	if !limited || rate.Count <= 0 || rate.Window <= 0 {
		return true, 0, false
	}
	count := min(rate.Count, maxTrackedEvents)
	now := l.now()

	e := l.entryFor(ip, now, limits)
	e.lastSeen = now
	e.noteUsername(username)

	ts := pruneWindow(e.windows[kind], now, rate.Window)
	if len(ts) > count {
		// The limit was lowered: only the newest requests still matter.
		ts = append(ts[:0], ts[len(ts)-count:]...)
	}
	if len(ts) < count {
		e.windows[kind] = append(ts, now)
		return true, 0, false
	}
	e.windows[kind] = ts
	// Full window: a slot frees up when its oldest request leaves.
	retryAfter = ts[0].Add(rate.Window).Sub(now)

	r := e.report(now)
	r.lastTrip = now
	r.refused[kind]++
	r.totalRefused++
	return false, retryAfter, l.strike(e, now, limits)
}

// strike records one refused request and reports whether it completes an
// automatic ban. Called with mu held.
func (l *ActionLimiter) strike(e *actionEntry, now time.Time, limits ActionLimits) bool {
	strikes := min(limits.BanStrikes, maxTrackedEvents)
	if strikes <= 0 || limits.BanWindow <= 0 {
		e.strikes = nil
		return false
	}
	// Already reported: the caller's ban is running, and requests still in
	// flight from the closing connections must not report it again.
	if now.Before(e.rec.bannedUntil) {
		return false
	}
	e.strikes = append(pruneWindow(e.strikes, now, limits.BanWindow), now)
	if len(e.strikes) < strikes {
		return false
	}
	e.strikes = e.strikes[:0]
	e.rec.autoBans++
	e.rec.autoBannedAt = now
	e.rec.bannedUntil = now.Add(limits.BanDuration)
	return true
}

// ClearBan forgets that an automatic ban was reported for ip, and its strikes,
// so a moderator's early unban doesn't stop the next one from being reported.
// The report's history (counts, AutoBans, AutoBannedAt) is kept.
func (l *ActionLimiter) ClearBan(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	e := l.ips[ip]
	if e == nil {
		return
	}
	e.strikes = nil
	if e.rec != nil {
		e.rec.bannedUntil = time.Time{}
	}
}

// Forget drops everything known about ip: its windows, strikes and record.
// It reports whether ip had a record in the report.
func (l *ActionLimiter) Forget(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	e := l.ips[ip]
	delete(l.ips, ip)
	return e != nil && e.rec != nil
}

// Limits returns the thresholds in effect now. Treat the Rates map as read
// only.
func (l *ActionLimiter) Limits() ActionLimits {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.currentLimits()
}

// ExemptsLoopback reports whether loopback addresses bypass the limiter.
func (l *ActionLimiter) ExemptsLoopback() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.exemptLoopback
}

// Snapshot returns the moderator report: one record per IP that has had a
// request refused, most recently refused first. It doubles as the export for
// persisting the report across restarts (see Import).
func (l *ActionLimiter) Snapshot() []ActionRecord {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	rates := l.currentLimits().Rates
	out := make([]ActionRecord, 0)
	for ip, e := range l.ips {
		if e.rec == nil {
			continue
		}
		r := e.rec
		rec := ActionRecord{
			IP:           ip,
			FirstTrip:    r.firstTrip,
			LastTrip:     r.lastTrip,
			Refused:      make(map[ActionKind]int, len(r.refused)),
			TotalRefused: r.totalRefused,
			Usernames:    append([]string(nil), e.usernames...),
			AutoBans:     r.autoBans,
			AutoBannedAt: r.autoBannedAt,
			BannedUntil:  r.bannedUntil,
		}
		for k, n := range r.refused {
			rec.Refused[k] = n
		}
		for k, ts := range e.windows {
			rate, ok := rates[k]
			if !ok || rate.Window <= 0 {
				continue
			}
			if n := countWithin(ts, now, rate.Window); n > 0 {
				if rec.Allowed == nil {
					rec.Allowed = make(map[ActionKind]int)
				}
				rec.Allowed[k] = n
			}
		}
		out = append(out, rec)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].LastTrip.Equal(out[j].LastTrip) {
			return out[i].LastTrip.After(out[j].LastTrip)
		}
		return out[i].IP < out[j].IP
	})
	return out
}

// Import loads records saved from Snapshot, e.g. after a restart. Records past
// ReportTTL are skipped, and an IP the limiter already reports on keeps its
// live record. Windows and strikes are not part of a record: a restart gives
// every IP fresh budgets, as it does for the connect gate.
func (l *ActionLimiter) Import(records []ActionRecord) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	limits := l.currentLimits()
	for _, rec := range records {
		if rec.IP == "" || rec.LastTrip.IsZero() || l.reportExpired(rec.LastTrip, rec.BannedUntil, now, limits) {
			continue
		}
		e := l.entryFor(rec.IP, now, limits)
		if e.rec != nil {
			continue
		}
		r := &actionReport{
			firstTrip:    rec.FirstTrip,
			lastTrip:     rec.LastTrip,
			refused:      make(map[ActionKind]int, len(rec.Refused)),
			totalRefused: rec.TotalRefused,
			autoBans:     rec.AutoBans,
			autoBannedAt: rec.AutoBannedAt,
			bannedUntil:  rec.BannedUntil,
		}
		if r.firstTrip.IsZero() || r.firstTrip.After(r.lastTrip) {
			r.firstTrip = r.lastTrip
		}
		for k, n := range rec.Refused {
			r.refused[k] = n
		}
		e.rec = r
		if e.lastSeen.IsZero() {
			e.lastSeen = rec.LastTrip
		}
		// Oldest first, so the most recent name ends up in front.
		for i := len(rec.Usernames) - 1; i >= 0; i-- {
			e.noteUsername(rec.Usernames[i])
		}
	}
}

// Janitor periodically drops idle entries and expired records. Run it in its
// own goroutine.
func (l *ActionLimiter) Janitor(ctx context.Context) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			l.mu.Lock()
			l.pruneLocked(l.now(), l.currentLimits())
			l.mu.Unlock()
		}
	}
}

// pruneLocked trims every entry's windows and strikes, drops records past
// ReportTTL, and deletes entries with nothing left. Called with mu held.
func (l *ActionLimiter) pruneLocked(now time.Time, limits ActionLimits) {
	for ip, e := range l.ips {
		for k, ts := range e.windows {
			rate, ok := limits.Rates[k]
			if ok && rate.Window > 0 {
				ts = pruneWindow(ts, now, rate.Window)
			} else {
				ts = nil // the kind is no longer limited
			}
			if len(ts) == 0 {
				delete(e.windows, k)
			} else {
				e.windows[k] = ts
			}
		}
		if limits.BanWindow > 0 {
			e.strikes = pruneWindow(e.strikes, now, limits.BanWindow)
		} else {
			e.strikes = nil
		}
		if e.rec != nil && l.reportExpired(e.rec.lastTrip, e.rec.bannedUntil, now, limits) {
			e.rec = nil
		}
		if e.rec == nil && len(e.windows) == 0 && len(e.strikes) == 0 {
			delete(l.ips, ip)
		}
	}
}

func (l *ActionLimiter) reportExpired(lastTrip, bannedUntil, now time.Time, limits ActionLimits) bool {
	ttl := limits.ReportTTL
	if ttl <= 0 {
		ttl = DefaultActionReportTTL
	}
	return now.Sub(lastTrip) > ttl && !now.Before(bannedUntil)
}

// entryFor returns ip's entry, creating it (and making room) if needed.
// Called with mu held.
func (l *ActionLimiter) entryFor(ip string, now time.Time, limits ActionLimits) *actionEntry {
	if e := l.ips[ip]; e != nil {
		return e
	}
	maxIPs := limits.MaxIPs
	if maxIPs <= 0 {
		maxIPs = DefaultActionMaxIPs
	}
	if len(l.ips) >= maxIPs {
		l.pruneLocked(now, limits)
		for len(l.ips) >= maxIPs {
			l.evictOneLocked()
		}
	}
	e := &actionEntry{windows: make(map[ActionKind][]time.Time)}
	l.ips[ip] = e
	return e
}

// evictOneLocked drops the entry that matters least: an IP that was never
// refused (least recently seen first), otherwise the least recently tripped.
func (l *ActionLimiter) evictOneLocked() {
	var victim string
	var victimTripped bool
	var victimTime time.Time
	for ip, e := range l.ips {
		tripped := e.rec != nil
		t := e.lastSeen
		if tripped {
			t = e.rec.lastTrip
		}
		if victim == "" ||
			(!tripped && victimTripped) ||
			(tripped == victimTripped && (t.Before(victimTime) || (t.Equal(victimTime) && ip < victim))) {
			victim, victimTripped, victimTime = ip, tripped, t
		}
	}
	delete(l.ips, victim)
}

// report returns the entry's record, creating it on the first trip.
func (e *actionEntry) report(now time.Time) *actionReport {
	if e.rec == nil {
		e.rec = &actionReport{firstTrip: now, refused: make(map[ActionKind]int)}
	}
	return e.rec
}

// noteUsername moves name to the front of the recent-usernames list.
func (e *actionEntry) noteUsername(name string) {
	if name == "" || (len(e.usernames) > 0 && e.usernames[0] == name) {
		return
	}
	for i, n := range e.usernames {
		if n == name {
			copy(e.usernames[1:i+1], e.usernames[:i])
			e.usernames[0] = name
			return
		}
	}
	if len(e.usernames) < maxUsernames {
		e.usernames = append(e.usernames, "")
	}
	copy(e.usernames[1:], e.usernames)
	e.usernames[0] = name
}

// countWithin counts the timestamps newer than now-window without modifying ts.
func countWithin(ts []time.Time, now time.Time, window time.Duration) int {
	cutoff := now.Add(-window)
	n := 0
	for _, t := range ts {
		if t.After(cutoff) {
			n++
		}
	}
	return n
}
