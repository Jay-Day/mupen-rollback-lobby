package moderation

import (
	"fmt"
	"log/slog"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"
)

// LoginGuard stops password guessing against the moderator credentials. One
// guard is shared by every place a moderator password is checked (the
// moderator website and the lobby's ADMIN_AUTH from the desktop client), so
// moving between them doesn't reset the count.
//
// It counts failed logins per IP in a row; a success resets the count. At
// the policy's MaxFailures (lobby.json adminLoginMaxFailures, default 5) the
// IP is banned for the policy's BanDuration (adminLoginBanSeconds, default
// permanent): a ban like a moderator's, shown on the Sanctions page and
// lifted there, with /unban in the lobby, or by editing bans.json. The OnBan
// hook closes the IP's lobby connections. Loopback addresses are never
// banned, since everyone reaching the website through an SSH tunnel arrives
// from 127.0.0.1: they are locked out of moderator logins for
// LoopbackLockout instead.
//
// Memory is bounded: an IP's count is forgotten after guardForget without a
// failure, and at most guardMaxEntries IPs are tracked (the stalest go first).
type LoginGuard struct {
	mod    *Manager
	now    func() time.Time
	policy atomic.Pointer[func() LoginPolicy]

	// tryMu runs Try one at a time, so a burst of parallel guesses can't all
	// be checked before the last allowed failure bans the IP. A check is one
	// sha256 per moderator, so this costs honest logins nothing.
	tryMu sync.Mutex

	mu    sync.Mutex
	ips   map[string]*loginFails
	onBan func(GuardBan)
}

type loginFails struct {
	count       int
	last        time.Time
	lockedUntil time.Time
}

// Guard settings.
const (
	// MaxLoginFailures is the default for lobby.json adminLoginMaxFailures.
	MaxLoginFailures = 5
	LoopbackLockout  = 15 * time.Minute
	AutoBanBy        = "auto"

	guardForget     = 24 * time.Hour
	guardMaxEntries = 4096
)

// AutoBanReason is the reason a ban by the default policy records.
var AutoBanReason = LoginBanReason(MaxLoginFailures)

// LoginBanReason is the reason recorded for a ban after n failures.
func LoginBanReason(n int) string {
	if n == 1 {
		return "a failed moderator login"
	}
	return fmt.Sprintf("%d failed moderator logins in a row", n)
}

// LoginPolicy is when the guard bans: after MaxFailures wrong passwords in a
// row, for BanDuration (0: permanent).
type LoginPolicy struct {
	MaxFailures int
	BanDuration time.Duration
}

// DefaultLoginPolicy is 5 failures, banned for good.
func DefaultLoginPolicy() LoginPolicy { return LoginPolicy{MaxFailures: MaxLoginFailures} }

// GuardBan is one ban the guard made, for its OnBan hook.
type GuardBan struct {
	IP       string
	Where    string        // "moderator site" or "lobby"
	Reason   string        // as recorded on the ban
	Duration time.Duration // 0: permanent
}

// LoginResult is what a login attempt led to.
type LoginResult int

const (
	LoginCounted LoginResult = iota // wrong password, counted; more tries left
	LoginBanned                     // wrong password, and this failure banned the IP
	LoginLocked                     // wrong password, and loopback is now (or still) locked out
	LoginOK                         // right password (Try only)
	LoginRefused                    // not checked: the IP is banned or locked out (Try only)
)

// Try checks a moderator password from ip through the guard: refused
// without calling check while the IP is banned or locked out, otherwise
// check's answer is counted. Tries run one at a time.
func (g *LoginGuard) Try(ip, where string, check func() bool) LoginResult {
	if g == nil {
		if check() {
			return LoginOK
		}
		return LoginCounted
	}
	g.tryMu.Lock()
	defer g.tryMu.Unlock()
	if !IsLoopback(ip) && g.mod != nil {
		if _, banned := g.mod.IsBanned(CanonicalIP(ip)); banned {
			return LoginRefused
		}
	}
	if ok, _ := g.Allow(ip); !ok {
		return LoginRefused
	}
	if check() {
		g.Succeeded(ip)
		return LoginOK
	}
	return g.Failed(ip, where)
}

// NewLoginGuard makes a guard that bans through mod (nil: count and lock out,
// but ban nobody), with the default policy until SetPolicy.
func NewLoginGuard(mod *Manager) *LoginGuard {
	return &LoginGuard{mod: mod, now: time.Now, ips: map[string]*loginFails{}}
}

// SetPolicy sets where the policy comes from; it's read at every failure, so
// a lobby.json edit applies to the next one.
func (g *LoginGuard) SetPolicy(get func() LoginPolicy) {
	if g == nil || get == nil {
		return
	}
	g.policy.Store(&get)
}

func (g *LoginGuard) currentPolicy() LoginPolicy {
	p := DefaultLoginPolicy()
	if get := g.policy.Load(); get != nil {
		p = (*get)()
	}
	if p.MaxFailures < 1 {
		p.MaxFailures = 1
	}
	if p.BanDuration < 0 {
		p.BanDuration = 0
	}
	return p
}

// SetOnBan sets what happens after the guard bans an IP: close its lobby
// connections and record the ban. Called without the guard's lock held.
func (g *LoginGuard) SetOnBan(f func(GuardBan)) {
	if g == nil {
		return
	}
	g.mu.Lock()
	g.onBan = f
	g.mu.Unlock()
}

// CanonicalIP is ip as the lobby writes addresses (IPv4-mapped IPv6 as
// IPv4), or ip unchanged when it isn't an address.
func CanonicalIP(ip string) string {
	if a, err := netip.ParseAddr(ip); err == nil {
		return a.Unmap().String()
	}
	return ip
}

// IsLoopback reports whether ip is a loopback address (127.0.0.0/8, ::1).
func IsLoopback(ip string) bool {
	a, err := netip.ParseAddr(ip)
	return err == nil && a.Unmap().IsLoopback()
}

// Allow reports whether ip may try a moderator password now: false while a
// loopback lockout lasts, with the time left.
func (g *LoginGuard) Allow(ip string) (bool, time.Duration) {
	if g == nil {
		return true, 0
	}
	ip = CanonicalIP(ip)
	now := g.now()
	g.mu.Lock()
	defer g.mu.Unlock()
	f := g.ips[ip]
	if f == nil || !now.Before(f.lockedUntil) {
		return true, 0
	}
	return false, f.lockedUntil.Sub(now)
}

// Succeeded resets ip's count after a correct password.
func (g *LoginGuard) Succeeded(ip string) {
	if g == nil {
		return
	}
	ip = CanonicalIP(ip)
	g.mu.Lock()
	delete(g.ips, ip)
	g.mu.Unlock()
}

// Failed counts a wrong password from ip; where ("moderator site", "lobby")
// goes into the log line. The failure that reaches the policy's MaxFailures
// bans the IP, or locks out a loopback address.
func (g *LoginGuard) Failed(ip, where string) LoginResult {
	if g == nil {
		return LoginCounted
	}
	ip = CanonicalIP(ip)
	policy := g.currentPolicy()
	now := g.now()
	g.mu.Lock()
	g.pruneLocked(now)
	f := g.ips[ip]
	if f == nil {
		f = &loginFails{}
		g.ips[ip] = f
	}
	if now.Before(f.lockedUntil) {
		// Callers check Allow first; a straggler doesn't extend the lockout.
		g.mu.Unlock()
		return LoginLocked
	}
	f.count++
	f.last = now
	if f.count < policy.MaxFailures {
		g.mu.Unlock()
		return LoginCounted
	}
	if IsLoopback(ip) {
		f.count = 0
		f.lockedUntil = now.Add(LoopbackLockout)
		g.mu.Unlock()
		slog.Warn("moderator logins locked out after repeated failures (loopback is never banned)",
			"ip", ip, "where", where, "failures", policy.MaxFailures, "for", LoopbackLockout)
		return LoginLocked
	}
	delete(g.ips, ip) // the ban takes over
	onBan := g.onBan
	g.mu.Unlock()

	ban := GuardBan{IP: ip, Where: where, Reason: LoginBanReason(policy.MaxFailures), Duration: policy.BanDuration}
	if g.mod != nil {
		g.mod.Ban(ip, AutoBanBy, ban.Reason, ban.Duration)
	}
	slog.Warn("IP banned for repeated failed moderator logins", "ip", ip, "where", where,
		"failures", policy.MaxFailures, "for", banLength(ban.Duration))
	if onBan != nil {
		onBan(ban)
	}
	return LoginBanned
}

func banLength(d time.Duration) string {
	if d <= 0 {
		return "permanent"
	}
	return d.String()
}

// pruneLocked forgets stale counts, and the stalest ones beyond the cap.
func (g *LoginGuard) pruneLocked(now time.Time) {
	for ip, f := range g.ips {
		if now.Sub(f.last) > guardForget && !now.Before(f.lockedUntil) {
			delete(g.ips, ip)
		}
	}
	for len(g.ips) >= guardMaxEntries {
		var oldest string
		var at time.Time
		for ip, f := range g.ips {
			if now.Before(f.lockedUntil) {
				continue // a lockout is never forgotten early
			}
			if oldest == "" || f.last.Before(at) {
				oldest, at = ip, f.last
			}
		}
		if oldest == "" {
			return
		}
		delete(g.ips, oldest)
	}
}

// Tracked is how many IPs the guard holds a count for, for tests.
func (g *LoginGuard) Tracked() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.ips)
}
