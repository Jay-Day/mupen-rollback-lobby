package ws

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"time"

	"github.com/jay-day/rmgk-lobby/internal/moderation"
	"github.com/jay-day/rmgk-lobby/internal/modlog"
	"github.com/jay-day/rmgk-lobby/internal/protocol"
	"github.com/jay-day/rmgk-lobby/internal/ratelimit"
	"github.com/jay-day/rmgk-lobby/internal/state"
)

// Per-IP limits on the requests that reach every connected player, or cost
// the server bulk data (internal/ratelimit, lobby.json). Each handler asks
// allowAction before it changes anything, sends anything, or writes to the
// moderators' log, and answers a refusal with a reply today's clients already
// show (ROOM_CREATE_FAIL, ROOM_JOIN_FAIL, SPECTATE_FAIL, MATCH_ADMIT_FAIL or a
// MOD_NOTICE), saying how long to wait. Leaving, dropping, cancelling,
// stopping, ending and relaying are never limited, so nobody is ever stuck.
//
// Refusals reach moderators in aggregate only: the Rate limits report (one
// record per IP, saved in the moderators' log) and throttled server log lines.

const (
	// How often the report's changes are written to the moderators' log: a
	// flood changes a record on every request, the file at most this often.
	rateLimitSaveInterval = 30 * time.Second
	// One refusal notice per connection and kind per this long, so a flood
	// isn't answered with a flood.
	refusalNoticeInterval = 3 * time.Second
)

// Log lines that a misbehaving client could otherwise repeat at will: the
// first per key, then at most one a minute with the count it stood for.
var (
	punchFailLog    = ratelimit.NewLogThrottle(time.Minute, 4096)
	lateJoinDropLog = ratelimit.NewLogThrottle(time.Minute, 4096)
	liveReplayLog   = ratelimit.NewLogThrottle(time.Minute, 4096)
	roomLeaveLog    = ratelimit.NewLogThrottle(time.Minute, 4096)
)

// SetActionLimiter turns on the per-IP request limits. Call before Run, then
// LoadRateLimits and RunRateLimits.
func (h *Hub) SetActionLimiter(l *ratelimit.ActionLimiter) {
	h.actions = l
	h.actionLog = ratelimit.NewLogThrottle(time.Minute, ratelimit.DefaultActionMaxIPs)
	h.noticeLog = ratelimit.NewLogThrottle(refusalNoticeInterval, ratelimit.DefaultActionMaxIPs)
	h.rlSaved = map[string]string{}
}

// allowAction spends one request of kind from c's IP. On a refusal it logs
// (throttled) and, when the refusals add up to the automatic ban, bans the
// IP and closes its connections; retry is how long until it may try again.
// Moderators are never limited. Call it on c's read loop, before the request
// changes or sends anything.
func (h *Hub) allowAction(c *Client, kind ratelimit.ActionKind) (ok bool, retry time.Duration) {
	if h.actions == nil || c.user == nil {
		return true, 0
	}
	if h.activeModerator(c) {
		return true, 0
	}
	ip := moderation.CanonicalIP(c.ip)
	ok, retry, ban := h.actions.Allow(ip, kind, c.user.Username)
	if ok {
		return true, 0
	}
	if log, suppressed := h.actionLog.Allow(ip + " " + string(kind)); log {
		slog.Warn("rate limited", "ip", ip, "kind", kind, "user", c.user.Username,
			"retry_after", retry.Round(time.Second), "more_since_last_line", suppressed)
	}
	if ban {
		h.autoBan(ip, c.user.Username, kind)
	}
	return false, retry
}

// tooFast is a refusal: what the player did, and when they can try again.
func tooFast(what string, retry time.Duration) string {
	return what + " Try again in " + waitText(retry) + "."
}

// waitText is retry in whole seconds, rounded up: "30 seconds".
func waitText(retry time.Duration) string {
	s := int(math.Ceil(retry.Seconds()))
	if s <= 1 {
		return "1 second"
	}
	return fmt.Sprintf("%d seconds", s)
}

// refusalDue reports whether c should hear about a refusal of kind now: at
// most once per kind every refusalNoticeInterval, so a flood of requests
// isn't answered with a flood of replies. (Replies a client waits for, like
// ROOM_CREATE_FAIL, always go out.)
func (h *Hub) refusalDue(c *Client, kind ratelimit.ActionKind) bool {
	if h.noticeLog == nil || c.user == nil {
		return true
	}
	due, _ := h.noticeLog.Allow(fmt.Sprintf("%d %s", c.user.ID, kind))
	return due
}

// refusalNotice sends c a MOD_NOTICE about a refusal (see refusalDue).
func (h *Hub) refusalNotice(c *Client, kind ratelimit.ActionKind, text string) {
	if h.refusalDue(c, kind) {
		c.notice("warn", text)
	}
}

// refuseRoomUpdate answers a refused room setting change: the room as it
// stands (so the sender's controls go back to it) and a notice.
func (h *Hub) refuseRoomUpdate(c *Client, room *state.Room, retry time.Duration) {
	if !h.refusalDue(c, ratelimit.ActionRoomUpdate) {
		return
	}
	c.sendEnvelope(protocol.TypeRoomState, "", roomStatePayload(room))
	c.notice("warn", tooFast("You're changing room settings too fast.", retry))
}

// kindNames say what each limit covers, in the moderators' words.
var kindNames = map[ratelimit.ActionKind]string{
	ratelimit.ActionRoomCreate:  "creating rooms",
	ratelimit.ActionMatchStart:  "starting games",
	ratelimit.ActionRoomJoin:    "joining rooms",
	ratelimit.ActionSpectate:    "opening live replays",
	ratelimit.ActionRoomUpdate:  "changing room settings",
	ratelimit.ActionLobbyUpdate: "lobby updates",
}

func kindName(k ratelimit.ActionKind) string {
	if n, ok := kindNames[k]; ok {
		return n
	}
	return string(k)
}

// plainDuration is "10 minutes", "1 hour", "45 seconds".
func plainDuration(d time.Duration) string {
	unit := func(n int64, word string) string {
		if n == 1 {
			return "1 " + word
		}
		return fmt.Sprintf("%d %ss", n, word)
	}
	switch {
	case d >= 24*time.Hour && d%(24*time.Hour) == 0:
		return unit(int64(d/(24*time.Hour)), "day")
	case d >= time.Hour && d%time.Hour == 0:
		return unit(int64(d/time.Hour), "hour")
	case d >= time.Minute && d%time.Minute == 0:
		return unit(int64(d/time.Minute), "minute")
	}
	return unit(int64(math.Ceil(d.Seconds())), "second")
}

// autoBan bans ip for the configured time after its refusals reached the
// automatic ban, closes its connections (a signed-in moderator's excepted),
// and records it for the moderators, as the login guard's bans are.
func (h *Hub) autoBan(ip, name string, kind ratelimit.ActionKind) {
	limits := h.actions.Limits()
	reason := fmt.Sprintf("Rate limit: %d refused requests within %s (last: %s)",
		limits.BanStrikes, plainDuration(limits.BanWindow), kindName(kind))
	until := time.Now().Add(limits.BanDuration)
	targets := h.sanctionTargets(ip)
	if h.mod != nil {
		h.mod.BanNamed(ip, name, moderation.AutoBanBy, reason, limits.BanDuration)
	}
	detail := "Timed out " + ip + untilSuffix(until) + ": " + reason
	if len(targets) > 0 {
		detail += fmt.Sprintf(" (%d disconnected)", len(targets))
	}
	slog.Warn("rate limit: banning ip", "ip", ip, "user", name, "for", limits.BanDuration, "kind", kind)
	h.modlog.Event(modlog.Event{Type: modlog.EventModeration, Name: name, IP: ip, Text: moderation.AutoBanBy + ": " + detail})
	h.audit(moderation.AutoBanBy, "timeout", detail, modlog.SourceAuto, ip)
	notice := "You sent too many requests too fast, so this address is blocked" + untilSuffix(until) + "."
	for _, tc := range targets {
		go tc.disconnect(notice)
	}
}

// RateLimitKind is one request limit in effect.
type RateLimitKind struct {
	Kind   string
	Count  int // 0: no limit
	Window time.Duration
}

// RateLimitReport is the request limits in effect and the IPs that hit
// them, most recently refused first.
type RateLimitReport struct {
	Attached            bool // false: the lobby runs without a limiter
	ExemptLoopback      bool
	Kinds               []RateLimitKind
	BanStrikes          int // 0: no automatic ban
	BanWindow           time.Duration
	BanDuration         time.Duration
	ReportTTL           time.Duration
	RoomListMinInterval time.Duration
	Records             []ratelimit.ActionRecord
}

// rateLimitKinds is the report's order.
var rateLimitKinds = []ratelimit.ActionKind{
	ratelimit.ActionRoomCreate, ratelimit.ActionRoomJoin, ratelimit.ActionMatchStart,
	ratelimit.ActionSpectate, ratelimit.ActionRoomUpdate, ratelimit.ActionLobbyUpdate,
}

// RateLimitReport is the moderators' view of the request limits.
func (h *Hub) RateLimitReport() RateLimitReport {
	r := RateLimitReport{RoomListMinInterval: h.cfg().RoomListMinInterval()}
	if h.actions == nil {
		return r
	}
	limits := h.actions.Limits()
	r.Attached = true
	r.ExemptLoopback = h.actions.ExemptsLoopback()
	for _, k := range rateLimitKinds {
		rate := limits.Rates[k]
		if rate.Count < 0 || rate.Window <= 0 {
			rate.Count = 0
		}
		r.Kinds = append(r.Kinds, RateLimitKind{Kind: string(k), Count: rate.Count, Window: rate.Window})
	}
	r.BanStrikes, r.BanWindow, r.BanDuration, r.ReportTTL = limits.BanStrikes, limits.BanWindow, limits.BanDuration, limits.ReportTTL
	r.Records = h.actions.Snapshot()
	return r
}

// ForgetRateLimit drops an IP's record from the report (and its budgets);
// false when it had none. The caller audits it.
func (h *Hub) ForgetRateLimit(ip string) bool {
	if h.actions == nil {
		return false
	}
	return h.actions.Forget(moderation.CanonicalIP(strings.TrimSpace(ip)))
}

// LoadRateLimits restores the report saved in the moderators' log, and
// returns how many records it holds now. Call after SetModLog and
// SetActionLimiter, before Run.
func (h *Hub) LoadRateLimits() int {
	if h.actions == nil || h.modlog == nil {
		return 0
	}
	recs, err := h.modlog.RateLimits()
	if err != nil {
		slog.Warn("rate limit report: couldn't read the saved records", "err", err)
		return 0
	}
	h.actions.Import(recs)
	h.rlMu.Lock()
	for _, r := range recs {
		// what the file holds; anything Import skipped (expired) is removed
		// at the next save
		h.rlSaved[r.IP] = recordFingerprint(r)
	}
	h.rlMu.Unlock()
	return len(h.actions.Snapshot())
}

// SaveRateLimits writes what changed in the report since the last save, as
// one queued write to the moderators' log. RunRateLimits calls it every
// rateLimitSaveInterval; call it once more at shutdown, before closing the log.
func (h *Hub) SaveRateLimits() {
	if h.actions == nil || h.modlog == nil {
		return
	}
	snap := h.actions.Snapshot()
	h.rlMu.Lock()
	defer h.rlMu.Unlock()
	now := make(map[string]string, len(snap))
	var upsert []ratelimit.ActionRecord
	for _, r := range snap {
		r.Allowed = nil // the current windows aren't saved
		fp := recordFingerprint(r)
		now[r.IP] = fp
		if h.rlSaved[r.IP] != fp {
			upsert = append(upsert, r)
		}
	}
	var remove []string
	for ip := range h.rlSaved {
		if _, ok := now[ip]; !ok {
			remove = append(remove, ip)
		}
	}
	if len(upsert) == 0 && len(remove) == 0 {
		return
	}
	if !h.modlog.SaveRateLimits(upsert, remove) {
		return // the log's queue was full: the next save tries again
	}
	h.rlSaved = now
}

func recordFingerprint(r ratelimit.ActionRecord) string {
	r.Allowed = nil
	b, _ := json.Marshal(r)
	return string(b)
}

// RunRateLimits prunes the limiter, writes the summaries of throttled log
// lines, and saves the report until ctx ends. Run it in its own goroutine.
func (h *Hub) RunRateLimits(ctx context.Context) {
	if h.actions == nil {
		return
	}
	go h.actions.Janitor(ctx)
	go h.actionLog.Janitor(ctx, func(key string, n int) {
		ip, kind, _ := strings.Cut(key, " ")
		slog.Warn("rate limited (summary)", "ip", ip, "kind", kind, "more_refusals", n)
	})
	t := time.NewTicker(rateLimitSaveInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			h.SaveRateLimits()
		}
	}
}
