package ratelimit

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"
)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// limitsVar lets a test change the limits between calls, like a lobby.json
// reload does.
type limitsVar struct {
	mu sync.Mutex
	l  ActionLimits
}

func (v *limitsVar) Get() ActionLimits {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.l
}

func (v *limitsVar) Set(l ActionLimits) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.l = l
}

func testLimiter(t *testing.T, limits ActionLimits) (*ActionLimiter, *fakeClock, *limitsVar) {
	t.Helper()
	clock := newFakeClock()
	v := &limitsVar{l: limits}
	l := NewActionLimiter()
	l.SetClock(clock.Now)
	l.SetLimits(v.Get)
	return l, clock, v
}

func oneRate(kind ActionKind, count int, window time.Duration) ActionLimits {
	limits := DefaultActionLimits()
	limits.Rates = map[ActionKind]Rate{kind: {Count: count, Window: window}}
	return limits
}

const ip1, ip2 = "203.0.113.7", "198.51.100.20"

func TestActionDefaultLimitsPerKind(t *testing.T) {
	want := map[ActionKind]Rate{
		ActionRoomCreate:  {5, time.Minute},
		ActionMatchStart:  {10, time.Minute},
		ActionRoomJoin:    {15, time.Minute},
		ActionSpectate:    {10, time.Minute},
		ActionRoomUpdate:  {40, 10 * time.Second},
		ActionLobbyUpdate: {30, time.Minute},
	}
	if got := DefaultActionLimits().Rates; !reflect.DeepEqual(got, want) {
		t.Fatalf("default rates = %v, want %v", got, want)
	}
	for kind, rate := range want {
		t.Run(string(kind), func(t *testing.T) {
			l := NewActionLimiter() // built-in limits, real clock
			for i := 0; i < rate.Count; i++ {
				if ok, _, _ := l.Allow(ip1, kind, "alice"); !ok {
					t.Fatalf("request %d of %d refused", i+1, rate.Count)
				}
			}
			ok, retry, ban := l.Allow(ip1, kind, "alice")
			if ok || ban {
				t.Fatalf("request %d: ok=%v ban=%v, want refused without ban", rate.Count+1, ok, ban)
			}
			if retry <= 0 || retry > rate.Window {
				t.Fatalf("retryAfter = %v, want in (0, %v]", retry, rate.Window)
			}
		})
	}
}

func TestActionWindowSlides(t *testing.T) {
	l, clock, _ := testLimiter(t, oneRate(ActionRoomCreate, 3, 10*time.Second))
	steps := []struct {
		at        time.Duration // since the first request
		wantOK    bool
		wantRetry time.Duration
	}{
		{0, true, 0},
		{1 * time.Second, true, 0},
		{2 * time.Second, true, 0},
		{3 * time.Second, false, 7 * time.Second},              // 0s request leaves at 10s
		{9999 * time.Millisecond, false, 1 * time.Millisecond}, // still inside
		{10 * time.Second, true, 0},                            // 0s request expired
		{10500 * time.Millisecond, false, 500 * time.Millisecond},
		{11 * time.Second, true, 0}, // 1s request expired
		{12 * time.Second, true, 0}, // 2s request expired
		{12 * time.Second, false, 8 * time.Second},
	}
	start := clock.Now()
	for i, s := range steps {
		clock.Advance(start.Add(s.at).Sub(clock.Now()))
		ok, retry, _ := l.Allow(ip1, ActionRoomCreate, "alice")
		if ok != s.wantOK || retry != s.wantRetry {
			t.Fatalf("step %d at %v: ok=%v retry=%v, want ok=%v retry=%v",
				i, s.at, ok, retry, s.wantOK, s.wantRetry)
		}
	}
}

func TestActionKindsAndIPsIndependent(t *testing.T) {
	limits := DefaultActionLimits()
	limits.Rates = map[ActionKind]Rate{
		ActionRoomCreate: {2, time.Minute},
		ActionRoomJoin:   {2, time.Minute},
	}
	l, _, _ := testLimiter(t, limits)
	for i := 0; i < 2; i++ {
		l.Allow(ip1, ActionRoomCreate, "alice")
	}
	cases := []struct {
		ip     string
		kind   ActionKind
		wantOK bool
	}{
		{ip1, ActionRoomCreate, false}, // spent
		{ip1, ActionRoomJoin, true},    // its own budget
		{ip2, ActionRoomCreate, true},  // another IP
		{ip1, ActionMatchStart, true},  // not configured: unlimited
	}
	for _, c := range cases {
		if ok, _, _ := l.Allow(c.ip, c.kind, "x"); ok != c.wantOK {
			t.Errorf("%s %s: ok=%v, want %v", c.ip, c.kind, ok, c.wantOK)
		}
	}
}

func TestActionBudgetSharedAcrossUsernames(t *testing.T) {
	l, _, _ := testLimiter(t, oneRate(ActionRoomCreate, 2, time.Minute))
	l.Allow(ip1, ActionRoomCreate, "alice")
	l.Allow(ip1, ActionRoomCreate, "bob") // second connection, same IP
	if ok, _, _ := l.Allow(ip1, ActionRoomCreate, "carol"); ok {
		t.Fatal("third request from the same IP allowed; the budget must be per IP")
	}
	recs := l.Snapshot()
	if len(recs) != 1 || !reflect.DeepEqual(recs[0].Usernames, []string{"carol", "bob", "alice"}) {
		t.Fatalf("records = %+v, want one with usernames [carol bob alice]", recs)
	}
}

func TestActionHotLimitChange(t *testing.T) {
	l, clock, v := testLimiter(t, oneRate(ActionRoomJoin, 2, time.Minute))
	l.Allow(ip1, ActionRoomJoin, "a")
	clock.Advance(time.Second)
	l.Allow(ip1, ActionRoomJoin, "a")
	if ok, _, _ := l.Allow(ip1, ActionRoomJoin, "a"); ok {
		t.Fatal("third request allowed at 2/min")
	}

	// Raised: applies to the next request.
	v.Set(oneRate(ActionRoomJoin, 4, time.Minute))
	if ok, _, _ := l.Allow(ip1, ActionRoomJoin, "a"); !ok {
		t.Fatal("request refused after raising the limit to 4")
	}

	// Lowered below what the window holds: refused, and the retry is when the
	// newest-but-one request leaves (only the newest 1 still matter).
	clock.Advance(time.Second)
	v.Set(oneRate(ActionRoomJoin, 1, time.Minute))
	ok, retry, _ := l.Allow(ip1, ActionRoomJoin, "a")
	if ok {
		t.Fatal("request allowed after lowering the limit to 1")
	}
	if want := 59 * time.Second; retry != want {
		t.Fatalf("retry = %v, want %v (the newest request, 1s old, leaves the window)", retry, want)
	}

	// Disabled (Count 0) and removed kinds are unlimited.
	v.Set(oneRate(ActionRoomJoin, 0, time.Minute))
	if ok, _, _ := l.Allow(ip1, ActionRoomJoin, "a"); !ok {
		t.Fatal("request refused with the kind disabled")
	}
	v.Set(oneRate(ActionRoomCreate, 1, time.Minute))
	for i := 0; i < 5; i++ {
		if ok, _, _ := l.Allow(ip1, ActionRoomJoin, "a"); !ok {
			t.Fatal("request refused for a kind with no rate")
		}
	}

	// A longer window applies to requests already recorded.
	v.Set(oneRate(ActionRoomCreate, 1, time.Minute))
	l.Allow(ip2, ActionRoomCreate, "b")
	clock.Advance(90 * time.Second)
	v.Set(oneRate(ActionRoomCreate, 1, 2*time.Minute))
	if ok, retry, _ := l.Allow(ip2, ActionRoomCreate, "b"); ok || retry != 30*time.Second {
		t.Fatalf("after widening the window: ok=%v retry=%v, want refused, retry 30s", ok, retry)
	}
}

func TestActionBanSignal(t *testing.T) {
	limits := oneRate(ActionRoomCreate, 1, 24*time.Hour) // everything after the first is refused
	limits.BanStrikes = 5
	limits.BanWindow = time.Minute
	limits.BanDuration = time.Hour

	t.Run("exactly once per ban", func(t *testing.T) {
		l, clock, _ := testLimiter(t, limits)
		l.Allow(ip1, ActionRoomCreate, "a")
		bans := 0
		for i := 1; i <= 200; i++ {
			_, _, ban := l.Allow(ip1, ActionRoomCreate, "a")
			if ban {
				bans++
				if i != 5 {
					t.Fatalf("ban reported on strike %d, want strike 5", i)
				}
			}
		}
		if bans != 1 {
			t.Fatalf("ban reported %d times during one ban, want 1", bans)
		}
		recs := l.Snapshot()
		if recs[0].AutoBans != 1 || !recs[0].AutoBannedAt.Equal(clock.Now()) ||
			!recs[0].BannedUntil.Equal(clock.Now().Add(time.Hour)) {
			t.Fatalf("record ban fields = %+v", recs[0])
		}
		// Once that ban has run out, a new run of strikes reports again.
		clock.Advance(time.Hour)
		for i := 1; i <= 5; i++ {
			_, _, ban := l.Allow(ip1, ActionRoomCreate, "a")
			if ban != (i == 5) {
				t.Fatalf("after the ban: strike %d ban=%v", i, ban)
			}
		}
		if got := l.Snapshot()[0].AutoBans; got != 2 {
			t.Fatalf("AutoBans = %d, want 2", got)
		}
	})

	t.Run("strikes slide out of the window", func(t *testing.T) {
		l, clock, _ := testLimiter(t, limits)
		l.Allow(ip1, ActionRoomCreate, "a")
		for i := 0; i < 4; i++ {
			l.Allow(ip1, ActionRoomCreate, "a")
		}
		clock.Advance(time.Minute) // the 4 strikes leave the 1-minute window
		for i := 0; i < 4; i++ {
			if _, _, ban := l.Allow(ip1, ActionRoomCreate, "a"); ban {
				t.Fatalf("ban after %d strikes inside the window", i+1)
			}
		}
		if _, _, ban := l.Allow(ip1, ActionRoomCreate, "a"); !ban {
			t.Fatal("5th strike inside the window did not ban")
		}
	})

	t.Run("strikes count across kinds", func(t *testing.T) {
		lim := limits
		lim.Rates = map[ActionKind]Rate{
			ActionRoomCreate: {1, 24 * time.Hour},
			ActionRoomJoin:   {1, 24 * time.Hour},
		}
		l, _, _ := testLimiter(t, lim)
		l.Allow(ip1, ActionRoomCreate, "a")
		l.Allow(ip1, ActionRoomJoin, "a")
		kinds := []ActionKind{ActionRoomCreate, ActionRoomJoin, ActionRoomCreate, ActionRoomJoin, ActionRoomCreate}
		for i, k := range kinds {
			if _, _, ban := l.Allow(ip1, k, "a"); ban != (i == 4) {
				t.Fatalf("strike %d (%s): ban=%v", i+1, k, ban)
			}
		}
	})

	t.Run("disabled", func(t *testing.T) {
		lim := limits
		lim.BanStrikes = 0
		l, _, _ := testLimiter(t, lim)
		for i := 0; i < 500; i++ {
			if _, _, ban := l.Allow(ip1, ActionRoomCreate, "a"); ban {
				t.Fatal("ban reported with BanStrikes 0")
			}
		}
	})

	t.Run("ClearBan re-arms", func(t *testing.T) {
		l, _, _ := testLimiter(t, limits)
		l.Allow(ip1, ActionRoomCreate, "a")
		for i := 0; i < 5; i++ {
			l.Allow(ip1, ActionRoomCreate, "a")
		}
		l.ClearBan(ip1) // a moderator unbanned early
		for i := 1; i <= 5; i++ {
			if _, _, ban := l.Allow(ip1, ActionRoomCreate, "a"); ban != (i == 5) {
				t.Fatalf("after ClearBan: strike %d ban=%v", i, ban)
			}
		}
		if got := l.Snapshot()[0]; got.AutoBans != 2 || got.TotalRefused != 10 {
			t.Fatalf("after ClearBan record = %+v, want AutoBans 2, TotalRefused 10", got)
		}
	})
}

func TestActionReportContents(t *testing.T) {
	limits := DefaultActionLimits()
	limits.Rates = map[ActionKind]Rate{
		ActionRoomCreate: {2, time.Minute},
		ActionRoomJoin:   {3, time.Minute},
	}
	l, clock, _ := testLimiter(t, limits)
	t0 := clock.Now()

	// ip2 never trips: no record, even with traffic.
	l.Allow(ip2, ActionRoomCreate, "quiet")

	// ip1: 2 creates allowed, then 3 refused over time; 1 join allowed.
	l.Allow(ip1, ActionRoomCreate, "alice")
	l.Allow(ip1, ActionRoomCreate, "alice")
	clock.Advance(5 * time.Second)
	l.Allow(ip1, ActionRoomCreate, "bob") // first trip
	clock.Advance(5 * time.Second)
	l.Allow(ip1, ActionRoomJoin, "carol")
	l.Allow(ip1, ActionRoomCreate, "alice")
	clock.Advance(5 * time.Second)
	l.Allow(ip1, ActionRoomCreate, "dave") // last trip

	// ip3 trips later than ip1, so it sorts first.
	const ip3 = "192.0.2.55"
	clock.Advance(time.Second)
	l.Allow(ip3, ActionRoomJoin, "eve")
	l.Allow(ip3, ActionRoomJoin, "eve")
	l.Allow(ip3, ActionRoomJoin, "eve")
	l.Allow(ip3, ActionRoomJoin, "eve")

	recs := l.Snapshot()
	if len(recs) != 2 || recs[0].IP != ip3 || recs[1].IP != ip1 {
		t.Fatalf("snapshot order = %+v, want [%s %s]", recs, ip3, ip1)
	}
	got := recs[1]
	want := ActionRecord{
		IP:           ip1,
		FirstTrip:    t0.Add(5 * time.Second),
		LastTrip:     t0.Add(15 * time.Second),
		Refused:      map[ActionKind]int{ActionRoomCreate: 3},
		TotalRefused: 3,
		Allowed:      map[ActionKind]int{ActionRoomCreate: 2, ActionRoomJoin: 1},
		Usernames:    []string{"dave", "alice", "carol", "bob"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("record =\n%+v\nwant\n%+v", got, want)
	}

	// Allowed counts only what is still inside each window.
	clock.Advance(time.Minute)
	if got := l.Snapshot()[1]; got.Allowed != nil {
		t.Fatalf("allowed after the window passed = %v, want none", got.Allowed)
	}
}

func TestActionUsernamesBounded(t *testing.T) {
	l, _, _ := testLimiter(t, oneRate(ActionLobbyUpdate, 1, time.Hour))
	for i := 0; i < 10; i++ {
		l.Allow(ip1, ActionLobbyUpdate, fmt.Sprintf("user%d", i))
	}
	l.Allow(ip1, ActionLobbyUpdate, "user5") // moves to the front
	l.Allow(ip1, ActionLobbyUpdate, "")      // ignored
	want := []string{"user5", "user9", "user8", "user7", "user6", "user4", "user3", "user2"}
	if got := l.Snapshot()[0].Usernames; !reflect.DeepEqual(got, want) {
		t.Fatalf("usernames = %v, want %v", got, want)
	}
}

func TestActionEviction(t *testing.T) {
	limits := oneRate(ActionRoomCreate, 1, time.Hour)
	limits.MaxIPs = 3
	l, clock, _ := testLimiter(t, limits)
	trip := func(ip string) {
		l.Allow(ip, ActionRoomCreate, "")
		l.Allow(ip, ActionRoomCreate, "")
	}
	trip("10.0.0.1") // tripped first
	clock.Advance(time.Second)
	trip("10.0.0.2") // tripped second
	clock.Advance(time.Second)
	l.Allow("10.0.0.3", ActionRoomCreate, "") // active, never tripped
	clock.Advance(time.Second)

	// Full: a new IP evicts the untripped entry before any report.
	trip("10.0.0.4")
	if ips := snapshotIPs(l); !reflect.DeepEqual(ips, []string{"10.0.0.4", "10.0.0.2", "10.0.0.1"}) {
		t.Fatalf("after first eviction: %v", ips)
	}
	l.mu.Lock()
	_, kept := l.ips["10.0.0.3"]
	l.mu.Unlock()
	if kept {
		t.Fatal("untripped entry survived eviction")
	}

	// Then the least recently tripped goes.
	clock.Advance(time.Second)
	trip("10.0.0.5")
	if ips := snapshotIPs(l); !reflect.DeepEqual(ips, []string{"10.0.0.5", "10.0.0.4", "10.0.0.2"}) {
		t.Fatalf("after second eviction: %v", ips)
	}
	// A re-trip refreshes an old record so it outlives newer ones.
	clock.Advance(time.Second)
	trip("10.0.0.2")
	clock.Advance(time.Second)
	trip("10.0.0.6")
	if ips := snapshotIPs(l); !reflect.DeepEqual(ips, []string{"10.0.0.6", "10.0.0.2", "10.0.0.5"}) {
		t.Fatalf("after third eviction: %v", ips)
	}
}

func snapshotIPs(l *ActionLimiter) []string {
	var ips []string
	for _, r := range l.Snapshot() {
		ips = append(ips, r.IP)
	}
	return ips
}

func TestActionPruneAndReportTTL(t *testing.T) {
	limits := oneRate(ActionRoomCreate, 1, time.Minute)
	limits.ReportTTL = time.Hour
	limits.BanStrikes = 2
	limits.BanDuration = 3 * time.Hour
	l, clock, _ := testLimiter(t, limits)

	l.Allow(ip2, ActionRoomCreate, "quiet") // never trips
	l.Allow(ip1, ActionRoomCreate, "a")
	l.Allow(ip1, ActionRoomCreate, "a") // trip

	const banned = "192.0.2.99"
	l.Allow(banned, ActionRoomCreate, "b")
	l.Allow(banned, ActionRoomCreate, "b")
	if _, _, ban := l.Allow(banned, ActionRoomCreate, "b"); !ban {
		t.Fatal("expected a ban")
	}

	prune := func() {
		l.mu.Lock()
		l.pruneLocked(clock.Now(), limits)
		l.mu.Unlock()
	}
	entries := func() int {
		l.mu.Lock()
		defer l.mu.Unlock()
		return len(l.ips)
	}

	clock.Advance(2 * time.Minute)
	prune()
	if n := entries(); n != 2 {
		t.Fatalf("after the window: %d entries, want 2 (the idle untripped one dropped)", n)
	}
	clock.Advance(time.Hour)
	prune()
	if ips := snapshotIPs(l); !reflect.DeepEqual(ips, []string{banned}) {
		t.Fatalf("after the TTL: %v, want only the still-banned IP", ips)
	}
	clock.Advance(2 * time.Hour)
	prune()
	if n := entries(); n != 0 {
		t.Fatalf("after the ban ran out: %d entries, want 0", n)
	}
}

func TestActionLoopback(t *testing.T) {
	cases := []struct {
		name    string
		exempt  bool
		ip      string
		wantCap bool
	}{
		{"default exempts IPv4 loopback", true, "127.0.0.1", false},
		{"default exempts IPv6 loopback", true, "::1", false},
		{"default limits public", true, ip1, true},
		{"disabled limits IPv4 loopback", false, "127.0.0.1", true},
		{"disabled limits IPv6 loopback", false, "::1", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			l, _, _ := testLimiter(t, oneRate(ActionRoomCreate, 2, time.Minute))
			l.SetExemptLoopback(c.exempt)
			allowed := 0
			for i := 0; i < 10; i++ {
				if ok, _, _ := l.Allow(c.ip, ActionRoomCreate, "a"); ok {
					allowed++
				}
			}
			if capped := allowed == 2; capped != c.wantCap {
				t.Fatalf("allowed %d of 10, want capped=%v", allowed, c.wantCap)
			}
			if !c.wantCap && len(l.Snapshot()) != 0 {
				t.Fatal("an exempt address got a report record")
			}
		})
	}
}

func TestActionExportImport(t *testing.T) {
	limits := oneRate(ActionRoomJoin, 1, time.Minute)
	limits.BanStrikes = 3
	limits.ReportTTL = 24 * time.Hour
	l, clock, _ := testLimiter(t, limits)
	for i := 0; i < 4; i++ {
		l.Allow(ip1, ActionRoomJoin, fmt.Sprintf("n%d", i%2))
	}
	clock.Advance(time.Minute)
	l.Allow(ip2, ActionRoomJoin, "z")
	l.Allow(ip2, ActionRoomJoin, "z")

	data, err := json.Marshal(l.Snapshot())
	if err != nil {
		t.Fatal(err)
	}
	var loaded []ActionRecord
	if err := json.Unmarshal(data, &loaded); err != nil {
		t.Fatal(err)
	}

	l2, clock2, _ := testLimiter(t, limits)
	clock2.Advance(clock.Now().Sub(clock2.Now()) + 2*time.Minute)
	l2.Import(loaded)
	got := l2.Snapshot()
	want := l.Snapshot()
	for i := range want {
		want[i].Allowed = nil // windows aren't persisted
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("round trip:\n got %+v\nwant %+v", got, want)
	}
	// Budgets start fresh after an import.
	if ok, _, _ := l2.Allow(ip1, ActionRoomJoin, "n0"); !ok {
		t.Fatal("imported IP has no budget left")
	}

	// A live record is kept over an imported one.
	l3, clock3, _ := testLimiter(t, limits)
	clock3.Advance(clock.Now().Sub(clock3.Now()) + 2*time.Minute)
	l3.Allow(ip1, ActionRoomJoin, "live")
	l3.Allow(ip1, ActionRoomJoin, "live")
	l3.Import(loaded)
	recs := l3.Snapshot()
	if len(recs) != 2 || recs[0].IP != ip1 || !reflect.DeepEqual(recs[0].Usernames, []string{"live"}) ||
		recs[0].TotalRefused != 1 || recs[1].IP != ip2 {
		t.Fatalf("import over a live record = %+v", recs)
	}

	// Records past ReportTTL are skipped (ip1's ban has run out by then too).
	l4, clock4, _ := testLimiter(t, limits)
	clock4.Advance(clock.Now().Sub(clock4.Now()) + 25*time.Hour)
	l4.Import(loaded)
	if recs := l4.Snapshot(); len(recs) != 0 {
		t.Fatalf("expired records imported: %+v", recs)
	}
}

func TestActionForget(t *testing.T) {
	l, _, _ := testLimiter(t, oneRate(ActionRoomCreate, 1, time.Hour))
	l.Allow(ip1, ActionRoomCreate, "a")
	l.Allow(ip1, ActionRoomCreate, "a")
	if !l.Forget(ip1) {
		t.Fatal("Forget reported no record")
	}
	if len(l.Snapshot()) != 0 {
		t.Fatal("record survived Forget")
	}
	if l.Forget(ip1) || l.Forget(ip2) {
		t.Fatal("Forget reported a record for an unknown IP")
	}
	if ok, _, _ := l.Allow(ip1, ActionRoomCreate, "a"); !ok {
		t.Fatal("budget survived Forget")
	}
}

func TestActionMemoryBounded(t *testing.T) {
	limits := DefaultActionLimits()
	limits.Rates = map[ActionKind]Rate{
		ActionRoomCreate: {5, time.Minute},
		ActionRoomJoin:   {1_000_000, time.Hour}, // clamped to maxTrackedEvents
	}
	limits.BanStrikes = 1_000_000 // clamped too
	limits.MaxIPs = 100
	l, _, _ := testLimiter(t, limits)

	// One IP flooding: stored timestamps stay at the limits.
	for i := 0; i < 20_000; i++ {
		l.Allow(ip1, ActionRoomCreate, "flood")
		l.Allow(ip1, ActionRoomJoin, "flood")
	}
	l.mu.Lock()
	e := l.ips[ip1]
	nCreate, nJoin, nStrikes := len(e.windows[ActionRoomCreate]), len(e.windows[ActionRoomJoin]), len(e.strikes)
	l.mu.Unlock()
	if nCreate != 5 || nJoin != maxTrackedEvents || nStrikes > maxTrackedEvents {
		t.Fatalf("stored create=%d join=%d strikes=%d, want 5, %d, <=%d",
			nCreate, nJoin, nStrikes, maxTrackedEvents, maxTrackedEvents)
	}

	// Many IPs: the table stays at MaxIPs.
	for i := 0; i < 5000; i++ {
		ip := fmt.Sprintf("10.%d.%d.%d", i/65536, (i/256)%256, i%256)
		l.Allow(ip, ActionRoomCreate, "")
	}
	l.mu.Lock()
	n := len(l.ips)
	l.mu.Unlock()
	if n > 100 {
		t.Fatalf("%d entries tracked, want at most MaxIPs=100", n)
	}
}

func TestActionConcurrent(t *testing.T) {
	limits := DefaultActionLimits()
	limits.Rates = map[ActionKind]Rate{
		ActionRoomCreate: {7, time.Hour},
		ActionRoomJoin:   {11, time.Hour},
	}
	limits.BanStrikes = 50
	l, clock, _ := testLimiter(t, limits)
	ips := []string{"203.0.113.1", "203.0.113.2", "203.0.113.3"}

	var mu sync.Mutex
	allowed := map[string]int{}
	bans := map[string]int{}
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				ip := ips[(g+i)%len(ips)]
				kind := ActionRoomCreate
				if i%2 == 1 {
					kind = ActionRoomJoin
				}
				ok, _, ban := l.Allow(ip, kind, fmt.Sprintf("g%d", g))
				mu.Lock()
				if ok {
					allowed[ip+"/"+string(kind)]++
				}
				if ban {
					bans[ip]++
				}
				mu.Unlock()
				switch i % 50 {
				case 10:
					_ = l.Snapshot()
				case 20:
					clock.Advance(time.Millisecond)
				case 30:
					l.mu.Lock()
					l.pruneLocked(clock.Now(), limits)
					l.mu.Unlock()
				}
			}
		}(g)
	}
	wg.Wait()
	for _, ip := range ips {
		if got := allowed[ip+"/room_create"]; got != 7 {
			t.Errorf("%s room_create allowed %d, want exactly 7", ip, got)
		}
		if got := allowed[ip+"/room_join"]; got != 11 {
			t.Errorf("%s room_join allowed %d, want exactly 11", ip, got)
		}
		if bans[ip] != 1 {
			t.Errorf("%s banned %d times, want 1", ip, bans[ip])
		}
	}
}
