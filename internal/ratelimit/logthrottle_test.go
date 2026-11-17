package ratelimit

import (
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"
)

func testThrottle(interval time.Duration, maxKeys int) (*LogThrottle, *fakeClock) {
	clock := newFakeClock()
	t := NewLogThrottle(interval, maxKeys)
	t.SetClock(clock.Now)
	return t, clock
}

func TestLogThrottleSequence(t *testing.T) {
	lt, clock := testThrottle(time.Minute, 100)
	key := ip1 + " room_create"
	steps := []struct {
		advance        time.Duration
		repeat         int
		wantLog        bool
		wantSuppressed int
	}{
		{0, 1, true, 0},                 // first refusal: logged
		{time.Second, 999, false, 0},    // the flood: silent
		{58 * time.Second, 1, false, 0}, // 59s after the first line: still silent
		{time.Second, 1, true, 1000},    // a minute on: summary of the 1000 skipped
		{30 * time.Second, 5, false, 0}, // next minute starts quiet
		{30 * time.Second, 1, true, 5},  // and summarizes again
		{10 * time.Minute, 1, true, 0},  // after a quiet spell: logged as a fresh first
	}
	for i, s := range steps {
		clock.Advance(s.advance)
		for r := 0; r < s.repeat; r++ {
			log, suppressed := lt.Allow(key)
			if r == s.repeat-1 && (log != s.wantLog || suppressed != s.wantSuppressed) {
				t.Fatalf("step %d: log=%v suppressed=%d, want log=%v suppressed=%d",
					i, log, suppressed, s.wantLog, s.wantSuppressed)
			}
		}
	}
}

func TestLogThrottleKeysIndependent(t *testing.T) {
	lt, _ := testThrottle(time.Minute, 100)
	for _, k := range []string{"a room_create", "a room_join", "b room_create"} {
		if log, _ := lt.Allow(k); !log {
			t.Fatalf("first event for %q not logged", k)
		}
	}
	if log, _ := lt.Allow("a room_create"); log {
		t.Fatal("second event inside the interval logged")
	}
}

func TestLogThrottleFlush(t *testing.T) {
	lt, clock := testThrottle(time.Minute, 100)
	lt.Allow("flood")
	for i := 0; i < 41; i++ {
		lt.Allow("flood")
	}
	lt.Allow("single")
	if got := lt.Flush(); len(got) != 0 {
		t.Fatalf("flush inside the interval = %v, want nothing", got)
	}
	clock.Advance(time.Minute)
	if got, want := lt.Flush(), []ThrottledKey{{"flood", 41}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("flush = %v, want %v", got, want)
	}
	// Reset: nothing left to report, and the next event is still throttled
	// against the flush's line.
	if got := lt.Flush(); len(got) != 0 {
		t.Fatalf("second flush = %v, want nothing", got)
	}
	if log, _ := lt.Allow("flood"); log {
		t.Fatal("event right after a flushed summary logged")
	}
	// Idle keys are forgotten once their interval passes.
	clock.Advance(time.Minute)
	lt.Flush() // reports flood's 1 skipped event
	clock.Advance(time.Minute)
	lt.Flush()
	lt.mu.Lock()
	n := len(lt.keys)
	lt.mu.Unlock()
	if n != 0 {
		t.Fatalf("%d keys kept after going idle, want 0", n)
	}
}

func TestLogThrottleBounded(t *testing.T) {
	lt, clock := testThrottle(time.Minute, 10)
	for i := 0; i < 1000; i++ {
		lt.Allow(fmt.Sprintf("k%d", i))
		clock.Advance(time.Millisecond)
	}
	lt.mu.Lock()
	n := len(lt.keys)
	_, newest := lt.keys["k999"]
	lt.mu.Unlock()
	if n > 10 || !newest {
		t.Fatalf("%d keys (newest kept: %v), want at most 10 with the newest kept", n, newest)
	}
}

func TestLogThrottleConcurrent(t *testing.T) {
	lt, clock := testThrottle(time.Minute, 100)
	var wg sync.WaitGroup
	var mu sync.Mutex
	logged := 0
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 500; i++ {
				if log, _ := lt.Allow("shared"); log {
					mu.Lock()
					logged++
					mu.Unlock()
				}
				if i%100 == 0 {
					lt.Flush()
				}
			}
		}(g)
	}
	wg.Wait()
	if logged != 1 {
		t.Fatalf("logged %d lines for one key inside one interval, want 1", logged)
	}
	clock.Advance(time.Minute)
	if log, suppressed := lt.Allow("shared"); !log || suppressed != 3999 {
		t.Fatalf("after the interval: log=%v suppressed=%d, want true 3999", log, suppressed)
	}
}
