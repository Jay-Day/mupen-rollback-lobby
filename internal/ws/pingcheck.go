package ws

import (
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"github.com/jay-day/rmgk-lobby/internal/protocol"
	"github.com/jay-day/rmgk-lobby/internal/ratelimit"
	"github.com/jay-day/rmgk-lobby/internal/state"
)

// Ping checks: two PCs measure the real round trip between them over ICE at
// the moment they agree to connect, instead of trusting the region estimate.
// A probe ICE_SIGNAL hands the other side this PC's addresses, so the server
// relays one only between the two PCs of a live grant:
//   - a challenge grant, made when a challenge (or invite) goes out, while it
//     is open and pingCheckGrace after, so a last sample can still land;
//   - a join grant, made by PING_CHECK_REQUEST between someone who could join
//     a room and its host, for pingCheckJoinLife.
//
// Each side of a grant knows the other by one user id: the player it
// challenged, the one who challenged it, or the room's host. That may be a
// local player, so signals are routed by it and stamped with the id the
// receiver knows, and nobody learns which accounts share a PC. For the same
// reason two PCs share at most one grant, under one pair of ids: a second one
// under other ids would show both ids reaching the same addresses.

const (
	// PING_CHECK_REQUEST budget per connection. Each one pokes the room's host,
	// so it's tighter than the probe budget; a player clicking through a few
	// rooms stays well inside it.
	pingCheckBurst  = 8
	pingCheckWindow = 30 * time.Second

	// Probe signals one side of a grant may send. A check takes a description,
	// a few candidates and gathering_done, and about as much again per restart.
	maxProbeSignals = 200
)

// How long a join grant lasts, and how long a challenge grant outlives its
// challenge. Variables for tests.
var (
	pingCheckJoinLife = 15 * time.Second
	pingCheckGrace    = 5 * time.Second
)

// pingSide is one PC in a grant.
type pingSide struct {
	pc    *state.User // the connection's owner
	alias uint64      // the id the other side knows this PC by
	sent  int         // probe signals relayed from this side
}

type pingGrant struct {
	key   [2]uint64 // the two PCs' ids, lower first
	sides [2]pingSide
	// Open challenges keeping the grant alive. With none left, it ends at until.
	holds map[uint64]struct{}
	until time.Time
	timer *time.Timer
}

// side is the index of pc's side in g, or -1.
func (g *pingGrant) side(pc *state.User) int {
	for i := range g.sides {
		if g.sides[i].pc == pc {
			return i
		}
	}
	return -1
}

// pingCheckEnd is one PING_CHECK_END, sent once the manager's lock is let go.
type pingCheckEnd struct {
	to   *state.User // a PC
	peer uint64      // the id it knew the other PC by
}

// pingCheckManager holds the grants. Its timers end grants on their own
// goroutines, as the challenge timers do; they take mu, and send outside it.
type pingCheckManager struct {
	mu          sync.Mutex
	grants      map[[2]uint64]*pingGrant
	byChallenge map[uint64]*pingGrant
	notify      func([]pingCheckEnd) // tells the PCs; called without mu
}

func newPingCheckManager(notify func([]pingCheckEnd)) *pingCheckManager {
	return &pingCheckManager{
		grants:      make(map[[2]uint64]*pingGrant),
		byChallenge: make(map[uint64]*pingGrant),
		notify:      notify,
	}
}

func pingPairKey(a, b *state.User) [2]uint64 {
	if a.ID < b.ID {
		return [2]uint64{a.ID, b.ID}
	}
	return [2]uint64{b.ID, a.ID}
}

// open grants a's and b's PCs probes, held open by challengeID (0: none) and
// lasting at least life (0: no minimum). A pair that already has a grant
// keeps it, extended, when it names them by the same ids. Under other ids it
// reports false and grants nothing.
func (m *pingCheckManager) open(a, b pingSide, challengeID uint64, life time.Duration) bool {
	if a.pc == b.pc {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	key := pingPairKey(a.pc, b.pc)
	g := m.grants[key]
	if g == nil {
		g = &pingGrant{key: key, sides: [2]pingSide{a, b}, holds: make(map[uint64]struct{})}
		m.grants[key] = g
	} else if g.sides[g.side(a.pc)].alias != a.alias || g.sides[g.side(b.pc)].alias != b.alias {
		return false
	}
	if challengeID != 0 {
		g.holds[challengeID] = struct{}{}
		m.byChallenge[challengeID] = g
	}
	if until := time.Now().Add(life); life > 0 && until.After(g.until) {
		g.until = until
	}
	m.schedule(g)
	return true
}

// schedule arms g's timer for when it ends, or stops it while challenges hold
// g open. Call with mu held.
func (m *pingCheckManager) schedule(g *pingGrant) {
	if len(g.holds) > 0 {
		if g.timer != nil {
			g.timer.Stop()
		}
		return
	}
	wait := time.Until(g.until)
	if g.timer == nil {
		g.timer = time.AfterFunc(wait, func() { m.expire(g) })
	} else {
		g.timer.Reset(wait)
	}
}

// expire ends g if its time is up. A run that an extension overtook (it fired
// while the extension held mu) finds g held or not yet due, and leaves it.
func (m *pingCheckManager) expire(g *pingGrant) {
	m.mu.Lock()
	if m.grants[g.key] != g || len(g.holds) > 0 {
		m.mu.Unlock()
		return
	}
	if wait := time.Until(g.until); wait > 0 {
		g.timer.Reset(wait)
		m.mu.Unlock()
		return
	}
	ends := m.remove(g)
	m.mu.Unlock()
	m.notify(ends)
}

// remove forgets g and returns the PING_CHECK_ENDs for both its PCs. Call with
// mu held.
func (m *pingCheckManager) remove(g *pingGrant) []pingCheckEnd {
	delete(m.grants, g.key)
	for id := range g.holds {
		delete(m.byChallenge, id)
	}
	if g.timer != nil {
		g.timer.Stop()
	}
	return []pingCheckEnd{
		{to: g.sides[0].pc, peer: g.sides[1].alias},
		{to: g.sides[1].pc, peer: g.sides[0].alias},
	}
}

// release lets go of a challenge's hold on its grant: with no other hold left,
// the grant ends pingCheckGrace from now. Releasing twice, or a challenge that
// never had a grant, does nothing.
func (m *pingCheckManager) release(challengeID uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	g := m.byChallenge[challengeID]
	if g == nil {
		return
	}
	delete(m.byChallenge, challengeID)
	delete(g.holds, challengeID)
	if until := time.Now().Add(pingCheckGrace); until.After(g.until) {
		g.until = until
	}
	m.schedule(g)
}

// endFor ends every grant of pc's at once (its connection is gone), and
// returns the PING_CHECK_ENDs for the other PCs.
func (m *pingCheckManager) endFor(pc *state.User) []pingCheckEnd {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []pingCheckEnd
	for _, g := range m.grants {
		if g.side(pc) < 0 {
			continue
		}
		for _, end := range m.remove(g) {
			if end.to != pc {
				out = append(out, end)
			}
		}
	}
	return out
}

// endAlias ends at once every grant that names a PC by id (a local player who
// left the roster), and returns the PING_CHECK_ENDs for both PCs of each.
func (m *pingCheckManager) endAlias(id uint64) []pingCheckEnd {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []pingCheckEnd
	for _, g := range m.grants {
		if g.sides[0].alias == id || g.sides[1].alias == id {
			out = append(out, m.remove(g)...)
		}
	}
	return out
}

// route checks that from's PC may send a probe signal to target, which names
// to's PC, and counts it. It returns the id to stamp it with: the one to's PC
// knows from's by. ok is false without a grant naming to's PC as target, or
// once from's side has sent its share.
func (m *pingCheckManager) route(from, to *state.User, target uint64) (stamp uint64, ok bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	g := m.grants[pingPairKey(from, to)]
	if g == nil {
		return 0, false
	}
	mine, theirs := g.side(from), g.side(to)
	if mine < 0 || theirs < 0 || mine == theirs || g.sides[theirs].alias != target {
		return 0, false
	}
	if g.sides[mine].sent >= maxProbeSignals {
		return 0, false
	}
	g.sides[mine].sent++
	return g.sides[mine].alias, true
}

// sendPingCheckEnds tells each PC its ping check with a peer is over.
func sendPingCheckEnds(ends []pingCheckEnd) {
	for _, end := range ends {
		sendToUser(end.to, protocol.TypePingCheckEnd, protocol.PingCheckEndPayload{PeerUserID: end.peer})
	}
}

// pingCheckable reports whether u's PC can take part in a ping check.
func pingCheckable(u *state.User) bool {
	pc := u.Primary()
	return pc.PingCheck && pc.ICESupported
}

// allowPingCheck records a PING_CHECK_REQUEST and reports whether it's within
// the flood limit. Called only from this client's read loop.
func (c *Client) allowPingCheck(now time.Time) bool {
	cutoff := now.Add(-pingCheckWindow)
	kept := c.pingCheckTimes[:0]
	for _, t := range c.pingCheckTimes {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	c.pingCheckTimes = kept
	if len(c.pingCheckTimes) >= pingCheckBurst {
		return false
	}
	c.pingCheckTimes = append(c.pingCheckTimes, now)
	return true
}

// openChallengePingCheck grants a challenge's two PCs probes while it's open.
// It returns the probe target for each side, the ids the challenge already
// shows them (ToUserID for the sender, FromUserID for the recipient), or zeros
// when there's no grant. Call after the challenge is pending.
func (h *Hub) openChallengePingCheck(ch *challenge) (forSender, forRecipient uint64) {
	from, to := ch.from.user, ch.to.Primary()
	if from == to || !pingCheckable(from) || !pingCheckable(to) {
		return 0, 0
	}
	if !h.pingChecks.open(pingSide{pc: from, alias: from.ID}, pingSide{pc: to, alias: ch.to.ID}, ch.id, 0) {
		return 0, 0
	}
	// It may have ended while the grant was being made (its recipient's PC
	// dropped, say). Its end then let go of the grant, or came too early to
	// see it; either way nothing holds the grant once this lets go too.
	if !h.challenges.isPending(ch.id) {
		h.pingChecks.release(ch.id)
		return 0, 0
	}
	return ch.to.ID, from.ID
}

// endPingChecksFor ends pc's ping checks at once: its connection is gone.
func (h *Hub) endPingChecksFor(pc *state.User) {
	sendPingCheckEnds(h.pingChecks.endFor(pc.Primary()))
}

// endPingChecksAs ends at once the ping checks that know a PC by u, a local
// player leaving the roster.
func (h *Hub) endPingChecksAs(u *state.User) {
	sendPingCheckEnds(h.pingChecks.endAlias(u.ID))
}

// handlePingCheckRequest lets the requester measure the ping to a room's host
// before joining it. Nothing changes on a refusal, and the host only hears
// about a requester who could join: the probes hand both sides each other's
// addresses, so a password room needs its password here too.
func (h *Hub) handlePingCheckRequest(c *Client, env protocol.Envelope) {
	var req protocol.PingCheckRequest
	refuse := func(reason string) {
		c.sendEnvelope(protocol.TypePingCheckReply, env.ID, protocol.PingCheckReplyPayload{
			RoomID: req.RoomID, Reason: reason,
		})
	}
	if err := json.Unmarshal(env.Data, &req); err != nil {
		refuse(protocol.ReasonInvalidPayload)
		return
	}
	if !c.allowPingCheck(time.Now()) {
		refuse(protocol.ReasonTooFast)
		return
	}
	if !pingCheckable(c.user) {
		refuse(protocol.ReasonPingCheckUnavailable)
		return
	}
	if c.user.CurrentRoom != nil {
		refuse(protocol.ReasonAlreadyInRoom)
		return
	}
	room := h.state.GetRoom(req.RoomID)
	if room == nil {
		refuse(protocol.ReasonRoomNotFound)
		return
	}
	if reason := joinRefusal(c.user, room); reason != "" {
		refuse(reason)
		return
	}
	// The ping limit is what this measures; the rest of the limits apply.
	if reason := joinLimitsRefusal(c.user, room, 0, false); reason != "" {
		refuse(reason)
		return
	}
	_, _, _, players, maxPlayers, _ := room.Snapshot()
	if free := maxPlayers - players; free <= 0 {
		refuse(protocol.ReasonRoomFull)
		return
	} else if len(c.user.Group()) > free {
		refuse(protocol.ReasonNotEnoughSeats)
		return
	}
	if room.Password != "" {
		// This tells a right password from a wrong one as surely as ROOM_JOIN
		// does, so it spends the same budget, before the answer is known.
		if ok, _ := h.allowAction(c, ratelimit.ActionRoomJoin); !ok {
			refuse(protocol.ReasonTooFast)
			return
		}
		if req.Password != room.Password {
			refuse(protocol.ReasonWrongPassword)
			return
		}
	}
	if room.Host == nil {
		refuse(protocol.ReasonPingCheckUnavailable)
		return
	}
	host := room.Host.Primary()
	if host == c.user || !pingCheckable(host) || h.getClient(host.ID) == nil ||
		!h.pingChecks.open(pingSide{pc: c.user, alias: c.user.ID}, pingSide{pc: host, alias: host.ID}, 0, pingCheckJoinLife) {
		refuse(protocol.ReasonPingCheckUnavailable)
		return
	}

	slog.Debug("ping check granted", "user", c.user.Username, "room", room.ID, "host", host.Username)
	expires := pingCheckJoinLife.Milliseconds()
	// The host first, so its probe agent is likelier to be up when the
	// requester's first signal lands.
	sendToUser(host, protocol.TypePingCheckIncoming, protocol.PingCheckIncomingPayload{
		FromUserID: c.user.ID, ExpiresInMs: expires,
	})
	c.sendEnvelope(protocol.TypePingCheckReply, env.ID, protocol.PingCheckReplyPayload{
		RoomID: room.ID, OK: true, PeerUserID: host.ID, ExpiresInMs: expires,
	})
}

// relayProbeSignal passes a ping check's ICE signal to the PC its target names
// in a grant c's PC holds, stamped with the id that PC knows c's by. Anything
// else is dropped silently, like a bad room signal: an answer would say which
// ids share a PC.
func (h *Hub) relayProbeSignal(c *Client, id string, req protocol.ICESignalRequest) {
	if req.RoomID != 0 || req.TargetUserID == 0 || !pingCheckable(c.user) || !validICESignalRequest(req) {
		return
	}
	target := h.state.GetUser(req.TargetUserID)
	if target == nil {
		return
	}
	to := target.Primary()
	if to == c.user || !pingCheckable(to) {
		return
	}
	stamp, ok := h.pingChecks.route(c.user, to, req.TargetUserID)
	if !ok {
		return
	}
	targetClient := h.getClient(to.ID)
	if targetClient == nil {
		return
	}
	targetClient.sendEnvelope(protocol.TypeICESignal, id, protocol.ICESignalPayload{
		FromUserID: stamp,
		RoomID:     0,
		Kind:       req.Kind,
		Value:      req.Value,
		Generation: req.Generation,
		Probe:      true,
	})
}
