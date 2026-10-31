package ws

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jay-day/rmgk-lobby/internal/geo"
	"github.com/jay-day/rmgk-lobby/internal/protocol"
	"github.com/jay-day/rmgk-lobby/internal/state"
)

const (
	qmDefaultDelay      = 2
	qmDefaultPrediction = 7
	qmDefaultPacing     = 1 // smooth/asymmetric — the only model current clients run
	qmDefaultMaxPlayers = 2
)

// qmEntry is a queued searcher: when they enqueued, the ROM they want and,
// for a ranked search, the set they want and their standing for it.
type qmEntry struct {
	userID     uint64
	region     string // lobby region, for the max-ping estimate
	enqueuedAt time.Time
	rom        protocol.RomInfo
	ranked     *state.RankedSettings    // nil = casual
	standing   *protocol.LadderStanding // on ranked's ladder and roster
	// continuous: search again after each set or match
	continuous bool
	// avoid: the previous set's opponents, matched again only once this
	// player accepts a rematch (rematchOK)
	avoid     map[uint64]bool
	rematchOK bool
	// tooFar: players whose measured ping was over a limit; never matched
	// again during this search or session
	tooFar  map[uint64]bool
	offered bool // a rematch offer is waiting for an answer
	// keepTeammate (2v2 sessions): search with the last set's teammate again
	// when they keep the team too
	keepTeammate bool
	// lastTeammate: the last finished set's teammate (0 after a 1v1)
	lastTeammate uint64
	// partner: the kept teammate this search goes with (0 = on your own).
	// A kept team is only ever matched together (see pickGroup).
	partner uint64
	// username: the searcher's name, for teammate requests
	username string
	// wantTeammate (2v2): the player this search asked for as its teammate.
	// It isn't matched until that player searches for the same set asking
	// for this one; then the two become partners. Only the session's first
	// match: later sets keep the team with keepTeammate.
	wantTeammate string
}

// waiting reports whether a search waits for the teammate it asked for.
func (e qmEntry) waiting() bool {
	return e.wantTeammate != "" && e.partner == 0
}

// samePool reports whether two searchers want the same game: the same ROM,
// and either both casual or both ranked on the same ladder and roster. Set
// length and skill aren't choices, so they don't split the queue.
func (e qmEntry) samePool(o qmEntry) bool {
	if !strings.EqualFold(e.rom.MD5, o.rom.MD5) || (e.ranked == nil) != (o.ranked == nil) {
		return false
	}
	return e.ranked == nil || (e.ranked.LadderID == o.ranked.LadderID &&
		e.ranked.RulesetID == o.ranked.RulesetID)
}

// accepts reports whether e's max ping lets o play, with the same rule a
// ranked room applies to joiners (see rankedEligibility).
func (e qmEntry) accepts(o qmEntry) bool {
	if e.ranked == nil {
		return true
	}
	estimate := geo.EstimatedRTT(e.region, o.region)
	return e.ranked.MaxPing == 0 || estimate <= e.ranked.MaxPing
}

// blocks reports whether e won't play o yet: o was e's previous opponent and
// e hasn't accepted a rematch.
func (e qmEntry) blocks(o qmEntry) bool {
	return e.avoid[o.userID] && !e.rematchOK
}

func (e qmEntry) compatible(o qmEntry) bool {
	return e.compatibleIgnoringOpponents(o) && !e.blocks(o) && !o.blocks(e)
}

// compatibleIgnoringOpponents is compatible without the previous-opponent
// rule, to find rematches worth offering.
func (e qmEntry) compatibleIgnoringOpponents(o qmEntry) bool {
	return e.samePool(o) && e.accepts(o) && o.accepts(e) && !e.tooFar[o.userID] && !o.tooFar[e.userID]
}

// players is how many searchers one match takes.
func (e qmEntry) players() int {
	if e.ranked == nil {
		return qmDefaultMaxPlayers
	}
	return e.ranked.Players()
}

// matchmaker tracks users searching for a quick match.
// Pairing rule: same pool (see samePool) and mutually accepted skill, then
// lowest combined pingToServer, ties FIFO.
type matchmaker struct {
	mu    sync.Mutex
	queue map[uint64]qmEntry // userId → entry
	rooms map[uint64]*qmRoom // roomId → the searches that made the room
}

// qmRoom remembers how each player of a quick-match room was searching, to
// search again for them when the set ends or someone leaves the warmup.
type qmRoom struct {
	entries map[uint64]qmEntry
	started bool // the match started (the warmup is over)
	setDone bool
}

func newMatchmaker() *matchmaker {
	return &matchmaker{
		queue: make(map[uint64]qmEntry),
		rooms: make(map[uint64]*qmRoom),
	}
}

// add queues a search. A zero enqueuedAt means now; a requeued search may
// keep its original time and with it its place.
func (m *matchmaker) add(userID uint64, e qmEntry) qmEntry {
	m.mu.Lock()
	defer m.mu.Unlock()
	if existing, exists := m.queue[userID]; exists {
		return existing
	}
	e.userID = userID
	if e.enqueuedAt.IsZero() {
		e.enqueuedAt = time.Now()
	}
	m.queue[userID] = e
	return e
}

// update changes a queued search in place; false when not queued.
func (m *matchmaker) update(userID uint64, change func(*qmEntry)) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.queue[userID]
	if ok {
		change(&e)
		m.queue[userID] = e
	}
	return ok
}

func (m *matchmaker) addRoom(roomID uint64, entries map[uint64]qmEntry) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rooms[roomID] = &qmRoom{entries: entries}
}

// takeRoom forgets a quick-match room and returns its searches.
func (m *matchmaker) takeRoom(roomID uint64) *qmRoom {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := m.rooms[roomID]
	delete(m.rooms, roomID)
	return r
}

// roomOfUsers finds the quick-match room holding any of these users' searches.
func (m *matchmaker) roomOfUsers(userIDs []uint64) (uint64, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for roomID, r := range m.rooms {
		for _, id := range userIDs {
			if _, ok := r.entries[id]; ok {
				return roomID, true
			}
		}
	}
	return 0, false
}

// roomUsers lists the players whose searches made a quick-match room and who
// are still in it.
func (m *matchmaker) roomUsers(roomID uint64) []uint64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	var ids []uint64
	if r := m.rooms[roomID]; r != nil {
		for id := range r.entries {
			ids = append(ids, id)
		}
	}
	return ids
}

// roomSearches copies a quick-match room's searches.
func (m *matchmaker) roomSearches(roomID uint64) map[uint64]qmEntry {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := m.rooms[roomID]
	if r == nil {
		return nil
	}
	out := make(map[uint64]qmEntry, len(r.entries))
	for id, e := range r.entries {
		out[id] = e
	}
	return out
}

func (m *matchmaker) markStarted(roomID uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r := m.rooms[roomID]; r != nil {
		r.started = true
	}
}

// qmLeave describes a player leaving a quick-match room.
type qmLeave struct {
	search     qmEntry // the player's search, when hadSearch
	hadSearch  bool
	quickMatch bool // the room is a quick-match room
	warmup     bool // its match hadn't started
	setDone    bool // its set (ranked) or match (casual) is over
}

// leaveRoom drops a player's search from a quick-match room.
func (m *matchmaker) leaveRoom(roomID, userID uint64) qmLeave {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := m.rooms[roomID]
	if r == nil {
		return qmLeave{}
	}
	search, had := r.entries[userID]
	delete(r.entries, userID)
	return qmLeave{search: search, hadSearch: had, quickMatch: true, warmup: !r.started, setDone: r.setDone}
}

// finishSet marks a quick-match room's set as over: everyone's previous
// opponents and teammate are recorded for the next search, and the player
// who quit (if any) leaves the session. Teams whose players both keep each
// other and keep searching stay together. Reports whether anyone keeps
// searching, and the kept teams.
func (m *matchmaker) finishSet(roomID uint64, opponents map[uint64][]uint64, teammates map[uint64]uint64,
	quitter uint64) (continuing bool, kept [][2]uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := m.rooms[roomID]
	if r == nil || r.setDone {
		return false, nil
	}
	r.setDone = true
	delete(r.entries, quitter)
	for id, e := range r.entries {
		e.avoid = make(map[uint64]bool)
		for _, o := range opponents[id] {
			e.avoid[o] = true
		}
		e.lastTeammate, e.partner = teammates[id], 0
		r.entries[id] = e
		continuing = continuing || e.continuous
	}
	for id, e := range r.entries {
		t, ok := r.entries[e.lastTeammate]
		if !ok || id > e.lastTeammate {
			continue // no teammate left, or the team is handled from its other player
		}
		if e.keepTeammate && e.continuous && t.keepTeammate && t.continuous && t.lastTeammate == id {
			e.partner, t.partner = e.lastTeammate, id
			r.entries[id], r.entries[e.lastTeammate] = e, t
			kept = append(kept, [2]uint64{id, e.lastTeammate})
		}
	}
	return continuing, kept
}

// findLocked returns a session's search wherever it is: queued (room nil)
// or in a quick-match room. m.mu must be held.
func (m *matchmaker) findLocked(userID uint64) (qmEntry, *qmRoom, bool) {
	if e, ok := m.queue[userID]; ok {
		return e, nil, true
	}
	for _, r := range m.rooms {
		if e, ok := r.entries[userID]; ok {
			return e, r, true
		}
	}
	return qmEntry{}, nil, false
}

// storeLocked writes back a search found by findLocked. m.mu must be held.
func (m *matchmaker) storeLocked(userID uint64, e qmEntry, r *qmRoom) {
	if r == nil {
		m.queue[userID] = e
	} else {
		r.entries[userID] = e
	}
}

// find returns a session's search, queued or in a quick-match room.
func (m *matchmaker) find(userID uint64) (qmEntry, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, _, ok := m.findLocked(userID)
	return e, ok
}

// setKeepTeammate turns keeping the teammate on or off for a 2v2 session,
// queued or in a quick-match room. Between sets (searching, or in a room
// whose set is over) turning it on keeps the last set's team when the
// teammate keeps it too; turning it off splits a kept team. Returns the
// search, whether one was found, and the teammate the team was just kept
// with (joined) or split from (split).
func (m *matchmaker) setKeepTeammate(userID uint64, keep bool) (e qmEntry, found bool, joined, split uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, room, ok := m.findLocked(userID)
	if !ok || e.ranked == nil || e.ranked.TeamSize < 2 {
		return e, ok, 0, 0
	}
	e.keepTeammate = keep
	if !keep && e.partner != 0 {
		split = e.partner
		if p, pRoom, ok := m.findLocked(split); ok && p.partner == userID {
			p.partner = 0
			m.storeLocked(split, p, pRoom)
		}
		e.partner = 0
	}
	// between sets: searching now, or searching once the finished room closes
	betweenSets := func(s qmEntry, r *qmRoom) bool { return r == nil || (r.setDone && s.continuous) }
	if keep && e.partner == 0 && e.lastTeammate != 0 && betweenSets(e, room) {
		t, tRoom, ok := m.findLocked(e.lastTeammate)
		if ok && betweenSets(t, tRoom) && t.keepTeammate && t.partner == 0 && t.lastTeammate == userID {
			e.partner, t.partner = e.lastTeammate, userID
			joined = e.lastTeammate
			m.storeLocked(joined, t, tRoom)
		}
	}
	m.storeLocked(userID, e, room)
	return e, true, joined, split
}

// teammateRequests is what a search's teammate request found among the
// players searching.
type teammateRequests struct {
	joined   uint64   // asked each other: now partners
	notAsked uint64   // the wanted player, searching without asking back
	otherSet uint64   // asked back, but searching for a different set
	askedBy  []uint64 // players waiting for this one, who didn't ask for them
}

func (r teammateRequests) found() bool {
	return r.joined != 0 || r.notAsked != 0 || r.otherSet != 0 || len(r.askedBy) > 0
}

// matchTeammateRequests makes two searches that asked for each other as
// teammates, for the same set, partners.
func (m *matchmaker) matchTeammateRequests(userID uint64) teammateRequests {
	m.mu.Lock()
	defer m.mu.Unlock()
	var r teammateRequests
	e, ok := m.queue[userID]
	if !ok || e.partner != 0 {
		return r
	}
	for id, o := range m.queue {
		if id == userID {
			continue
		}
		wanted := e.wantTeammate != "" && strings.EqualFold(o.username, e.wantTeammate)
		asksBack := o.waiting() && strings.EqualFold(o.wantTeammate, e.username)
		switch {
		case wanted && asksBack && e.samePool(o):
			e.partner, o.partner = id, userID
			m.queue[userID], m.queue[id] = e, o
			r.joined = id
		case wanted && asksBack:
			r.otherSet = id
		case wanted && o.partner == 0:
			r.notAsked = id
		case asksBack:
			r.askedBy = append(r.askedBy, id)
		}
	}
	return r
}

// splitFrom splits the kept team of a player who stopped searching: their
// teammate, queued or in a quick-match room, goes on alone. Returns that
// teammate (0 = none).
func (m *matchmaker) splitFrom(gone uint64) uint64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e, r, ok := m.findLocked(gone); ok && e.partner != 0 {
		e.partner = 0
		m.storeLocked(gone, e, r)
	}
	for id, e := range m.queue {
		if e.partner == gone {
			e.partner = 0
			m.queue[id] = e
			return id
		}
	}
	for _, r := range m.rooms {
		for id, e := range r.entries {
			if e.partner == gone {
				e.partner = 0
				r.entries[id] = e
				return id
			}
		}
	}
	return 0
}

// setContinuous turns a player's session on or off, whether they're queued
// or in a quick-match room. Returns their search and whether one was found.
func (m *matchmaker) setContinuous(userID uint64, continuous bool) (qmEntry, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e, ok := m.queue[userID]; ok {
		e.continuous = continuous
		m.queue[userID] = e
		return e, true
	}
	for _, r := range m.rooms {
		if e, ok := r.entries[userID]; ok {
			e.continuous = continuous
			r.entries[userID] = e
			return e, true
		}
	}
	return qmEntry{}, false
}

// entry returns a queued user's search.
func (m *matchmaker) entry(userID uint64) (qmEntry, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.queue[userID]
	return e, ok
}

// romOf returns the ROM a queued user is searching for.
func (m *matchmaker) romOf(userID uint64) (protocol.RomInfo, bool) {
	e, ok := m.entry(userID)
	return e.rom, ok
}

func (m *matchmaker) remove(userID uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.queue, userID)
}

func (m *matchmaker) contains(userID uint64) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.queue[userID]
	return ok
}

func (m *matchmaker) size() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.queue)
}

// poolSize counts the searchers in the same pool as e, e included when queued.
func (m *matchmaker) poolSize(e qmEntry) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, o := range m.queue {
		if o.samePool(e) {
			n++
		}
	}
	return n
}

// pickGroup returns the searchers to put in a match with `searcherID` (not
// including them), or nil when there aren't enough yet. Everyone in the group
// accepts everyone else, except that a kept team's two players don't need to
// accept each other. Candidates are taken by lowest combined ping with the
// searcher, ties FIFO. Pings come from getPing(uid) (the user's pingToServer,
// or 0 if unknown). With ignoreOpponents the previous-opponent rule is
// skipped, to find rematches worth offering.
//
// A kept team (see qmEntry.partner) is matched together or not at all: it
// waits until both players are searching, and fills two seats at once.
func (m *matchmaker) pickGroup(searcherID uint64, searcherPing uint16, getPing func(uint64) uint16, ignoreOpponents bool) []uint64 {
	m.mu.Lock()
	defer m.mu.Unlock()

	me, ok := m.queue[searcherID]
	if !ok || me.waiting() {
		return nil
	}
	compatible := qmEntry.compatible
	if ignoreOpponents {
		compatible = qmEntry.compatibleIgnoringOpponents
	}
	side := []qmEntry{me}
	taken := map[uint64]bool{searcherID: true}
	if me.partner != 0 {
		partner, ok := m.queue[me.partner]
		if !ok || partner.partner != searcherID {
			return nil // the teammate isn't searching yet
		}
		side = append(side, partner)
		taken[me.partner] = true
	}

	// candidates: players on their own, and kept teams with both searching
	type candidate struct {
		members []qmEntry
		score   uint32
		since   time.Time
	}
	var candidates []candidate
	for id, e := range m.queue {
		if taken[id] || e.waiting() {
			continue
		}
		c := candidate{members: []qmEntry{e}, score: uint32(searcherPing) + uint32(getPing(id)), since: e.enqueuedAt}
		if e.partner != 0 {
			partner, ok := m.queue[e.partner]
			if !ok || partner.partner != id || taken[e.partner] {
				continue
			}
			taken[e.partner] = true
			c.members = append(c.members, partner)
			c.score = uint32(searcherPing) + (uint32(getPing(id))+uint32(getPing(e.partner)))/2
			if partner.enqueuedAt.Before(c.since) {
				c.since = partner.enqueuedAt
			}
		}
		taken[id] = true
		if getsAlong(compatible, side, c.members) {
			candidates = append(candidates, c)
		}
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].score != candidates[j].score {
			return candidates[i].score < candidates[j].score
		}
		return candidates[i].since.Before(candidates[j].since)
	})

	// the first candidates, in order, who fill the match and all get along
	// (when taking each one that fits works, that's what this finds)
	var fill func(from, need int, group []qmEntry) []qmEntry
	fill = func(from, need int, group []qmEntry) []qmEntry {
		if need == 0 {
			return group
		}
		for i := from; i < len(candidates); i++ {
			c := candidates[i]
			if len(c.members) > need || !getsAlong(compatible, group[len(side):], c.members) {
				continue
			}
			if found := fill(i+1, need-len(c.members), append(group[:len(group):len(group)], c.members...)); found != nil {
				return found
			}
		}
		return nil
	}
	group := fill(0, me.players()-len(side), side)
	if group == nil {
		return nil
	}
	ids := make([]uint64, 0, len(group)-1)
	for _, e := range group[1:] {
		ids = append(ids, e.userID)
	}
	return ids
}

// getsAlong reports whether every searcher in a and every one in b accept
// each other.
func getsAlong(compatible func(qmEntry, qmEntry) bool, a, b []qmEntry) bool {
	for _, x := range a {
		for _, y := range b {
			if !compatible(x, y) {
				return false
			}
		}
	}
	return true
}

// -- Hub integration --

func (h *Hub) handleQuickMatchJoin(c *Client, env protocol.Envelope) {
	if c.user.CurrentRoom != nil {
		// Already in a room — can't queue
		slog.Debug("QUICK_MATCH_JOIN rejected: in a room", "user", c.user.Username)
		return
	}

	var req protocol.QuickMatchJoinRequest
	if len(env.Data) > 0 {
		if err := json.Unmarshal(env.Data, &req); err != nil {
			slog.Warn("QUICK_MATCH_JOIN invalid payload", "user", c.user.Username, "err", err)
			return
		}
	}
	if strings.TrimSpace(req.Rom.MD5) == "" {
		// No ROM selected — can't scope the search, so don't queue.
		slog.Warn("QUICK_MATCH_JOIN rejected: no rom md5", "user", c.user.Username)
		return
	}

	if h.matchmaker.contains(c.user.ID) {
		return // already queued
	}

	entry := qmEntry{rom: req.Rom, continuous: req.Continuous, region: c.user.Region}
	if req.Ranked != nil && req.Ranked.Enabled {
		settings, reason, message := h.resolveRankedSettings(req.Rom.MD5, req.Ranked)
		if reason == "" {
			reason, message = h.rankedEligibility(c.user, settings, nil)
		}
		if reason == "" {
			entry.standing = h.standing(c.user.AccountID, settings.LadderID, settings.RulesetID)
			if entry.standing == nil {
				reason, message = protocol.ReasonRankedUnavailable, "Ratings are temporarily unavailable."
			}
		}
		if reason != "" {
			c.notice("warn", message)
			c.sendEnvelope(protocol.TypeQuickMatchStatus, env.ID, protocol.QuickMatchStatusPayload{Ranked: true})
			return
		}
		entry.ranked = settings
		if teammate := strings.TrimSpace(req.Teammate); teammate != "" && settings.TeamSize > 1 {
			name, problem := h.wantedTeammate(c.user, teammate)
			if problem != "" {
				c.notice("warn", problem)
				c.sendEnvelope(protocol.TypeQuickMatchStatus, env.ID, protocol.QuickMatchStatusPayload{Ranked: true})
				return
			}
			entry.wantTeammate = name
			// keeping the teammate from the start needs a chosen one; in
			// the room it can be turned on for any teammate
			entry.keepTeammate = req.KeepTeammate
		}
	}
	h.enqueueQuickMatch(c, entry, env.ID)
}

// wantedTeammate checks a teammate request: an account other than the
// searcher's own. Returns the account's name as registered, or why not.
func (h *Hub) wantedTeammate(u *state.User, name string) (string, string) {
	if strings.EqualFold(name, u.Username) {
		return "", "You can't pick yourself as your teammate."
	}
	if h.accounts == nil {
		return name, ""
	}
	account, err := h.accounts.AccountByName(name)
	if err != nil {
		return "", fmt.Sprintf("There's no account named %s to team up with.", name)
	}
	return account.Username, ""
}

// enqueueQuickMatch queues a search, tells the client and tries to match it.
// Used for new searches and for a ranked session searching again.
func (h *Hub) enqueueQuickMatch(c *Client, entry qmEntry, envID string) {
	entry.username = c.user.Username
	entry = h.matchmaker.add(c.user.ID, entry)
	if requests := h.matchmaker.matchTeammateRequests(c.user.ID); requests.found() {
		h.quickMatchTeammateRequests(c, requests)
		entry, _ = h.matchmaker.entry(c.user.ID)
	}
	c.user.SetState(state.UserStateSearching)
	h.broadcastPresenceUpdated(c.user)
	h.sendQuickMatchStatus(c, envID, entry, true)

	if entry.ranked != nil {
		slog.Info("ranked quick match joined", "user", c.user.Username, "rom", entry.rom.Name,
			"ladder", entry.ranked.LadderID, "ruleset", entry.ranked.RulesetID, "bestOf", entry.ranked.BestOf,
			"tier", entry.standing.Tier, "continuous", entry.continuous,
			"avoid", len(entry.avoid), "keepTeammate", entry.keepTeammate, "teammate", h.usernameOf(entry.partner),
			"wantTeammate", entry.wantTeammate,
			"pool", h.matchmaker.poolSize(entry))
	} else {
		slog.Info("quick match joined", "user", c.user.Username,
			"rom", entry.rom.Name, "md5", entry.rom.MD5, "pool", h.matchmaker.poolSize(entry))
	}
	if len(entry.avoid) > 0 {
		h.scheduleRematchCheck(c.user.ID, entry.enqueuedAt, qmRematchAfter)
	}
	h.quickMatchAttempt(c.user)
}

func (h *Hub) sendQuickMatchStatus(c *Client, envID string, entry qmEntry, searching bool) {
	waitingFor := ""
	if entry.waiting() {
		waitingFor = entry.wantTeammate
	}
	c.sendEnvelope(protocol.TypeQuickMatchStatus, envID, protocol.QuickMatchStatusPayload{
		Searching:    searching,
		QueueSize:    h.matchmaker.poolSize(entry),
		Ranked:       entry.ranked != nil,
		Continuous:   entry.continuous,
		KeepTeammate: entry.keepTeammate,
		Teammate:     h.usernameOf(entry.partner),
		WaitingFor:   waitingFor,
	})
}

// usernameOf names a user, "" for 0 or someone no longer connected.
func (h *Hub) usernameOf(userID uint64) string {
	if userID == 0 {
		return ""
	}
	if u := h.state.GetUser(userID); u != nil {
		return u.Username
	}
	return ""
}

func (h *Hub) handleQuickMatchCancel(c *Client, env protocol.Envelope) {
	entry, ok := h.matchmaker.entry(c.user.ID)
	if !ok {
		return
	}
	h.matchmaker.remove(c.user.ID)
	c.user.SetState(state.UserStateIdle)
	h.broadcastPresenceUpdated(c.user)

	c.sendEnvelope(protocol.TypeQuickMatchStatus, env.ID, protocol.QuickMatchStatusPayload{
		Searching: false,
		QueueSize: h.matchmaker.poolSize(entry),
		Ranked:    entry.ranked != nil,
	})
	slog.Info("quick match cancelled", "user", c.user.Username, "queue", h.matchmaker.size())
	h.quickMatchSplitFrom(c.user.ID)
}

// dequeueQuickMatch drops a user's quick-match queue entry when they enter a
// room by other means, and resets their client's search toggle. Without this
// the stale entry sits in the queue until pairing fires and corrupts their
// room state (see quickMatchAttempt).
func (h *Hub) dequeueQuickMatch(c *Client) {
	entry, ok := h.matchmaker.entry(c.user.ID)
	if !ok {
		return
	}
	h.matchmaker.remove(c.user.ID)
	c.sendEnvelope(protocol.TypeQuickMatchStatus, "", protocol.QuickMatchStatusPayload{
		Searching: false,
		QueueSize: h.matchmaker.poolSize(entry),
		Ranked:    entry.ranked != nil,
	})
	slog.Info("quick match dequeued: entered a room", "user", c.user.Username)
	h.quickMatchSplitFrom(c.user.ID)
}

// quickMatchAttempt runs the matching logic on behalf of `searcher`.
// If enough compatible searchers are queued (one opponent, or three other
// players for a ranked 2v2 set), they are removed from the queue and dropped
// into an auto-created room which then auto-starts.
func (h *Hub) quickMatchAttempt(searcher *state.User) {
	entry, ok := h.matchmaker.entry(searcher.ID)
	if !ok {
		return // no longer queued
	}

	others := h.matchmaker.pickGroup(searcher.ID, searcher.PingToServer, h.pingToServer, false)
	if len(others) == 0 {
		return // not enough compatible searchers yet
	}

	// A queued user who has since entered a room by other means is a stale
	// entry: seating them would overwrite their CurrentRoom pointer and orphan
	// their seat, leaving behind a room nobody can dissolve — the "ghost match
	// that sits in the list until a server restart" bug. Drop the stale side
	// and let the live side keep searching.
	if searcher.CurrentRoom != nil {
		slog.Warn("quick match dropped stale queue entry", "user", searcher.Username)
		h.matchmaker.remove(searcher.ID)
		h.quickMatchSplitFrom(searcher.ID)
		return
	}
	members := []*state.User{searcher}
	entries := []qmEntry{entry}
	for _, id := range others {
		u := h.state.GetUser(id)
		if u == nil || u.CurrentRoom != nil {
			if u != nil {
				slog.Warn("quick match dropped stale queue entry", "user", u.Username)
			}
			h.matchmaker.remove(id)
			h.quickMatchSplitFrom(id)
			return
		}
		e, ok := h.matchmaker.entry(id)
		if !ok {
			return
		}
		members = append(members, u)
		entries = append(entries, e)
	}

	// The earliest-enqueued player hosts. Their ROM names the room (everyone
	// matched on MD5) and, for a ranked set, their settings are the room's:
	// every other player passes their skill filter, which is what the start
	// check tests.
	// (on a tie, someone who was already waiting rather than the searcher)
	host := 0
	for i := 1; i < len(entries); i++ {
		if entries[i].enqueuedAt.Before(entries[host].enqueuedAt) ||
			(host == 0 && entries[i].enqueuedAt.Equal(entries[0].enqueuedAt)) {
			host = i
		}
	}
	members[0], members[host] = members[host], members[0]
	entries[0], entries[host] = entries[host], entries[0]
	hostUser, hostEntry := members[0], entries[0]

	// Pop everyone from the queue
	for _, u := range members {
		h.matchmaker.remove(u.ID)
	}

	// Tell every client they've stopped searching. Without this the client's
	// Quick Match button stays in its "Cancel" toggle state for the rest of the
	// session (it only clears on QUICK_MATCH_STATUS{searching:false} or
	// disconnect), so after the game ends and they leave the room, clicking
	// Quick Match sends a cancel instead of a new join and they can't requeue.
	// A continuous session stays on (Continuous) through the room.
	for i, u := range members {
		if cl := h.getClient(u.ID); cl != nil {
			h.sendQuickMatchStatus(cl, "", entries[i], false)
		}
	}

	names := make([]string, len(members))
	for i, u := range members {
		names[i] = u.Username
	}
	teams := quickMatchTeams(members, entries)
	room := &state.Room{
		ID:         h.state.NextRoomID(),
		Name:       quickMatchRoomName(hostEntry.ranked, names, teams),
		Host:       hostUser,
		RomName:    hostEntry.rom.Name,
		RomMD5:     hostEntry.rom.MD5,
		RomRegion:  hostEntry.rom.Region,
		MaxPlayers: hostEntry.players(),
		Delay:      qmDefaultDelay,
		Prediction: qmDefaultPrediction,
		Pacing:     qmDefaultPacing,
		// Quick Match opens on Auto delay and Default prediction for every
		// seat; each client publishes its resolved local pair during warmup.
		DelayAuto:      true,
		PredictionAuto: true,
		Visibility:     state.VisibilityInviteOnly,
		QuickMatch:     true,
		State:          state.RoomStateWaiting,
		CreatedAt:      time.Now(),
	}
	if hostEntry.ranked != nil {
		settings := *hostEntry.ranked
		// a chosen or kept team plays together: the module and the result
		// check hold the set to it
		settings.Teammates = nil
		for i, e := range entries {
			for _, o := range entries[i+1:] {
				if e.partner == o.userID && o.partner == e.userID {
					settings.Teammates = append(settings.Teammates, [2]uint64{e.userID, o.userID})
				}
			}
		}
		room.SetRanked(&settings)
	}

	slog.Info("quick match paired", "rom", hostEntry.rom.Name, "md5", hostEntry.rom.MD5,
		"ranked", hostEntry.ranked != nil, "players", strings.Join(names, ","), "room", room.ID)
	// Seat through the choke point: if a racing handler seated any user
	// between the check above and here, this refuses instead of corrupting
	// their membership — the room object is simply discarded.
	for i, u := range members {
		if err := h.seatUser(u, room, state.UserStateInRoom); err != nil {
			slog.Warn("quick match aborted: player could not be seated", "user", u.Username, "err", err)
			for _, seated := range members[:i] {
				h.unseatUser(seated, room)
			}
			return
		}
	}

	h.state.AddRoom(room)
	searches := make(map[uint64]qmEntry, len(members))
	for i, u := range members {
		e := entries[i]
		e.rematchOK, e.offered = false, false
		// a teammate request is for the first match: after it, the team
		// carries on only when both keep it
		e.wantTeammate = ""
		searches[u.ID] = e
	}
	h.matchmaker.addRoom(room.ID, searches)

	// Send ROOM_JOIN_OK to everyone so their UIs switch into the in-room view.
	payload := roomStatePayload(room)
	for _, u := range members {
		if cl := h.getClient(u.ID); cl != nil {
			cl.sendEnvelope(protocol.TypeRoomJoinOK, "", payload)
		}
	}

	// Punch every pair against each other immediately. The warmup below is
	// only long enough to be useful if a real ping lands inside it, and a
	// one-sided probe usually won't — so the Auto delay would fall back to the
	// region estimate on exactly the path that most needs a measurement.
	for i, u := range members {
		for _, peer := range members[:i] {
			if !(peer.ICESupported && u.ICESupported) {
				h.introduceTo(peer, u)
				h.introduceTo(u, peer)
			}
		}
	}

	// Don't start instantly. Give the clients a short warmup in the in-room
	// view: seats + live peer ping render, and the host's Auto delay resolves
	// from that ping and pushes ROOM_UPDATE_DELAY before the match locks in.
	// The match then auto-starts with the tuned delay.
	for _, u := range members {
		h.broadcastPresenceUpdated(u)
	}
	h.broadcastRoomState(room)
	h.broadcastRoomList()
	// The warmup (lobby config) must be long enough for both clients to render
	// the in-room view, probe each other, and publish their local rollback
	// settings.
	warmup := h.cfg().QuickMatchWarmup()
	h.sendRoomSystemChat(room,
		fmt.Sprintf("Match found! Measuring connection — game starts in %d seconds.",
			int(warmup/time.Second)))
	if teams != nil {
		h.sendRoomSystemChat(room, fmt.Sprintf("Teams: %s vs %s. Character select starts each team on its own "+
			"color; teammates must stay on the same team.", strings.Join(teams[0], " & "), strings.Join(teams[1], " & ")))
	}
	h.scheduleQuickMatchStart(room, warmup)
}

// quickMatchTeams splits a 2v2 group into its two teams when a kept team
// decides them: the kept team (first) against everyone else. nil when there's
// no kept team and the players pick teams in the game.
func quickMatchTeams(members []*state.User, entries []qmEntry) [][]string {
	if len(members) != 4 {
		return nil
	}
	for i, e := range entries {
		for j, o := range entries {
			if i < j && e.partner == o.userID && o.partner == e.userID {
				kept, rest := []string{members[i].Username, members[j].Username}, []string{}
				for k, u := range members {
					if k != i && k != j {
						rest = append(rest, u.Username)
					}
				}
				return [][]string{kept, rest}
			}
		}
	}
	return nil
}

// quickMatchRoomName: "Quick Match: a vs b", "Ranked Quick Match: a vs b",
// "Ranked Quick Match 2v2: a & b vs c & d" when a kept team decides the
// teams, or "Ranked Quick Match 2v2: a, b, c, d" (teams are picked in the game).
func quickMatchRoomName(ranked *state.RankedSettings, names []string, teams [][]string) string {
	switch {
	case ranked == nil:
		return "Quick Match: " + strings.Join(names, " vs ")
	case ranked.TeamSize > 1 && teams != nil:
		return fmt.Sprintf("Ranked Quick Match %dv%d: %s vs %s", ranked.TeamSize, ranked.TeamSize,
			strings.Join(teams[0], " & "), strings.Join(teams[1], " & "))
	case ranked.TeamSize > 1:
		return fmt.Sprintf("Ranked Quick Match %dv%d: %s", ranked.TeamSize, ranked.TeamSize, strings.Join(names, ", "))
	default:
		return "Ranked Quick Match: " + strings.Join(names, " vs ")
	}
}

// scheduleQuickMatchStart auto-starts the paired match after the warmup window,
// but only if the room is still valid — a player may have left during warmup,
// dissolving the room or emptying a seat.
func (h *Hub) scheduleQuickMatchStart(room *state.Room, warmup time.Duration) {
	roomID := room.ID
	time.AfterFunc(warmup, func() {
		r := h.state.GetRoom(roomID)
		if r == nil {
			return
		}
		_, _, _, players, _, st := r.Snapshot()
		if st != state.RoomStateWaiting || players < minRoomPlayers {
			slog.Info("quick match warmup aborted", "room", roomID, "state", st, "players", players)
			return
		}
		// A ranked match whose measured ping is over someone's limit doesn't
		// start; everyone searches again.
		if h.quickMatchTooFar(r) {
			return
		}
		// A ranked set still needs every player seated and eligible.
		if settings := r.GetRanked(); settings != nil {
			if problem := h.rankedStartCheck(r, settings); problem != "" {
				slog.Info("ranked quick match not started", "room", roomID, "reason", problem)
				h.sendRoomSystemChat(r, "The ranked set can't start: "+problem)
				return
			}
		}
		// host-only validation doesn't apply to a system-created room.
		h.matchmaker.markStarted(roomID)
		h.startMatchInRoom(r, "quick-match")
	})
}

// sendRoomSystemChat pushes an informational chat line to a room's members,
// attributed to a synthetic sender (userID 0) so it renders like a normal room
// message on every client without needing a dedicated system-message type.
func (h *Hub) sendRoomSystemChat(room *state.Room, message string) {
	h.broadcastToRoom(room, protocol.TypeChatMsg, protocol.ChatMsgPayload{
		Channel:      channelRoom,
		FromUserID:   0,
		FromUsername: "Quick Match",
		Message:      message,
		ServerTime:   nowMs(),
	})
}

// getClient returns the WS client for a given user id, or nil if not connected.
func (h *Hub) getClient(userID uint64) *Client {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.clients[userID]
}
