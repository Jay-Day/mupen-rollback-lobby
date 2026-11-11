package ws

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/jay-day/rmgk-lobby/internal/accounts"
	"github.com/jay-day/rmgk-lobby/internal/discord"
	"github.com/jay-day/rmgk-lobby/internal/protocol"
)

// Discord sign-in: a linked Discord account is an easier way into an
// account that still has its password. An unlinked Discord account gets a
// ticket; logging in (discord_link) or creating an account
// (discord_register) with it links the two. Only a successful link uses
// the ticket up.

// discordTicketTTL is how long a player has to log in after an unlinked
// Discord sign-in.
const discordTicketTTL = 10 * time.Minute

// EnableDiscord turns on Discord sign-in.
func (h *Hub) EnableDiscord(client *discord.Client) {
	h.discord = client
}

func (h *Hub) identifyDiscord(code, verifier, redirectURI string) (discord.User, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return h.discord.Identify(ctx, code, verifier, redirectURI)
}

type discordTicket struct {
	user    discord.User
	ip      string
	expires time.Time
}

// discordTickets holds unlinked Discord sign-ins waiting for a password
// login. Tickets are single-use, expire, and only work from the same IP.
// The zero value is ready to use.
type discordTickets struct {
	mu sync.Mutex
	m  map[string]discordTicket
}

func (t *discordTickets) put(user discord.User, ip string) string {
	var b [24]byte
	rand.Read(b[:])
	id := base64.RawURLEncoding.EncodeToString(b[:])
	now := time.Now()
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.m == nil {
		t.m = make(map[string]discordTicket)
	}
	for k, v := range t.m {
		if now.After(v.expires) {
			delete(t.m, k)
		}
	}
	t.m[id] = discordTicket{user: user, ip: ip, expires: now.Add(discordTicketTTL)}
	return id
}

// peek returns a ticket's Discord account without using it up, so a
// mistyped password or a taken name can be retried.
func (t *discordTickets) peek(id, ip string) (discord.User, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	ticket, ok := t.m[id]
	if !ok || time.Now().After(ticket.expires) || ticket.ip != ip {
		return discord.User{}, false
	}
	return ticket.user, true
}

func (t *discordTickets) take(id, ip string) (discord.User, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	ticket, ok := t.m[id]
	if !ok {
		return discord.User{}, false
	}
	delete(t.m, id)
	if time.Now().After(ticket.expires) || ticket.ip != ip {
		return discord.User{}, false
	}
	return ticket.user, true
}

// handleAccountLinkDiscord links the logged-in account to a Discord account.
func (h *Hub) handleAccountLinkDiscord(c *Client, env protocol.Envelope) {
	fail := func(reason, message string) {
		c.sendEnvelope(protocol.TypeAccountDiscordFail, env.ID, protocol.AccountPasswordFailPayload{Reason: reason, Message: message})
	}
	if h.accounts == nil || !c.user.Verified() {
		fail(protocol.ReasonNotAuthorized, "Log in to link Discord.")
		return
	}
	if h.discord == nil {
		fail(protocol.ReasonDiscordUnavailable, "This server doesn't support Discord sign-in.")
		return
	}
	now := time.Now()
	if !h.authFailures.allowed(c.ip, authFailLimit, authFailWindow, now) {
		fail(protocol.ReasonTooManyAttempts, "Too many failed attempts. Try again later.")
		return
	}
	var req protocol.DiscordCodeRequest
	if err := json.Unmarshal(env.Data, &req); err != nil {
		fail(protocol.ReasonInvalidPayload, "")
		return
	}
	user, err := h.identifyDiscord(req.Code, req.CodeVerifier, req.RedirectURI)
	if err != nil {
		h.authFailures.record(c.ip, now)
		slog.Info("discord link failed", "user", c.user.Username, "err", err)
		fail(protocol.ReasonDiscordFailed, "Discord sign-in didn't work. Try again.")
		return
	}
	err = h.accounts.LinkDiscord(c.user.AccountID, user.ID, user.Username, user.DisplayName())
	if errors.Is(err, accounts.ErrDiscordInUse) {
		fail(protocol.ReasonDiscordInUse, "That Discord account is already linked to another account.")
		return
	}
	if err != nil {
		slog.Error("discord link failed", "user", c.user.Username, "err", err)
		fail(protocol.ReasonAccountsUnavailable, "Accounts are temporarily unavailable.")
		return
	}
	slog.Info("discord linked", "user", c.user.Username, "discord", user.Username)
	c.sendEnvelope(protocol.TypeAccountDiscord, env.ID, protocol.DiscordAccountInfo{
		Linked: true, Username: user.Username, Name: user.DisplayName(),
	})
}

// handleAccountUnlinkDiscord removes the logged-in account's Discord link;
// the password keeps working.
func (h *Hub) handleAccountUnlinkDiscord(c *Client, env protocol.Envelope) {
	if h.accounts == nil || !c.user.Verified() {
		c.sendEnvelope(protocol.TypeAccountDiscordFail, env.ID,
			protocol.AccountPasswordFailPayload{Reason: protocol.ReasonNotAuthorized, Message: "Log in to unlink Discord."})
		return
	}
	if err := h.accounts.UnlinkDiscord(c.user.AccountID); err != nil {
		slog.Error("discord unlink failed", "user", c.user.Username, "err", err)
		c.sendEnvelope(protocol.TypeAccountDiscordFail, env.ID,
			protocol.AccountPasswordFailPayload{Reason: protocol.ReasonAccountsUnavailable, Message: "Accounts are temporarily unavailable."})
		return
	}
	slog.Info("discord unlinked", "user", c.user.Username)
	c.sendEnvelope(protocol.TypeAccountDiscord, env.ID, protocol.DiscordAccountInfo{Linked: false})
}
