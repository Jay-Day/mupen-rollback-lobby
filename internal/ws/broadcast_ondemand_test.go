package ws

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"

	"github.com/jay-day/rmgk-lobby/internal/protocol"
	"github.com/jay-day/rmgk-lobby/internal/state"
)

// keyframeRig is one broadcast with a connected broadcaster, on demand or not.
type keyframeRig struct {
	t           *testing.T
	m           *broadcastManager
	b           *broadcast
	broadcaster *Client
	matchID     uint64
}

func newKeyframeRig(t *testing.T, onDemand bool, broadcasterQueue int) *keyframeRig {
	t.Helper()
	const matchID uint64 = 300
	broadcaster := &Client{user: &state.User{ID: 30, Username: "host"}, send: make(chan []byte, broadcasterQueue)}
	h := &Hub{clients: map[uint64]*Client{broadcaster.user.ID: broadcaster}}
	m := &broadcastManager{hub: h, dir: t.TempDir(), byMatch: make(map[uint64]*broadcast)}
	h.broadcasts = m
	if !m.begin(matchID, 1, broadcaster.user.ID, []uint64{broadcaster.user.ID}, "", onDemand) {
		t.Fatal("begin broadcast")
	}
	t.Cleanup(func() { m.endByMatch(matchID) })
	return &keyframeRig{t: t, m: m, b: m.byMatch[matchID], broadcaster: broadcaster, matchID: matchID}
}

// pastStartup moves the broadcast's start out of the window where frame 0
// still counts as a fresh start point.
func (r *keyframeRig) pastStartup() {
	r.b.mu.Lock()
	r.b.startedAt = time.Now().Add(-time.Hour)
	r.b.mu.Unlock()
}

func (r *keyframeRig) data(raw []byte, frame int) {
	r.m.data(r.broadcaster.user.ID, r.matchID, base64.StdEncoding.EncodeToString(raw), frame)
}

// uploadKeyframe sends a keyframe in chunks, as the broadcaster's client does.
func (r *keyframeRig) uploadKeyframe(frame int, data []byte, chunks int) {
	size := (len(data) + chunks - 1) / chunks
	for i := 0; i < chunks; i++ {
		end := (i + 1) * size
		if end > len(data) {
			end = len(data)
		}
		r.m.keyframe(r.broadcaster.user.ID, r.matchID, frame, i, chunks,
			base64.StdEncoding.EncodeToString(data[i*size:end]))
	}
}

// ageKeyframe makes the cached keyframe older than any keyframe interval.
func (r *keyframeRig) ageKeyframe() {
	r.b.mu.Lock()
	r.b.keyframeAt = time.Now().Add(-time.Hour)
	r.b.mu.Unlock()
}

// keyframeRequests drains the broadcaster's queue and counts the keyframe asks.
func (r *keyframeRig) keyframeRequests() int {
	r.t.Helper()
	n := 0
	for {
		select {
		case raw := <-r.broadcaster.send:
			var env protocol.Envelope
			if err := json.Unmarshal(raw, &env); err != nil {
				r.t.Fatalf("decode broadcaster envelope: %v", err)
			}
			if env.Type != protocol.TypeBroadcastKeyframeRequest {
				continue
			}
			var payload protocol.BroadcastKeyframeRequestPayload
			if err := json.Unmarshal(env.Data, &payload); err != nil || payload.MatchID != r.matchID {
				r.t.Fatalf("keyframe request %s for the wrong match (%v)", env.Data, err)
			}
			n++
		default:
			return n
		}
	}
}

// expireWait makes a waiting viewer's deadline pass; its pump notices on the
// next tick.
func (r *keyframeRig) expireWait(viewer *Client) {
	r.b.mu.Lock()
	sc := r.b.spectators[viewer.user.ID]
	if sc == nil || !sc.waitingKeyframe {
		r.b.mu.Unlock()
		r.t.Fatalf("%s isn't waiting for a keyframe", viewer.user.Username)
	}
	sc.waitDeadline = time.Now().Add(-time.Millisecond)
	r.b.mu.Unlock()
}

func newViewer(id uint64, name string) *Client {
	return &Client{user: &state.User{ID: id, Username: name}, send: make(chan []byte, 256)}
}

func testKrec(records int) []byte {
	krec := make([]byte, 400)
	copy(krec, "KRC1")
	for i := 0; i < records; i++ {
		krec = append(krec, krecRecord(i)...)
	}
	return krec
}

func krecRecord(i int) []byte { return []byte{0x12, 0x01, 0x00, byte(i)} }

// spectatorStream is what one viewer has received so far.
type spectatorStream struct {
	begin         bool
	keyframe      []byte
	keyframeFrame int
	krec          []byte
}

// read takes c's messages until done holds, failing on anything out of order:
// SPECTATE_BEGIN first and once, keyframe chunks before any data, and data at
// contiguous offsets.
func (s *spectatorStream) read(t *testing.T, c *Client, done func(*spectatorStream) bool) {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for !done(s) {
		select {
		case raw := <-c.send:
			var env protocol.Envelope
			if err := json.Unmarshal(raw, &env); err != nil {
				t.Fatalf("decode envelope: %v", err)
			}
			switch env.Type {
			case protocol.TypeSpectateBegin:
				if s.begin {
					t.Fatal("duplicate SPECTATE_BEGIN")
				}
				s.begin = true
			case protocol.TypeSpectateKeyframe:
				if !s.begin || len(s.krec) > 0 {
					t.Fatal("keyframe outside the begin/data boundary")
				}
				var payload protocol.SpectateKeyframePayload
				if err := json.Unmarshal(env.Data, &payload); err != nil {
					t.Fatal(err)
				}
				chunk, err := base64.StdEncoding.DecodeString(payload.Data)
				if err != nil {
					t.Fatal(err)
				}
				s.keyframe = append(s.keyframe, chunk...)
				s.keyframeFrame = payload.Frame
			case protocol.TypeSpectateData:
				if !s.begin {
					t.Fatal("data before SPECTATE_BEGIN")
				}
				var payload protocol.SpectateDataPayload
				if err := json.Unmarshal(env.Data, &payload); err != nil {
					t.Fatal(err)
				}
				if payload.Offset != int64(len(s.krec)) {
					t.Fatalf("data offset = %d, want %d", payload.Offset, len(s.krec))
				}
				chunk, err := base64.StdEncoding.DecodeString(payload.Data)
				if err != nil {
					t.Fatal(err)
				}
				s.krec = append(s.krec, chunk...)
			default:
				t.Fatalf("unexpected envelope %q", env.Type)
			}
		case <-deadline:
			t.Fatalf("timed out: begin %v, keyframe %d bytes, krec %d bytes", s.begin, len(s.keyframe), len(s.krec))
		}
	}
}

func hasBegun(s *spectatorStream) bool { return s.begin }

func hasKrec(n int) func(*spectatorStream) bool {
	return func(s *spectatorStream) bool { return len(s.krec) >= n }
}

// expectQuiet fails if c receives anything within d.
func expectQuiet(t *testing.T, c *Client, d time.Duration) {
	t.Helper()
	select {
	case raw := <-c.send:
		t.Fatalf("%s got %s while it should wait", c.user.Username, raw)
	case <-time.After(d):
	}
}

func TestOnDemandFreshKeyframeServedWithoutRequest(t *testing.T) {
	r := newKeyframeRig(t, true, 64)
	r.pastStartup()
	krec := testKrec(5)
	r.data(krec, 5)
	keyframe := bytes.Repeat([]byte{0x5A}, spectateDataChunkBytes+33)
	r.uploadKeyframe(5, keyframe, 2)
	r.keyframeRequests()

	viewer := newViewer(31, "viewer")
	r.m.spectateStart(viewer, r.matchID)
	var s spectatorStream
	s.read(t, viewer, hasKrec(400))
	if s.keyframeFrame != 5 || !bytes.Equal(s.keyframe, keyframe) {
		t.Fatalf("cached keyframe not served: frame %d, %d bytes", s.keyframeFrame, len(s.keyframe))
	}
	if !bytes.Equal(s.krec, krec[:400]) {
		t.Fatal("krec header differs")
	}
	r.data(krecRecord(5), 6)
	s.read(t, viewer, hasKrec(404))
	if !bytes.Equal(s.krec[400:], krecRecord(5)) {
		t.Fatal("live tail differs")
	}
	if n := r.keyframeRequests(); n != 0 {
		t.Fatalf("%d keyframe requests for a fresh cache, want 0", n)
	}
}

func TestOnDemandStaleKeyframeIsRequestedAndHoldsViewer(t *testing.T) {
	r := newKeyframeRig(t, true, 64)
	r.pastStartup()
	r.data(testKrec(3), 3)
	stale := bytes.Repeat([]byte{0x11}, 100)
	r.uploadKeyframe(2, stale, 1)
	r.ageKeyframe()
	r.keyframeRequests()

	viewer := newViewer(31, "viewer")
	r.m.spectateStart(viewer, r.matchID)
	if n := r.keyframeRequests(); n != 1 {
		t.Fatalf("%d keyframe requests, want 1", n)
	}
	var s spectatorStream
	s.read(t, viewer, hasBegun)
	expectQuiet(t, viewer, 100*time.Millisecond)

	// The match goes on while the broadcaster captures; the viewer still waits.
	r.data(append(krecRecord(3), krecRecord(4)...), 5)
	expectQuiet(t, viewer, 100*time.Millisecond)

	// A keyframe taken after the latest input points one record past the spool.
	fresh := bytes.Repeat([]byte{0x22}, 2*spectateDataChunkBytes+5)
	r.uploadKeyframe(5, fresh, 3)
	s.read(t, viewer, hasKrec(400))
	if s.keyframeFrame != 5 || !bytes.Equal(s.keyframe, fresh) {
		t.Fatalf("viewer got keyframe frame %d (%d bytes), want the new one", s.keyframeFrame, len(s.keyframe))
	}
	r.data(append(krecRecord(5), krecRecord(6)...), 7)
	s.read(t, viewer, hasKrec(408))
	want := append(append(testKrec(0), krecRecord(5)...), krecRecord(6)...)
	if !bytes.Equal(s.krec, want) {
		t.Fatalf("krec stream = %x, want %x", s.krec, want)
	}
	expectQuiet(t, viewer, 50*time.Millisecond)
	if n := r.keyframeRequests(); n != 0 {
		t.Fatalf("%d more keyframe requests after it arrived, want 0", n)
	}
}

func TestOnDemandJoinersShareOneRequest(t *testing.T) {
	r := newKeyframeRig(t, true, 64)
	r.pastStartup()
	krec := testKrec(4)
	r.data(krec, 4)

	viewers := []*Client{newViewer(31, "one"), newViewer(32, "two")}
	for _, v := range viewers {
		r.m.spectateStart(v, r.matchID)
	}
	if n := r.keyframeRequests(); n != 1 {
		t.Fatalf("%d keyframe requests for two joiners, want 1", n)
	}

	keyframe := bytes.Repeat([]byte{0x33}, 300)
	r.uploadKeyframe(4, keyframe, 1)
	for _, v := range viewers {
		var s spectatorStream
		s.read(t, v, hasKrec(400))
		if s.keyframeFrame != 4 || !bytes.Equal(s.keyframe, keyframe) || !bytes.Equal(s.krec, krec[:400]) {
			t.Fatalf("%s wasn't started from the requested keyframe", v.user.Username)
		}
	}

	// Within the interval, the next joiner gets it from the cache.
	late := newViewer(33, "late")
	r.m.spectateStart(late, r.matchID)
	var s spectatorStream
	s.read(t, late, hasKrec(400))
	if !bytes.Equal(s.keyframe, keyframe) {
		t.Fatal("late joiner didn't get the cached keyframe")
	}
	if n := r.keyframeRequests(); n != 0 {
		t.Fatalf("%d keyframe requests for a cached keyframe, want 0", n)
	}
}

func TestOnDemandWaitTimesOutToStaleKeyframe(t *testing.T) {
	r := newKeyframeRig(t, true, 64)
	r.pastStartup()
	krec := testKrec(6)
	r.data(krec, 6)
	stale := bytes.Repeat([]byte{0x44}, 200)
	r.uploadKeyframe(3, stale, 1)
	r.ageKeyframe()
	r.keyframeRequests()

	viewer := newViewer(31, "viewer")
	r.m.spectateStart(viewer, r.matchID)
	if n := r.keyframeRequests(); n != 1 {
		t.Fatalf("%d keyframe requests, want 1", n)
	}
	var s spectatorStream
	s.read(t, viewer, hasBegun)
	r.expireWait(viewer)

	tailStart := krecFrameOffset(krec, 3)
	want := append(append([]byte{}, krec[:400]...), krec[tailStart:]...)
	s.read(t, viewer, hasKrec(len(want)))
	if s.keyframeFrame != 3 || !bytes.Equal(s.keyframe, stale) || !bytes.Equal(s.krec, want) {
		t.Fatal("timed-out viewer wasn't started from the cached keyframe")
	}
	r.data(krecRecord(6), 7)
	s.read(t, viewer, hasKrec(len(want)+4))

	// While the ask is outstanding, another joiner doesn't repeat it; once it
	// has gone unanswered too long, the next joiner asks again.
	second := newViewer(32, "second")
	r.m.spectateStart(second, r.matchID)
	if n := r.keyframeRequests(); n != 0 {
		t.Fatalf("%d keyframe requests while one is outstanding, want 0", n)
	}
	r.m.spectateStop(second.user.ID, r.matchID)
	r.b.mu.Lock()
	r.b.kfRequestedAt = time.Now().Add(-keyframeRequestTimeout)
	r.b.mu.Unlock()
	third := newViewer(33, "third")
	r.m.spectateStart(third, r.matchID)
	if n := r.keyframeRequests(); n != 1 {
		t.Fatalf("%d keyframe requests after the last went unanswered, want 1", n)
	}
}

func TestOnDemandWaitTimesOutToFullReplay(t *testing.T) {
	r := newKeyframeRig(t, true, 64)
	r.pastStartup()
	krec := testKrec(2)
	r.data(krec, 2)

	viewer := newViewer(31, "viewer")
	r.m.spectateStart(viewer, r.matchID)
	if n := r.keyframeRequests(); n != 1 {
		t.Fatalf("%d keyframe requests, want 1", n)
	}
	var s spectatorStream
	s.read(t, viewer, hasBegun)
	r.expireWait(viewer)
	s.read(t, viewer, hasKrec(len(krec)))
	if len(s.keyframe) != 0 || !bytes.Equal(s.krec, krec) {
		t.Fatal("viewer without any keyframe wasn't given the whole spool")
	}

	// A keyframe that turns up afterwards is cached for later joiners and
	// changes nothing for this one.
	r.data(krecRecord(2), 3)
	r.uploadKeyframe(3, []byte{1, 2, 3}, 1)
	r.data(krecRecord(3), 4)
	s.read(t, viewer, hasKrec(len(krec)+8))
	if len(s.keyframe) != 0 || !bytes.Equal(s.krec[len(krec):], append(krecRecord(2), krecRecord(3)...)) {
		t.Fatal("late keyframe disturbed a running full replay")
	}
}

func TestOnDemandRetriesRequestTheBroadcasterQueueDropped(t *testing.T) {
	r := newKeyframeRig(t, true, 1)
	r.pastStartup()
	r.data(testKrec(2), 2)
	r.broadcaster.send <- []byte(`{"type":"FILLER"}`)

	viewer := newViewer(31, "viewer")
	r.m.spectateStart(viewer, r.matchID)
	<-r.broadcaster.send // the filler; the ask didn't fit behind it
	deadline := time.Now().Add(2 * time.Second)
	for r.keyframeRequests() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the waiting viewer's pump never asked again")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestOnDemandMatchStartReplaysFromFrameZero(t *testing.T) {
	r := newKeyframeRig(t, true, 64)
	krec := testKrec(3)
	r.data(krec, 3)

	viewer := newViewer(31, "viewer")
	r.m.spectateStart(viewer, r.matchID)
	var s spectatorStream
	s.read(t, viewer, hasKrec(len(krec)))
	if len(s.keyframe) != 0 || !bytes.Equal(s.krec, krec) {
		t.Fatal("early viewer wasn't given the whole spool")
	}
	if n := r.keyframeRequests(); n != 0 {
		t.Fatalf("%d keyframe requests in the match's first interval, want 0", n)
	}
}

func TestPeriodicBroadcastNeverRequestsKeyframes(t *testing.T) {
	r := newKeyframeRig(t, false, 64)
	r.pastStartup()
	krec := testKrec(4)
	r.data(krec, 4)

	first := newViewer(31, "first")
	r.m.spectateStart(first, r.matchID)
	var s spectatorStream
	s.read(t, first, hasKrec(len(krec)))
	if len(s.keyframe) != 0 || !bytes.Equal(s.krec, krec) {
		t.Fatal("viewer before any keyframe wasn't given the whole spool")
	}

	stale := bytes.Repeat([]byte{0x55}, 64)
	r.uploadKeyframe(2, stale, 1)
	r.ageKeyframe()
	second := newViewer(32, "second")
	r.m.spectateStart(second, r.matchID)
	var s2 spectatorStream
	tailStart := krecFrameOffset(krec, 2)
	want := append(append([]byte{}, krec[:400]...), krec[tailStart:]...)
	s2.read(t, second, hasKrec(len(want)))
	if s2.keyframeFrame != 2 || !bytes.Equal(s2.keyframe, stale) || !bytes.Equal(s2.krec, want) {
		t.Fatal("periodic broadcast didn't serve its latest keyframe, however old")
	}
	if n := r.keyframeRequests(); n != 0 {
		t.Fatalf("%d keyframe requests on a periodic broadcast, want 0", n)
	}
}

func TestBroadcastBeginTakesKeyframeModeFromBroadcaster(t *testing.T) {
	for _, onDemand := range []bool{false, true} {
		f := newLateJoinFixture(t, 2)
		f.user("host", true)
		f.user("guest", true)
		match := f.startMatch(2, "host", "guest")
		f.send("host", protocol.TypeBroadcastBegin, protocol.BroadcastBeginRequest{
			MatchID: match.ID, KeyframeOnDemand: onDemand,
		})
		b := f.h.broadcasts.byMatch[match.ID]
		if b == nil {
			t.Fatal("broadcast didn't begin")
		}
		if b.keyframeOnDemand != onDemand {
			t.Fatalf("keyframeOnDemand = %v, want %v", b.keyframeOnDemand, onDemand)
		}
		f.h.broadcasts.endByMatch(match.ID)
	}
}
