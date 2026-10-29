package ratelimit

import (
	"context"
	"net"
	"sync"
	"time"
)

// Default thresholds. The lobby config can override them at runtime.
const (
	// RateLimit is the max number of connection attempts allowed
	// per RateLimitWindow before triggering a ban.
	RateLimit = 5

	// RateLimitWindow is the sliding window for counting attempts.
	RateLimitWindow = 60 * time.Second

	// BanDuration is how long an IP is blocked after exceeding the rate.
	BanDuration = 5 * time.Minute

	// MaxConcurrentPerIP caps how many active WS connections one IP may hold.
	// 4 so a full 4-player group behind one home NAT can all connect.
	MaxConcurrentPerIP = 4
)

// Limits are the thresholds a ConnectGate enforces.
type Limits struct {
	Attempts      int           // connect attempts allowed per Window
	Window        time.Duration // sliding window for counting attempts
	BanDuration   time.Duration // how long an IP is blocked after exceeding Attempts
	MaxConcurrent int           // active connections one IP may hold
}

// DefaultLimits returns the built-in thresholds.
func DefaultLimits() Limits {
	return Limits{
		Attempts:      RateLimit,
		Window:        RateLimitWindow,
		BanDuration:   BanDuration,
		MaxConcurrent: MaxConcurrentPerIP,
	}
}

// ConnectGate enforces per-IP connect rate limits and concurrent connection caps.
// It is safe for concurrent use.
type ConnectGate struct {
	mu       sync.Mutex
	attempts map[string]*ipAttempts
	limits   func() Limits // nil = DefaultLimits
}

type ipAttempts struct {
	timestamps  []time.Time
	bannedUntil time.Time
	active      int
}

func NewConnectGate() *ConnectGate {
	return &ConnectGate{
		attempts: make(map[string]*ipAttempts),
	}
}

// SetLimits makes the gate read its thresholds from f on every check, so they
// can change while the server runs. f must not call back into the gate.
func (g *ConnectGate) SetLimits(f func() Limits) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.limits = f
}

// currentLimits must be called with mu held.
func (g *ConnectGate) currentLimits() Limits {
	if g.limits == nil {
		return DefaultLimits()
	}
	return g.limits()
}

// Allow records an attempt from ip and reports whether it may proceed.
// If denied, the returned duration is the time remaining until the IP may try again.
// Loopback is exempt so local multi-instance testing is never throttled.
func (g *ConnectGate) Allow(ip string) (bool, time.Duration) {
	if isLoopback(ip) {
		return true, 0
	}
	now := time.Now()
	g.mu.Lock()
	defer g.mu.Unlock()
	limits := g.currentLimits()
	a := g.getOrCreate(ip)

	if now.Before(a.bannedUntil) {
		return false, a.bannedUntil.Sub(now)
	}
	if a.active >= limits.MaxConcurrent {
		return false, limits.Window
	}

	// Slide the attempt window, then check the rate.
	a.timestamps = pruneWindow(a.timestamps, now, limits.Window)
	if len(a.timestamps) >= limits.Attempts {
		a.bannedUntil = now.Add(limits.BanDuration)
		a.timestamps = a.timestamps[:0]
		return false, limits.BanDuration
	}

	a.timestamps = append(a.timestamps, now)
	a.active++
	return true, 0
}

// Release decrements the active-connection count for ip.
// Call this when a connection closes (regardless of reason).
func (g *ConnectGate) Release(ip string) {
	if isLoopback(ip) {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	a, ok := g.attempts[ip]
	if !ok {
		return
	}
	if a.active > 0 {
		a.active--
	}
}

// Janitor periodically prunes stale entries. Run in its own goroutine.
func (g *ConnectGate) Janitor(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			g.prune(time.Now())
		}
	}
}

func (g *ConnectGate) prune(now time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	window := g.currentLimits().Window
	for ip, a := range g.attempts {
		a.timestamps = pruneWindow(a.timestamps, now, window)
		if a.active == 0 && now.After(a.bannedUntil) && len(a.timestamps) == 0 {
			delete(g.attempts, ip)
		}
	}
}

// pruneWindow drops timestamps older than window, in place.
func pruneWindow(ts []time.Time, now time.Time, window time.Duration) []time.Time {
	cutoff := now.Add(-window)
	kept := ts[:0]
	for _, t := range ts {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	return kept
}

func isLoopback(ip string) bool {
	if parsed := net.ParseIP(ip); parsed != nil {
		return parsed.IsLoopback()
	}
	return false
}

func (g *ConnectGate) getOrCreate(ip string) *ipAttempts {
	a, ok := g.attempts[ip]
	if !ok {
		a = &ipAttempts{}
		g.attempts[ip] = a
	}
	return a
}
