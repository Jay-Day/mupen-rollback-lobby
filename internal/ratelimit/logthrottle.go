package ratelimit

import (
	"context"
	"sort"
	"sync"
	"time"
)

// LogThrottle keeps a flood of identical events from becoming a flood of log
// lines. For each key (say, an IP plus an action kind) the first event is
// logged, then at most one line per interval, which carries the number of
// events skipped since the previous line. It is safe for concurrent use.
type LogThrottle struct {
	mu       sync.Mutex
	interval time.Duration
	maxKeys  int
	now      func() time.Time
	keys     map[string]*throttleState
}

type throttleState struct {
	lastLog    time.Time
	suppressed int
}

// ThrottledKey is a key with events that were never logged, from Flush.
type ThrottledKey struct {
	Key        string
	Suppressed int
}

// NewLogThrottle logs each key at most once per interval and tracks at most
// maxKeys keys (the least recently logged is forgotten first).
func NewLogThrottle(interval time.Duration, maxKeys int) *LogThrottle {
	if interval <= 0 {
		interval = time.Minute
	}
	if maxKeys <= 0 {
		maxKeys = DefaultActionMaxIPs
	}
	return &LogThrottle{
		interval: interval,
		maxKeys:  maxKeys,
		now:      time.Now,
		keys:     make(map[string]*throttleState),
	}
}

// SetClock replaces the time source (for tests).
func (t *LogThrottle) SetClock(now func() time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.now = now
}

// Allow records one event for key and reports whether to log it. suppressed
// is how many events for key were skipped since the last logged one, so the
// line can say "and N more".
func (t *LogThrottle) Allow(key string) (log bool, suppressed int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	s := t.keys[key]
	if s == nil {
		if len(t.keys) >= t.maxKeys {
			t.makeRoomLocked(now)
		}
		t.keys[key] = &throttleState{lastLog: now}
		return true, 0
	}
	if now.Sub(s.lastLog) < t.interval {
		s.suppressed++
		return false, 0
	}
	suppressed = s.suppressed
	s.lastLog = now
	s.suppressed = 0
	return true, suppressed
}

// Flush returns the keys whose skipped events have waited a full interval
// without being logged (the flood stopped before another line was due), so
// a periodic task can write their summaries, and resets them. Keys with
// nothing pending are forgotten once their interval has passed.
func (t *LogThrottle) Flush() []ThrottledKey {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	var out []ThrottledKey
	for k, s := range t.keys {
		if now.Sub(s.lastLog) < t.interval {
			continue
		}
		if s.suppressed > 0 {
			out = append(out, ThrottledKey{Key: k, Suppressed: s.suppressed})
			s.suppressed = 0
			s.lastLog = now
			continue
		}
		delete(t.keys, k)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// Janitor calls Flush every interval and hands each pending summary to
// report. Run it in its own goroutine.
func (t *LogThrottle) Janitor(ctx context.Context, report func(key string, suppressed int)) {
	ticker := time.NewTicker(t.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			for _, k := range t.Flush() {
				if report != nil {
					report(k.Key, k.Suppressed)
				}
			}
		}
	}
}

// makeRoomLocked forgets idle keys, then the least recently logged ones,
// until there is room for one more. Called with mu held.
func (t *LogThrottle) makeRoomLocked(now time.Time) {
	for k, s := range t.keys {
		if s.suppressed == 0 && now.Sub(s.lastLog) >= t.interval {
			delete(t.keys, k)
		}
	}
	for len(t.keys) >= t.maxKeys {
		var victim string
		var oldest time.Time
		for k, s := range t.keys {
			if victim == "" || s.lastLog.Before(oldest) || (s.lastLog.Equal(oldest) && k < victim) {
				victim, oldest = k, s.lastLog
			}
		}
		delete(t.keys, victim)
	}
}
