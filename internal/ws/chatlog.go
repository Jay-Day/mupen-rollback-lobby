package ws

import (
	"sync"
	"time"

	"github.com/jay-day/rmgk-lobby/internal/modlog"
)

// blockedChatLog samples the chat lines the lobby stopped (chat filter,
// sender muted, flood limit) before they reach the moderators' log. A muted
// or flooding sender can send a 500-character line every second; logged in
// full that is about 0.5 GB a day per address. So per IP, the first
// blockedLogBurst blocked lines in each blockedLogWindow are logged as they
// come, and the rest are counted: the next row logged from that IP carries
// the count (modlog Suppressed), and when the window ends with lines still
// counted, the newest of them is logged as the sample, carrying the count of
// the others. Moderators see what was said and how much, never a gap.
//
// Delivered chat isn't sampled: everyone saw it, and the flood limit bounds
// it. The moderators' log also has a size cap (lobby.json modLogMaxMB).
type blockedChatLog struct {
	mu  sync.Mutex
	ips map[string]*blockedIP
}

type blockedIP struct {
	windowStart time.Time
	logged      int
	pending     int                // blocked lines counted, not logged, since the last row
	last        modlog.ChatMessage // the newest of them
}

const (
	blockedLogBurst  = 3
	blockedLogWindow = time.Minute
	blockedLogMaxIPs = 4096
)

func newBlockedChatLog() *blockedChatLog {
	return &blockedChatLog{ips: map[string]*blockedIP{}}
}

// admit decides whether blocked line m from ip is logged now; when it is, m
// comes back carrying the count of the lines from ip held back before it. A
// nil log admits everything.
func (b *blockedChatLog) admit(ip string, m modlog.ChatMessage, now time.Time) (modlog.ChatMessage, bool) {
	if b == nil {
		return m, true
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	e := b.ips[ip]
	if e == nil {
		if len(b.ips) >= blockedLogMaxIPs {
			b.evictLocked()
		}
		e = &blockedIP{windowStart: now}
		b.ips[ip] = e
	}
	if now.Sub(e.windowStart) >= blockedLogWindow {
		e.windowStart, e.logged = now, 0
	}
	if e.logged < blockedLogBurst {
		e.logged++
		m.Suppressed = e.pending
		e.pending, e.last = 0, modlog.ChatMessage{}
		return m, true
	}
	e.pending++
	e.last = m
	return m, false
}

// flush returns the samples due: for every IP whose window has ended with
// lines held back, the newest of them carrying the count of the others. IPs
// with nothing held back and an ended window are forgotten.
func (b *blockedChatLog) flush(now time.Time) []modlog.ChatMessage {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []modlog.ChatMessage
	for ip, e := range b.ips {
		if now.Sub(e.windowStart) < blockedLogWindow {
			continue
		}
		if e.pending > 0 {
			m := e.last
			m.Suppressed = e.pending - 1
			out = append(out, m)
		}
		delete(b.ips, ip)
	}
	return out
}

// evictLocked makes room by dropping the IP whose window started first; its
// held-back lines are lost (only the count: nothing was logged for them).
func (b *blockedChatLog) evictLocked() {
	var oldest string
	var at time.Time
	for ip, e := range b.ips {
		if oldest == "" || e.windowStart.Before(at) {
			oldest, at = ip, e.windowStart
		}
	}
	delete(b.ips, oldest)
}

// flushBlockedChat writes the samples due. The hub's loop calls it.
func (h *Hub) flushBlockedChat(now time.Time) {
	if h.modlog == nil {
		return
	}
	for _, m := range h.blockedLog.flush(now) {
		h.modlog.Chat(m)
	}
}
