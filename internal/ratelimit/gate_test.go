package ratelimit

import (
	"testing"
	"time"
)

func TestAllowRateLimitAndBan(t *testing.T) {
	g := NewConnectGate()
	ip := "203.0.113.7"

	for i := 0; i < RateLimit; i++ {
		ok, _ := g.Allow(ip)
		if !ok {
			t.Fatalf("attempt %d: expected allowed", i+1)
		}
		g.Release(ip)
	}
	ok, retry := g.Allow(ip)
	if ok {
		t.Fatal("expected rate limit after RateLimit attempts")
	}
	if retry <= 0 || retry > BanDuration {
		t.Fatalf("bad retry-after: %v", retry)
	}
	// Still banned on the next attempt.
	if ok, _ := g.Allow(ip); ok {
		t.Fatal("expected ban to persist")
	}
}

func TestConcurrentCap(t *testing.T) {
	g := NewConnectGate()
	ip := "203.0.113.8"

	for i := 0; i < MaxConcurrentPerIP; i++ {
		if ok, _ := g.Allow(ip); !ok {
			t.Fatalf("connection %d: expected allowed", i+1)
		}
	}
	if ok, _ := g.Allow(ip); ok {
		t.Fatal("expected concurrent cap to deny")
	}
	// Releasing one slot frees one connection.
	g.Release(ip)
	if ok, _ := g.Allow(ip); !ok {
		t.Fatal("expected allowed after Release")
	}
}

func TestLoopbackExempt(t *testing.T) {
	g := NewConnectGate()
	for i := 0; i < RateLimit*3; i++ {
		if ok, _ := g.Allow("127.0.0.1"); !ok {
			t.Fatal("loopback should never be limited")
		}
	}
	if ok, _ := g.Allow("::1"); !ok {
		t.Fatal("IPv6 loopback should never be limited")
	}
}

func TestPruneDropsIdleEntries(t *testing.T) {
	g := NewConnectGate()
	ip := "203.0.113.9"
	g.Allow(ip)
	g.Release(ip)
	g.prune(time.Now().Add(RateLimitWindow + time.Second))
	g.mu.Lock()
	_, exists := g.attempts[ip]
	g.mu.Unlock()
	if exists {
		t.Fatal("expected idle entry to be pruned")
	}
}

func TestLimitsChangeAtRuntime(t *testing.T) {
	limits := DefaultLimits()
	limits.MaxConcurrent = 1
	g := NewConnectGate()
	g.SetLimits(func() Limits { return limits })
	ip := "203.0.113.10"

	if ok, _ := g.Allow(ip); !ok {
		t.Fatal("first connection should be allowed")
	}
	if ok, _ := g.Allow(ip); ok {
		t.Fatal("expected cap of 1 to deny a second connection")
	}
	// Raising the cap applies to the next attempt without a new gate.
	limits.MaxConcurrent = 2
	if ok, _ := g.Allow(ip); !ok {
		t.Fatal("expected raised cap to allow a second connection")
	}
}
