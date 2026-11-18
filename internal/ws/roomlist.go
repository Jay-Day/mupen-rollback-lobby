package ws

import (
	"time"

	"github.com/jay-day/rmgk-lobby/internal/protocol"
)

// The full room list goes to every player whenever a room appears, fills,
// starts, streams or goes away: about 300 bytes per room, times everyone
// online. However many players cause changes, it goes out at most once per
// lobby.json roomListMinIntervalMs: the first change is sent at once, and
// changes inside the interval go out together, once, when it ends.
//
// A list is a snapshot of the lobby when it's built, and clients replace
// their whole room list with each one, so skipping the in-between ones loses
// nothing. Lists are built and queued under roomListMu, as viewer counts are,
// so the two stay in order. HELLO's own list (sendRoomListTo) is never held
// back.

// broadcastRoomList sends the room list to everyone, now or at the end of
// the current interval.
func (h *Hub) broadcastRoomList() {
	h.roomListMu.Lock()
	defer h.roomListMu.Unlock()
	interval := h.cfg().RoomListMinInterval()
	now := time.Now()
	if interval <= 0 || h.roomListLast.IsZero() || now.Sub(h.roomListLast) >= interval {
		h.sendRoomListLocked(now)
		return
	}
	if !h.roomListPending {
		h.roomListPending = true
		select {
		case h.roomListKick <- struct{}{}:
		default: // Run already has a kick waiting
		}
	}
}

// sendRoomListLocked builds the list and queues it for everyone. Callers
// hold roomListMu.
func (h *Hub) sendRoomListLocked(now time.Time) {
	h.roomListPending = false
	h.roomListLast = now
	h.broadcastToAll(protocol.TypeRoomList, h.buildRoomListPayload(), 0)
}

// flushRoomList sends a held-back list once its interval is over, and
// returns how long until then (0: nothing waits). Run calls it.
func (h *Hub) flushRoomList(now time.Time) time.Duration {
	h.roomListMu.Lock()
	defer h.roomListMu.Unlock()
	if !h.roomListPending {
		return 0
	}
	interval := h.cfg().RoomListMinInterval()
	if wait := h.roomListLast.Add(interval).Sub(now); interval > 0 && wait > 0 {
		return wait
	}
	h.sendRoomListLocked(now)
	return 0
}

// sendRoomListTo sends one client the room list now (at HELLO).
func (h *Hub) sendRoomListTo(c *Client) {
	h.roomListMu.Lock()
	defer h.roomListMu.Unlock()
	c.sendEnvelope(protocol.TypeRoomList, "", h.buildRoomListPayload())
}
