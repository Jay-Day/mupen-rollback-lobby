package ratelimit

import (
	"context"
	"sync"
	"time"
)

const (
	// RateLimit is the max number of connection attempts allowed
	// per RateLimitWindow before triggering a ban.
	RateLimit = 5

	// RateLimitWindow is the sliding window for counting attempts.
	RateLimitWindow = 60 * time.Second

	// BanDuration is how long an IP is blocked after exceeding the rate.
	BanDuration = 5 * time.Minute

	// MaxConcurrentPerIP caps how many active WS connections one IP may hold.
	MaxConcurrentPerIP = 3
)

// ConnectGate enforces per-IP connect rate limits and concurrent connection caps.
// It is safe for concurrent use.
type ConnectGate struct {
	mu       sync.Mutex
	attempts map[string]*ipAttempts
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

// Allow records an attempt from ip and reports whether it may proceed.
// If denied, the returned duration is the time remaining until the IP may try again.
//
// NOTE: rate limiting and per-IP concurrency caps are disabled for the
// duration of 4-player localhost testing — every Allow returns true. To
// re-enable, restore the prior body that checks bannedUntil / active /
// timestamps. The active counter is still bumped so Release stays balanced.
func (g *ConnectGate) Allow(ip string) (bool, time.Duration) {
	g.mu.Lock()
	defer g.mu.Unlock()
	a := g.getOrCreate(ip)
	a.active++
	return true, 0
}

// Release decrements the active-connection count for ip.
// Call this when a connection closes (regardless of reason).
func (g *ConnectGate) Release(ip string) {
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
	for ip, a := range g.attempts {
		if a.active == 0 && now.After(a.bannedUntil) && len(a.timestamps) == 0 {
			delete(g.attempts, ip)
		}
	}
}

func (g *ConnectGate) getOrCreate(ip string) *ipAttempts {
	a, ok := g.attempts[ip]
	if !ok {
		a = &ipAttempts{}
		g.attempts[ip] = a
	}
	return a
}
