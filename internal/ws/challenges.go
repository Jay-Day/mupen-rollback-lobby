package ws

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/jay-day/rmgk-lobby/internal/protocol"
	"github.com/jay-day/rmgk-lobby/internal/state"
)

// Challenges: asking one player to play one game now. The sender's client shows
// a ticket and the recipient's a card, until CHALLENGE_RESULT ends it for both.
// Accepting makes a private room with exactly the seats both PCs need, hosted by
// the challenger. Sent from inside a room, the same thing is an invite to that
// room instead.
//
// A challenge stays between two connections: anything that would reveal how many
// players share a PC is reported to the sender as ChallengeUnavailable.

const (
	challengeNoteMaxRunes = 80
	// What a client says about a game reaches the recipient's card and the room list.
	romNameMaxRunes  = 128
	romFieldMaxRunes = 64
	// How many unanswered challenges one PC can have waiting.
	maxIncomingChallenges = 5
	// How long before asking again someone who said no.
	challengeCooldown = 60 * time.Second

	challengeBurst  = 6
	challengeWindow = time.Minute

	// A HELLO can't carry more than this many games worth comparing.
	maxRomHashes = 20000
)

// challengeAnswerTime is how long the recipient has. A variable for tests.
var challengeAnswerTime = 30 * time.Second

type challenge struct {
	id         uint64
	from       *Client
	to         *state.User // may be a local player
	rom        protocol.RomInfo
	note       string
	liveReplay bool
	roomID     uint64 // set when this is an invite to the sender's room
	timer      *time.Timer
}

type challengeManager struct {
	mu       sync.Mutex
	nextID   uint64
	pending  map[uint64]*challenge
	declined map[[2]uint64]time.Time // sender's connection and the player asked → when
}

func newChallengeManager() *challengeManager {
	return &challengeManager{
		pending:  make(map[uint64]*challenge),
		declined: make(map[[2]uint64]time.Time),
	}
}

// add registers ch and starts its answer timer, unless its sender already has one
// out or its recipient has too many waiting. It returns why not, or "".
func (m *challengeManager) add(ch *challenge, onTimeout func()) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	waiting := 0
	for _, other := range m.pending {
		if other.from.user == ch.from.user {
			return protocol.ChallengePending
		}
		if other.to.Primary() == ch.to.Primary() {
			waiting++
		}
	}
	if waiting >= maxIncomingChallenges {
		return protocol.ChallengeUnavailable
	}
	m.nextID++
	ch.id = m.nextID
	m.pending[ch.id] = ch
	ch.timer = time.AfterFunc(challengeAnswerTime, onTimeout)
	return ""
}

// take removes a pending challenge if allowed says it may, so whichever of an
// answer, a cancel and the timer comes first is the only one that acts on it.
func (m *challengeManager) take(id uint64, allowed func(*challenge) bool) *challenge {
	m.mu.Lock()
	defer m.mu.Unlock()
	ch := m.pending[id]
	if ch == nil || !allowed(ch) {
		return nil
	}
	delete(m.pending, id)
	if ch.timer != nil {
		ch.timer.Stop()
	}
	return ch
}

// takeWhere removes and returns every pending challenge matching keep.
func (m *challengeManager) takeWhere(keep func(*challenge) bool) []*challenge {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []*challenge
	for id, ch := range m.pending {
		if keep(ch) {
			delete(m.pending, id)
			if ch.timer != nil {
				ch.timer.Stop()
			}
			out = append(out, ch)
		}
	}
	return out
}

func (m *challengeManager) noteDeclined(from, to uint64, now time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for key, at := range m.declined {
		if now.Sub(at) >= challengeCooldown {
			delete(m.declined, key)
		}
	}
	m.declined[[2]uint64{from, to}] = now
}

func (m *challengeManager) inCooldown(from, to uint64, now time.Time) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	at, ok := m.declined[[2]uint64{from, to}]
	return ok && now.Sub(at) < challengeCooldown
}

// clipRunes cuts s to at most max runes.
func clipRunes(s string, max int) string {
	count := 0
	for i := range s {
		if count == max {
			return s[:i]
		}
		count++
	}
	return s
}

// romHashSet keeps a HELLO's game list for comparing libraries later.
func romHashSet(hashes []string) map[string]struct{} {
	if len(hashes) > maxRomHashes {
		hashes = hashes[:maxRomHashes]
	}
	out := make(map[string]struct{}, len(hashes))
	for _, md5 := range hashes {
		out[strings.ToUpper(md5)] = struct{}{}
	}
	return out
}

// allowChallenge records a CHALLENGE_SEND and reports whether it's within the
// flood limit. Called only from this client's read loop.
func (c *Client) allowChallenge(now time.Time) bool {
	cutoff := now.Add(-challengeWindow)
	kept := c.challengeTimes[:0]
	for _, t := range c.challengeTimes {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	c.challengeTimes = kept
	if len(c.challengeTimes) >= challengeBurst {
		return false
	}
	c.challengeTimes = append(c.challengeTimes, now)
	return true
}

// inviteRefusal reports whether target's PC can't be invited to room right now.
func inviteRefusal(target *state.User, room *state.Room) bool {
	if target.CurrentRoom == room {
		return true
	}
	_, _, _, players, maxPlayers, _ := room.Snapshot()
	group := target.Group()
	if len(group) > maxPlayers-players {
		return true
	}
	for _, u := range group {
		if room.WasKicked(u.ID, u.Username) {
			return true
		}
	}
	if localPlayersRefusal(target.Primary(), room) != "" {
		return true
	}
	return room.State != state.RoomStateWaiting && !(roomAcceptsLateJoin(room) && canLateJoin(target.Primary()))
}

func (h *Hub) handleChallengeSend(c *Client, env protocol.Envelope) {
	var req protocol.ChallengeSendRequest
	if err := json.Unmarshal(env.Data, &req); err != nil {
		return
	}
	refuse := func(result string) {
		c.sendEnvelope(protocol.TypeChallengeResult, env.ID, protocol.ChallengeResultPayload{
			ToUserID: req.ToUserID, Result: result,
		})
	}
	now := time.Now()
	if !c.allowChallenge(now) {
		refuse(protocol.ReasonTooFast)
		return
	}
	target := h.state.GetUser(req.ToUserID)
	if target == nil || target.Primary() == c.user || c.user.CurrentMatch != nil {
		refuse(protocol.ChallengeUnavailable)
		return
	}
	if !target.Primary().Social {
		refuse(protocol.ReasonNeedsUpdate)
		return
	}
	if target.Primary().CurrentMatch != nil {
		refuse(protocol.ChallengeBusy)
		return
	}
	// Per player asked, not per PC: a no from one name mustn't show on another.
	if h.challenges.inCooldown(c.user.ID, target.ID, now) {
		refuse(protocol.ChallengeCooldown)
		return
	}

	note := clipRunes(strings.TrimSpace(cleanText(req.Note)), challengeNoteMaxRunes)
	if note != "" && h.mod != nil {
		if term, hit := h.mod.FilterMessage(note); hit {
			refuse(protocol.ChallengeBlocked)
			slog.Info("challenge note blocked", "user", c.user.Username, "ip", c.ip, "term", term)
			return
		}
		if _, muted := h.mod.IsMuted(c.ip); muted && !c.isModerator {
			note = ""
		}
	}

	ch := &challenge{from: c, to: target, note: note, liveReplay: req.LiveReplay}
	incoming := protocol.ChallengeIncomingPayload{}
	if room := c.user.CurrentRoom; room != nil {
		if inviteRefusal(target, room) {
			refuse(protocol.ChallengeUnavailable)
			return
		}
		ch.roomID = room.ID
		ch.rom = protocol.RomInfo{Name: room.RomName, MD5: room.RomMD5, Region: room.RomRegion}
		_, _, name, players, maxPlayers, _ := room.Snapshot()
		incoming.Invite = true
		incoming.RoomName = name
		incoming.RoomPlayers = players
		incoming.RoomMax = maxPlayers
	} else {
		rom := protocol.RomInfo{
			Name:   clipRunes(strings.TrimSpace(req.Rom.Name), romNameMaxRunes),
			MD5:    clipRunes(strings.TrimSpace(req.Rom.MD5), romFieldMaxRunes),
			Region: clipRunes(strings.TrimSpace(req.Rom.Region), romFieldMaxRunes),
		}
		if rom.Name == "" || rom.MD5 == "" {
			refuse(protocol.ChallengeUnavailable)
			return
		}
		if h.mod != nil {
			if term, hit := h.mod.FilterMessage(rom.Name); hit {
				refuse(protocol.ChallengeUnavailable)
				slog.Info("challenge game name blocked", "user", c.user.Username, "ip", c.ip, "term", term)
				return
			}
		}
		if len(c.user.Group())+len(target.Group()) > maxRoomPlayers {
			refuse(protocol.ChallengeUnavailable)
			return
		}
		ch.rom = rom
	}
	if hashes := target.Primary().RomHashes; len(hashes) > 0 {
		if _, has := hashes[strings.ToUpper(ch.rom.MD5)]; !has {
			refuse(protocol.ChallengeNoGame)
			return
		}
	}
	result := h.challenges.add(ch, func() {
		if expired := h.challenges.take(ch.id, func(*challenge) bool { return true }); expired != nil {
			h.finishChallenge(expired, protocol.ChallengeTimedOut)
		}
	})
	if result != "" {
		refuse(result)
		return
	}
	id := ch.id

	slog.Info("challenge sent", "id", id, "from", c.user.Username, "to", target.Username,
		"rom", ch.rom.Name, "invite", ch.roomID != 0)
	expires := challengeAnswerTime.Milliseconds()
	c.sendEnvelope(protocol.TypeChallengeSent, env.ID, protocol.ChallengeSentPayload{
		ChallengeID: id,
		ToUserID:    target.ID,
		ToUsername:  target.Username,
		Rom:         ch.rom,
		ExpiresInMs: expires,
		Invite:      ch.roomID != 0,
	})
	incoming.ChallengeID = id
	incoming.FromUserID = c.user.ID
	incoming.FromUsername = c.user.Username
	incoming.ToUserID = target.ID
	incoming.ToUsername = target.Username
	incoming.Rom = ch.rom
	incoming.Note = note
	incoming.ExpiresInMs = expires
	sendToUser(target, protocol.TypeChallengeIncoming, incoming)
}

func (h *Hub) handleChallengeCancel(c *Client, env protocol.Envelope) {
	var req protocol.ChallengeCancelRequest
	if err := json.Unmarshal(env.Data, &req); err != nil {
		return
	}
	ch := h.challenges.take(req.ChallengeID, func(ch *challenge) bool { return ch.from.user == c.user })
	if ch != nil {
		h.finishChallenge(ch, protocol.ChallengeCancelled)
	}
}

func (h *Hub) handleChallengeRespond(c *Client, env protocol.Envelope) {
	var req protocol.ChallengeRespondRequest
	if err := json.Unmarshal(env.Data, &req); err != nil {
		return
	}
	ch := h.challenges.take(req.ChallengeID, func(ch *challenge) bool { return ch.to.Primary() == c.user })
	if ch == nil {
		return // it ended first; the CHALLENGE_RESULT is already on its way
	}
	if !req.Accept {
		h.challenges.noteDeclined(ch.from.user.ID, ch.to.ID, time.Now())
		h.finishChallenge(ch, protocol.ChallengeDeclined)
		return
	}
	h.finishChallenge(ch, h.acceptChallenge(c, ch))
}

// acceptChallenge seats both PCs and returns the challenge's result.
func (h *Hub) acceptChallenge(c *Client, ch *challenge) string {
	from := ch.from.user
	if from.CurrentMatch != nil || c.user.CurrentMatch != nil {
		return protocol.ChallengeBusy
	}
	if ch.roomID != 0 {
		return h.acceptInvite(c, ch)
	}
	if from.CurrentRoom != nil {
		return protocol.ChallengeUnavailable // they went somewhere else meanwhile
	}
	seats := len(from.Group()) + len(c.user.Group())
	if seats > maxRoomPlayers {
		return protocol.ChallengeUnavailable
	}

	h.dequeueQuickMatch(ch.from)

	room := &state.Room{
		ID:         h.state.NextRoomID(),
		Name:       fmt.Sprintf("%s vs %s", from.Username, ch.to.Username),
		Host:       from,
		RomName:    ch.rom.Name,
		RomMD5:     ch.rom.MD5,
		RomRegion:  ch.rom.Region,
		MaxPlayers: seats,
		Delay:      qmDefaultDelay,
		Prediction: qmDefaultPrediction,
		Pacing:     qmDefaultPacing,
		// Open on Auto, as Create Room and Quick Match do.
		DelayAuto:         true,
		PredictionAuto:    true,
		LiveReplayEnabled: ch.liveReplay,
		Visibility:        state.VisibilityInviteOnly,
		Private:           true,
		State:             state.RoomStateWaiting,
		CreatedAt:         time.Now(),
	}
	// The sender first: if that fails, the recipient hasn't given anything up yet.
	if err := h.seatUser(from, room, state.UserStateHosting); err != nil {
		slog.Warn("challenge aborted: sender could not be seated", "user", from.Username, "err", err)
		return protocol.ChallengeUnavailable
	}
	// The card told the recipient what accepting costs.
	if current := c.user.CurrentRoom; current != nil {
		h.removeUserFromRoom(c.user, current, "left")
	}
	h.dequeueQuickMatch(c)
	if err := h.seatUser(c.user, room, state.UserStateInRoom); err != nil {
		slog.Warn("challenge aborted: recipient could not be seated", "user", c.user.Username, "err", err)
		for _, u := range from.Group() {
			h.unseatUser(u, room)
		}
		return protocol.ChallengeUnavailable
	}
	h.state.AddRoom(room)
	// A sender who disconnected just now was cleaned up without this room to clear.
	if h.getClient(from.ID) == nil {
		slog.Info("challenge aborted: sender is gone", "user", from.Username)
		for _, u := range append(from.Group(), c.user.Group()...) {
			h.unseatUser(u, room)
		}
		h.state.RemoveRoom(room.ID)
		return protocol.ChallengeUnavailable
	}
	slog.Info("challenge accepted", "id", ch.id, "room", room.ID, "host", from.Username, "seats", seats)

	payload := roomStatePayload(room)
	ch.from.sendEnvelope(protocol.TypeRoomJoinOK, "", payload)
	c.sendEnvelope(protocol.TypeRoomJoinOK, "", payload)
	h.introduceRoomPeers(room, c.user)
	for _, u := range append(from.Group(), c.user.Group()...) {
		h.broadcastPresenceUpdated(u)
	}
	h.broadcastRoomState(room)
	h.broadcastRoomList()
	return protocol.ChallengeAccepted
}

// acceptInvite seats the recipient's PC in the sender's room. An invite gets past
// the room's password and privacy; the sender is already inside it.
func (h *Hub) acceptInvite(c *Client, ch *challenge) string {
	room := h.state.GetRoom(ch.roomID)
	if room == nil || ch.from.user.CurrentRoom != room || inviteRefusal(ch.to, room) {
		return protocol.ChallengeUnavailable
	}
	if current := c.user.CurrentRoom; current != nil {
		h.removeUserFromRoom(c.user, current, "left")
	}
	if err := h.seatUser(c.user, room, state.UserStateInRoom); err != nil {
		slog.Warn("invite aborted: could not be seated", "user", c.user.Username, "err", err)
		return protocol.ChallengeUnavailable
	}
	h.announceRoomJoin(c, room, "")
	return protocol.ChallengeAccepted
}

// finishChallenge tells both sides how a challenge ended.
func (h *Hub) finishChallenge(ch *challenge, result string) {
	slog.Info("challenge ended", "id", ch.id, "from", ch.from.user.Username, "to", ch.to.Username, "result", result)
	payload := protocol.ChallengeResultPayload{ChallengeID: ch.id, ToUserID: ch.to.ID, Result: result}
	ch.from.sendEnvelope(protocol.TypeChallengeResult, "", payload)
	sendToUser(ch.to, protocol.TypeChallengeResult, payload)
}

// endChallengesFor ends every challenge to or from u's PC. Whoever was waiting on
// that PC is told result; challenges it sent are simply cancelled.
func (h *Hub) endChallengesFor(u *state.User, result string) {
	owner := u.Primary()
	for _, ch := range h.challenges.takeWhere(func(ch *challenge) bool {
		return ch.from.user == owner || ch.to.Primary() == owner
	}) {
		if ch.from.user == owner {
			h.finishChallenge(ch, protocol.ChallengeCancelled)
		} else {
			h.finishChallenge(ch, result)
		}
	}
}

// endChallengesTo ends the challenges waiting for one user, leaving the rest of
// their PC's alone. For a local player leaving the roster.
func (h *Hub) endChallengesTo(u *state.User, result string) {
	for _, ch := range h.challenges.takeWhere(func(ch *challenge) bool { return ch.to == u }) {
		h.finishChallenge(ch, result)
	}
}

// handleChallengeGames answers which of the asker's games a player also has, so
// the challenge dialog can offer those first. The asker's list is only what its
// HELLO claimed, so this can test someone's library for any game: game lists aren't
// treated as secret, and the probe limit is all that stands in the way.
func (h *Hub) handleChallengeGames(c *Client, env protocol.Envelope) {
	var req protocol.ChallengeGamesRequest
	if err := json.Unmarshal(env.Data, &req); err != nil {
		return
	}
	if !c.allowProbeRequest(time.Now()) {
		return
	}
	reply := protocol.ChallengeGamesReply{ToUserID: req.ToUserID, MD5s: []string{}}
	if target := h.state.GetUser(req.ToUserID); target != nil {
		theirs := target.Primary().RomHashes
		for md5 := range c.user.RomHashes {
			if _, has := theirs[md5]; has {
				reply.MD5s = append(reply.MD5s, md5)
			}
		}
	}
	c.sendEnvelope(protocol.TypeChallengeGamesReply, env.ID, reply)
}
