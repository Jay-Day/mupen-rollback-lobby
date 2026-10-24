package ws

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jay-day/rmgk-lobby/internal/protocol"
	"github.com/jay-day/rmgk-lobby/internal/state"
)

// Quick match sessions: a continuous search starts again after each ranked
// set or casual match, with the same settings, and avoids the previous
// opponent. When only the previous opponent is available after
// qmRematchAfter, both are asked whether to play again or keep searching.
//
// In 2v2, a team whose players both keep their teammate searches again
// together and is matched against another team or two players on their own.
// The team splits when either player turns that off or stops searching.

var (
	// time to read the result before the room closes and the search resumes
	qmNextSetDelay = 8 * time.Second
	// how long a session searches before offering a rematch
	qmRematchAfter = 2 * time.Minute
	// how often a waiting session looks for a rematch to offer after that
	qmRematchRecheck = 30 * time.Second
)

func (h *Hub) pingToServer(uid uint64) uint16 {
	if u := h.state.GetUser(uid); u != nil {
		return u.PingToServer
	}
	return 0
}

// quickMatchSetEnded runs after a ranked set is finalized. For a quick-match
// room with a continuous session, it records the opponents and, after a
// moment to read the result, closes the room and searches again.
func (h *Hub) quickMatchSetEnded(set *pendingSet, teams [4]int) {
	ids := make([]uint64, 0, len(set.participants))
	for _, p := range set.participants {
		ids = append(ids, p.userID)
	}
	roomID, ok := h.matchmaker.roomOfUsers(ids)
	if !ok {
		return
	}
	// previous opponents: everyone in 1v1, the other team in 2v2
	opponents := make(map[uint64][]uint64, len(set.participants))
	teammates := make(map[uint64]uint64, len(set.participants))
	teamOf := func(p rankedParticipant) int {
		if set.settings.TeamSize > 1 && p.slot >= 1 && p.slot <= 4 {
			return teams[p.slot-1]
		}
		return 0
	}
	for _, p := range set.participants {
		for _, o := range set.participants {
			if o.userID == p.userID {
				continue
			}
			if teamOf(p) == 0 || teamOf(p) != teamOf(o) {
				opponents[p.userID] = append(opponents[p.userID], o.userID)
			} else {
				teammates[p.userID] = o.userID
			}
		}
	}
	continuing, kept := h.matchmaker.finishSet(roomID, opponents, teammates, set.quitter)
	if !continuing {
		return
	}
	if room := h.state.GetRoom(roomID); room != nil {
		h.sendRoomSystemChat(room, fmt.Sprintf("Set over. Searching for your next match in %d seconds.",
			int(qmNextSetDelay/time.Second)))
		for _, team := range kept {
			h.sendRoomSystemChat(room, fmt.Sprintf("%s and %s keep their team for the next set.",
				h.usernameOf(team[0]), h.usernameOf(team[1])))
		}
	}
	time.AfterFunc(qmNextSetDelay, func() { h.quickMatchNextSet(roomID) })
}

// quickMatchNextSet closes a finished quick-match room and searches again for
// everyone still in the session. Players who stopped the session stay in the
// room as before when nobody continues.
func (h *Hub) quickMatchNextSet(roomID uint64) {
	r := h.matchmaker.takeRoom(roomID)
	if r == nil {
		return // already handled (everyone left)
	}
	continuing := false
	for _, e := range r.entries {
		continuing = continuing || e.continuous
	}
	if !continuing {
		return
	}
	if room := h.state.GetRoom(roomID); room != nil {
		h.closeQuickMatchRoom(room, "quick_match_next")
	}
	h.searchAgainAll(r.entries)
}

// searchAgainAll searches again after a set for everyone in a finished
// quick-match room's session; the others' sessions end. A kept team whose
// other player doesn't search again splits.
func (h *Hub) searchAgainAll(entries map[uint64]qmEntry) {
	var stopped []uint64
	for uid, e := range entries {
		if e.continuous {
			if !h.searchAgain(uid, e, false) {
				stopped = append(stopped, uid)
			}
			continue
		}
		stopped = append(stopped, uid)
		if c := h.getClient(uid); c != nil {
			h.sendQuickMatchStatus(c, "", qmEntry{ranked: e.ranked}, false)
		}
	}
	for _, uid := range stopped {
		h.quickMatchSplitFrom(uid)
	}
}

// closeQuickMatchRoom returns every player to the lobby and removes the room
// and its match.
func (h *Hub) closeQuickMatchRoom(room *state.Room, reason string) {
	var matchID uint64
	seats := append([]*state.RoomSeat(nil), room.Players...)
	for _, seat := range seats {
		u := seat.User
		if u == nil {
			continue
		}
		if u.CurrentMatch != nil {
			matchID = u.CurrentMatch.ID
		}
		h.unseatUser(u, room)
		h.broadcastPresenceUpdated(u)
		sendRoomLeft(u, reason)
	}
	if matchID != 0 {
		if match := h.state.GetMatch(matchID); match != nil {
			match.SetState(state.MatchStateFinished)
		}
		h.state.RemoveMatch(matchID)
		h.broadcasts.endByMatch(matchID)
	}
	h.state.RemoveRoom(room.ID)
	h.broadcastRoomList()
	slog.Info("quick match room closed", "room", room.ID, "reason", reason)
}

// searchAgain queues a player's search again: after a set (with a fresh
// standing and the previous opponent avoided), or after a warmup that fell
// through (keepPlace: same search, same place in the queue). Reports whether
// the player is searching.
func (h *Hub) searchAgain(uid uint64, e qmEntry, keepPlace bool) bool {
	c := h.getClient(uid)
	if c == nil || c.user.CurrentRoom != nil {
		return false
	}
	if !keepPlace {
		e.enqueuedAt = time.Time{}
		if e.ranked != nil {
			e.standing = h.standing(c.user.AccountID, e.ranked.LadderID, e.ranked.RulesetID)
			if e.standing == nil {
				c.notice("warn", "Ratings are temporarily unavailable, so the ranked search stopped.")
				h.sendQuickMatchStatus(c, "", qmEntry{ranked: e.ranked}, false)
				return false
			}
		}
	}
	e.rematchOK, e.offered = false, false
	h.enqueueQuickMatch(c, e, "")
	return true
}

// quickMatchTeammateRequests tells a new search's players about teammate
// requests: a team formed, a request the other player hasn't made, or two
// requests for different sets.
func (h *Hub) quickMatchTeammateRequests(c *Client, r teammateRequests) {
	invite := func(from string) string {
		return fmt.Sprintf("%s wants you as their 2v2 teammate. To play together, search for a Quick Ranked "+
			"Match with %s as your teammate.", from, from)
	}
	if r.joined != 0 {
		slog.Info("quick match teammates found each other", "user", c.user.Username, "teammate", h.usernameOf(r.joined))
		c.notice("info", fmt.Sprintf("You and %s are searching as a team.", h.usernameOf(r.joined)))
		if oc := h.getClient(r.joined); oc != nil {
			oc.notice("info", fmt.Sprintf("You and %s are searching as a team.", c.user.Username))
			if e, ok := h.matchmaker.entry(r.joined); ok {
				h.sendQuickMatchStatus(oc, "", e, true)
			}
		}
	}
	if r.notAsked != 0 {
		if oc := h.getClient(r.notAsked); oc != nil {
			oc.notice("info", invite(c.user.Username))
		}
	}
	if r.otherSet != 0 {
		message := "%s wants you as their teammate too, but you're searching for different sets (format " +
			"or roster). Search for the same one to play together."
		c.notice("warn", fmt.Sprintf(message, h.usernameOf(r.otherSet)))
		if oc := h.getClient(r.otherSet); oc != nil {
			oc.notice("warn", fmt.Sprintf(message, c.user.Username))
		}
	}
	for _, id := range r.askedBy {
		c.notice("info", invite(h.usernameOf(id)))
	}
}

// quickMatchSplitFrom splits the kept team of a player who stopped searching
// with it: their teammate is told and searches on their own.
func (h *Hub) quickMatchSplitFrom(gone uint64) {
	teammate := h.matchmaker.splitFrom(gone)
	if teammate == 0 {
		return
	}
	c := h.getClient(teammate)
	if c == nil {
		return
	}
	slog.Info("quick match team split", "user", c.user.Username, "teammate", h.usernameOf(gone))
	name := h.usernameOf(gone)
	if name == "" {
		name = "Your teammate"
	}
	if e, ok := h.matchmaker.entry(teammate); ok && e.waiting() {
		// a team that asked for each other and hasn't played yet: still
		// only together
		c.notice("info", name+" stopped searching. Your search waits until they search with you as their teammate again.")
	} else {
		c.notice("info", name+" isn't searching with you anymore, so you'll search on your own.")
	}
	if e, ok := h.matchmaker.entry(teammate); ok {
		h.sendQuickMatchStatus(c, "", e, true)
		h.quickMatchAttempt(c.user)
	} else if e, ok := h.matchmaker.find(teammate); ok {
		h.sendQuickMatchStatus(c, "", e, false)
	}
}

// quickMatchRoomLeft follows up a player leaving a quick-match room (see
// removeUserFromRoom): a warmup that fell through sends everyone else back to
// the queue; after the set, leaving early just moves on to the next search;
// quitting mid-set ends the quitter's session.
func (h *Hub) quickMatchRoomLeft(u *state.User, room *state.Room, left qmLeave, dissolved bool) {
	search, hadSearch := left.search, left.hadSearch
	switch {
	case left.warmup:
		r := h.matchmaker.takeRoom(room.ID)
		if r == nil {
			return
		}
		for uid, e := range r.entries {
			message := u.Username + " left before the match started. Searching again."
			if e.partner == u.ID {
				// their kept teammate left: on their own from here
				e.partner = 0
				message = u.Username + " left before the match started. Searching again on your own."
			}
			if c := h.getClient(uid); c != nil {
				c.notice("info", message)
			}
			h.searchAgain(uid, e, true)
		}
		if hadSearch && search.continuous {
			if c := h.getClient(u.ID); c != nil {
				h.sendQuickMatchStatus(c, "", qmEntry{ranked: search.ranked}, false)
			}
		}
	case left.setDone:
		// leaving early moves on to the next search; a kept teammate still in
		// the room joins it when the room closes
		if !hadSearch || !search.continuous || !h.searchAgain(u.ID, search, false) {
			h.quickMatchSplitFrom(u.ID)
		}
		if dissolved {
			// the room is gone: everyone else moves on now too
			if r := h.matchmaker.takeRoom(room.ID); r != nil {
				h.searchAgainAll(r.entries)
			}
		}
	case room.GetRanked() == nil:
		// a casual match is over when a player leaves or disconnects; the
		// others move on, and so does the leaver if still connected
		h.quickMatchCasualOver(room, u.ID)
		if hadSearch && search.continuous {
			h.searchAgain(u.ID, search, false)
		}
	default:
		// left mid-set: the session ends
		if hadSearch && search.continuous {
			if c := h.getClient(u.ID); c != nil {
				h.sendQuickMatchStatus(c, "", qmEntry{ranked: search.ranked}, false)
			}
		}
		if dissolved {
			h.matchmaker.takeRoom(room.ID)
		}
	}
}

// quickMatchCasualOver ends a casual quick match once a player closes the
// game, leaves or disconnects (gone, already out of the room). Players with a
// session search again, avoiding each other, as after a ranked set.
func (h *Hub) quickMatchCasualOver(room *state.Room, gone uint64) {
	if room.GetRanked() != nil {
		return // ranked sets end with their result
	}
	ids := h.matchmaker.roomUsers(room.ID)
	if gone != 0 {
		ids = append(ids, gone)
	}
	opponents := make(map[uint64][]uint64, len(ids))
	for _, id := range ids {
		for _, o := range ids {
			if o != id {
				opponents[id] = append(opponents[id], o)
			}
		}
	}
	if continuing, _ := h.matchmaker.finishSet(room.ID, opponents, nil, 0); !continuing {
		return
	}
	if h.state.GetRoom(room.ID) == nil {
		// the room is already gone (its host left): move on now
		if r := h.matchmaker.takeRoom(room.ID); r != nil {
			for uid, e := range r.entries {
				if e.continuous {
					h.searchAgain(uid, e, false)
				}
			}
		}
		return
	}
	h.sendRoomSystemChat(room, fmt.Sprintf("Match over. Searching for your next match in %d seconds.",
		int(qmNextSetDelay/time.Second)))
	roomID := room.ID
	time.AfterFunc(qmNextSetDelay, func() { h.quickMatchNextSet(roomID) })
}

// quickMatchTooFar checks a ranked quick match's measured pings against each
// player's max ping before it starts. When a pair is over a limit, the room
// closes and everyone searches again in their old place; that pair is never
// matched again during the search.
func (h *Hub) quickMatchTooFar(room *state.Room) bool {
	if room.GetRanked() == nil {
		return false
	}
	searches := h.matchmaker.roomSearches(room.ID)
	ids := make([]uint64, 0, len(searches))
	for id := range searches {
		ids = append(ids, id)
	}
	type pair struct {
		a, b       uint64
		rtt, limit int
	}
	var far []pair
	for i, a := range ids {
		for _, b := range ids[:i] {
			rtt, ok := room.PeerPing(a, b)
			if !ok {
				continue // not measured: the region estimate already passed
			}
			for _, limit := range []int{searches[a].ranked.MaxPing, searches[b].ranked.MaxPing} {
				if limit > 0 && rtt > limit {
					far = append(far, pair{a, b, rtt, limit})
					break
				}
			}
		}
	}
	if len(far) == 0 {
		return false
	}
	r := h.matchmaker.takeRoom(room.ID)
	if r == nil {
		return true
	}
	for _, p := range far {
		for _, side := range [][2]uint64{{p.a, p.b}, {p.b, p.a}} {
			e := r.entries[side[0]]
			if e.tooFar == nil {
				e.tooFar = map[uint64]bool{}
			}
			e.tooFar[side[1]] = true
			if e.partner == side[1] {
				e.partner = 0 // a kept team too far apart to play: on their own
			}
			r.entries[side[0]] = e
		}
	}
	h.closeQuickMatchRoom(room, "quick_match_cancelled")
	name := func(id uint64) string {
		if u := h.state.GetUser(id); u != nil {
			return u.Username
		}
		return "a player"
	}
	slog.Info("ranked quick match cancelled: ping over the limit", "room", room.ID,
		"players", name(far[0].a)+","+name(far[0].b), "rtt", far[0].rtt, "limit", far[0].limit)
	for uid, e := range r.entries {
		if c := h.getClient(uid); c != nil {
			p := far[0]
			c.notice("info", fmt.Sprintf("The connection between %s and %s measured %d ms, over the %d ms limit. Searching again.",
				name(p.a), name(p.b), p.rtt, p.limit))
		}
		h.searchAgain(uid, e, true)
	}
	return true
}

// scheduleRematchCheck looks for a rematch to offer once a session has
// searched for `after` without a new opponent.
func (h *Hub) scheduleRematchCheck(uid uint64, enqueuedAt time.Time, after time.Duration) {
	time.AfterFunc(after, func() { h.quickMatchRematchCheck(uid, enqueuedAt) })
}

// quickMatchRematchCheck offers a rematch when only previous opponents would
// complete a match. The previous opponent gets the same offer once they have
// searched as long.
func (h *Hub) quickMatchRematchCheck(uid uint64, enqueuedAt time.Time) {
	e, ok := h.matchmaker.entry(uid)
	if !ok || !e.enqueuedAt.Equal(enqueuedAt) || len(e.avoid) == 0 || e.rematchOK || e.offered {
		return // matched, cancelled, re-searched, or already asked
	}
	u := h.state.GetUser(uid)
	c := h.getClient(uid)
	if u == nil || c == nil {
		return
	}
	group := h.matchmaker.pickGroup(uid, u.PingToServer, h.pingToServer, true)
	var names []string
	for _, id := range group {
		if e.avoid[id] {
			if o := h.state.GetUser(id); o != nil {
				names = append(names, o.Username)
			}
		}
	}
	if len(names) == 0 {
		// nobody to offer yet; look again shortly
		h.scheduleRematchCheck(uid, enqueuedAt, qmRematchRecheck)
		return
	}
	if !h.matchmaker.update(uid, func(e *qmEntry) { e.offered = true }) {
		return
	}
	slog.Info("quick match rematch offered", "user", u.Username, "opponents", strings.Join(names, ","))
	c.sendEnvelope(protocol.TypeQuickMatchRematchOffer, "", protocol.QuickMatchRematchOfferPayload{Opponents: names})
}

func (h *Hub) handleQuickMatchRematch(c *Client, env protocol.Envelope) {
	var req protocol.QuickMatchRematchRequest
	if err := json.Unmarshal(env.Data, &req); err != nil {
		return
	}
	e, ok := h.matchmaker.entry(c.user.ID)
	if !ok || !e.offered {
		return
	}
	h.matchmaker.update(c.user.ID, func(e *qmEntry) {
		e.offered = false
		e.rematchOK = req.Accept
	})
	slog.Info("quick match rematch answered", "user", c.user.Username, "accept", req.Accept)
	if !req.Accept {
		// tell previous opponents who said yes, and ask again later
		for id := range e.avoid {
			if o, ok := h.matchmaker.entry(id); ok && o.rematchOK {
				if oc := h.getClient(id); oc != nil {
					oc.notice("info", c.user.Username+" is searching for a new opponent.")
				}
			}
		}
		h.scheduleRematchCheck(c.user.ID, e.enqueuedAt, qmRematchAfter)
		return
	}
	h.quickMatchAttempt(c.user)
	if h.matchmaker.contains(c.user.ID) {
		c.notice("info", "Waiting for your previous opponent to answer.")
	}
}

// handleQuickMatchSession turns "keep searching after each set" on or off,
// e.g. "stop after this set" from inside the room.
func (h *Hub) handleQuickMatchSession(c *Client, env protocol.Envelope) {
	var req protocol.QuickMatchSessionRequest
	if err := json.Unmarshal(env.Data, &req); err != nil {
		return
	}
	e, ok := h.matchmaker.setContinuous(c.user.ID, req.Continuous)
	if !ok {
		return
	}
	slog.Info("quick match session changed", "user", c.user.Username, "continuous", req.Continuous)
	searching := h.matchmaker.contains(c.user.ID)
	h.sendQuickMatchStatus(c, env.ID, e, searching)
	if !req.Continuous && !searching {
		// no search after this set: a kept teammate goes on alone
		h.quickMatchSplitFrom(c.user.ID)
	}
}

// handleQuickMatchTeammate turns a 2v2 session's "keep my teammate" on or
// off. Between sets this keeps or splits the last set's team at once;
// during a set it applies when the set ends.
func (h *Hub) handleQuickMatchTeammate(c *Client, env protocol.Envelope) {
	var req protocol.QuickMatchTeammateRequest
	if err := json.Unmarshal(env.Data, &req); err != nil {
		return
	}
	e, ok, joined, split := h.matchmaker.setKeepTeammate(c.user.ID, req.Keep)
	if !ok {
		return
	}
	slog.Info("quick match keep teammate changed", "user", c.user.Username, "keep", e.keepTeammate,
		"joined", h.usernameOf(joined), "split", h.usernameOf(split))
	searching := h.matchmaker.contains(c.user.ID)
	h.sendQuickMatchStatus(c, env.ID, e, searching)

	// tell the teammate, and match whoever is searching with their new team
	teammate := joined
	if teammate == 0 {
		teammate = split
	}
	if teammate == 0 {
		return
	}
	oc := h.getClient(teammate)
	if oc != nil {
		if te, ok := h.matchmaker.find(teammate); ok {
			h.sendQuickMatchStatus(oc, "", te, h.matchmaker.contains(teammate))
		}
	}
	if joined != 0 {
		message := fmt.Sprintf("%s and %s keep their team for the next set.", c.user.Username, h.usernameOf(joined))
		room := c.user.CurrentRoom
		if room != nil {
			h.sendRoomSystemChat(room, message)
		} else {
			c.notice("info", message)
		}
		if oc != nil && (room == nil || oc.user.CurrentRoom != room) {
			oc.notice("info", message)
		}
	} else if oc != nil {
		oc.notice("info", c.user.Username+" isn't keeping the team, so you'll search on your own.")
	}
	if searching {
		h.quickMatchAttempt(c.user)
	}
	if oc != nil && h.matchmaker.contains(teammate) {
		h.quickMatchAttempt(oc.user)
	}
}
