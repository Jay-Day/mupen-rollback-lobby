package ws

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/jay-day/rmgk-lobby/internal/accounts"
	"github.com/jay-day/rmgk-lobby/internal/protocol"
	"github.com/jay-day/rmgk-lobby/internal/ranked"
	"github.com/jay-day/rmgk-lobby/internal/state"
)

const (
	// Failed logins/token checks per IP before further attempts are refused.
	authFailLimit  = 10
	authFailWindow = 15 * time.Minute
	// New accounts per IP.
	registerLimit  = 3
	registerWindow = time.Hour

	// How long a verified login waits for an older connection holding the
	// same name (a stale session of the same account, or a guest) to clean up.
	usernameTakeoverWait = 3 * time.Second
)

// EnableAccounts turns on accounts and ranked play. Without it the lobby runs
// guest-only, exactly as before.
func (h *Hub) EnableAccounts(store *accounts.Store, cfg *ranked.Config) {
	h.accounts = store
	h.rankedConfig = cfg
	boot := make([]byte, 8)
	_, _ = rand.Read(boot)
	h.bootID = hex.EncodeToString(boot)
}

// attemptLimiter is a sliding-window counter keyed by IP.
type attemptLimiter struct {
	mu       sync.Mutex
	attempts map[string][]time.Time
}

// allowed reports whether key is under limit within window.
func (l *attemptLimiter) allowed(key string, limit int, window time.Duration, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	cutoff := now.Add(-window)
	kept := l.attempts[key][:0]
	for _, t := range l.attempts[key] {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	if len(kept) == 0 {
		delete(l.attempts, key)
	} else {
		l.attempts[key] = kept
	}
	return len(kept) < limit
}

// retryAfter is how long until key is under limit again (0 if it already is).
func (l *attemptLimiter) retryAfter(key string, limit int, window time.Duration, now time.Time) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	cutoff := now.Add(-window)
	var kept []time.Time
	for _, t := range l.attempts[key] {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	if len(kept) < limit {
		return 0
	}
	// the attempt whose expiry brings the count back under the limit
	return kept[len(kept)-limit].Add(window).Sub(now)
}

// lockedOut is the HELLO failure for an IP over the failed-login limit; it
// says when to try again.
func (h *Hub) lockedOut(ip string, now time.Time) helloAuthResult {
	wait := h.authFailures.retryAfter(ip, authFailLimit, authFailWindow, now)
	minutes := int((wait + time.Minute - 1) / time.Minute)
	if minutes < 1 {
		minutes = 1
	}
	unit := "minutes"
	if minutes == 1 {
		unit = "minute"
	}
	return helloAuthResult{reason: protocol.ReasonTooManyAttempts, retryAfter: int(wait.Seconds() + 0.999),
		message: fmt.Sprintf("Too many failed logins from your network. Try again in %d %s.", minutes, unit)}
}

func (l *attemptLimiter) record(key string, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.attempts == nil {
		l.attempts = make(map[string][]time.Time)
	}
	l.attempts[key] = append(l.attempts[key], now)
}

// helloAuthResult is the outcome of HELLO authentication.
type helloAuthResult struct {
	account *accounts.Account // nil for guests
	token   string
	reason  string // non-empty = reject the HELLO
	message string
	// discord_unlinked: the link ticket and the Discord name
	ticket      string
	discordName string
	retryAfter  int // too_many_attempts: seconds until a login is accepted again
}

func authFail(reason, message string) helloAuthResult {
	return helloAuthResult{reason: reason, message: message}
}

// authenticateHello resolves the HELLO's optional account authentication.
// Guests may not use a registered name.
func (h *Hub) authenticateHello(c *Client, req *protocol.HelloRequest) helloAuthResult {
	if req.Auth == nil {
		if h.accounts == nil {
			return helloAuthResult{}
		}
		registered, err := h.accounts.IsRegistered(req.Username)
		if err != nil {
			slog.Error("account lookup failed", "err", err)
			return authFail(protocol.ReasonAccountsUnavailable, "Accounts are temporarily unavailable.")
		}
		if registered {
			return authFail(protocol.ReasonUsernameRegistered,
				"That name belongs to a registered account. Log in or pick another name.")
		}
		return helloAuthResult{}
	}

	if h.accounts == nil {
		return authFail(protocol.ReasonAccountsUnavailable, "This server doesn't support accounts.")
	}
	now := time.Now()
	if !h.authFailures.allowed(c.ip, authFailLimit, authFailWindow, now) {
		return h.lockedOut(c.ip, now)
	}

	var (
		account accounts.Account
		token   string
		err     error
	)
	switch req.Auth.Mode {
	case "register":
		if !h.registrations.allowed(c.ip, registerLimit, registerWindow, now) {
			return authFail(protocol.ReasonTooManyAttempts, "Too many new accounts from this address. Try again later.")
		}
		account, err = h.accounts.Register(req.Username, req.Auth.Password)
		switch {
		case errors.Is(err, accounts.ErrUsernameTaken):
			return authFail(protocol.ReasonUsernameRegistered, "That name is already registered.")
		case errors.Is(err, accounts.ErrWeakPassword):
			return authFail(protocol.ReasonWeakPassword, "Passwords must be 8 to 128 characters.")
		case err == nil:
			h.registrations.record(c.ip, now)
			slog.Info("account registered", "user", account.Username, "id", account.ID, "ip", c.ip)
		}
	case "login":
		account, err = h.accounts.Login(req.Username, req.Auth.Password)
	case "token":
		token = req.Auth.Token
		account, err = h.accounts.SessionAccount(token)
	case "discord":
		if h.discord == nil {
			return authFail(protocol.ReasonDiscordUnavailable, "This server doesn't support Discord sign-in.")
		}
		user, derr := h.identifyDiscord(req.Auth.Code, req.Auth.CodeVerifier, req.Auth.RedirectURI)
		if derr != nil {
			h.authFailures.record(c.ip, now)
			slog.Info("discord sign-in failed", "ip", c.ip, "err", derr)
			return authFail(protocol.ReasonDiscordFailed, "Discord sign-in didn't work. Try again.")
		}
		account, err = h.accounts.DiscordLogin(user.ID, user.Username, user.DisplayName())
		if errors.Is(err, accounts.ErrNotFound) {
			ticket := h.discordTickets.put(user, c.ip)
			return helloAuthResult{reason: protocol.ReasonDiscordUnlinked, ticket: ticket, discordName: user.Username,
				message: "This Discord account isn't linked to an account yet. Log in or create an account to link it."}
		}
		if err == nil {
			slog.Info("discord sign-in", "user", account.Username, "discord", user.Username, "ip", c.ip)
		}
	case "discord_link", "discord_register":
		// the ticket stays valid until the link succeeds, so a wrong
		// password or a taken name can be fixed and retried
		user, ok := h.discordTickets.peek(req.Auth.Ticket, c.ip)
		if !ok {
			return authFail(protocol.ReasonTicketExpired, "The Discord sign-in expired. Use Log in with Discord again.")
		}
		if req.Auth.Mode == "discord_register" {
			if !h.registrations.allowed(c.ip, registerLimit, registerWindow, now) {
				return authFail(protocol.ReasonTooManyAttempts, "Too many new accounts from this address. Try again later.")
			}
			account, err = h.accounts.Register(req.Username, req.Auth.Password)
			switch {
			case errors.Is(err, accounts.ErrUsernameTaken):
				return authFail(protocol.ReasonUsernameRegistered, "That name is already registered.")
			case errors.Is(err, accounts.ErrWeakPassword):
				return authFail(protocol.ReasonWeakPassword, "Passwords must be 8 to 128 characters.")
			case err == nil:
				h.registrations.record(c.ip, now)
				slog.Info("account registered", "user", account.Username, "id", account.ID, "ip", c.ip)
			}
		} else {
			account, err = h.accounts.Login(req.Username, req.Auth.Password)
		}
		if err == nil {
			if lerr := h.accounts.LinkDiscord(account.ID, user.ID, user.Username, user.DisplayName()); errors.Is(lerr, accounts.ErrDiscordInUse) {
				h.discordTickets.take(req.Auth.Ticket, c.ip)
				return authFail(protocol.ReasonDiscordInUse, "That Discord account is already linked to another account.")
			} else if lerr != nil {
				err = lerr
			} else {
				h.discordTickets.take(req.Auth.Ticket, c.ip)
				slog.Info("discord linked", "user", account.Username, "discord", user.Username, "mode", req.Auth.Mode, "ip", c.ip)
			}
		}
	case "reset":
		account, err = h.accounts.ResetPassword(req.Username, req.Auth.ResetCode, req.Auth.Password)
		switch {
		case errors.Is(err, accounts.ErrBadResetCode):
			h.authFailures.record(c.ip, now)
			if h.mailer != nil {
				return authFail(protocol.ReasonAuthFailed,
					"That reset code is wrong or has expired. Email yourself a new one, or ask a moderator.")
			}
			return authFail(protocol.ReasonAuthFailed, "That reset code is wrong or has expired. Ask a moderator for a new one.")
		case errors.Is(err, accounts.ErrWeakPassword):
			return authFail(protocol.ReasonWeakPassword, "Passwords must be 8 to 128 characters.")
		case err == nil:
			slog.Info("password reset with code", "user", account.Username, "ip", c.ip)
		}
	default:
		return authFail(protocol.ReasonInvalidPayload, "")
	}

	switch {
	case errors.Is(err, accounts.ErrBadCredentials), errors.Is(err, accounts.ErrInvalidSession):
		h.authFailures.record(c.ip, now)
		slog.Info("authentication failed", "mode", req.Auth.Mode, "ip", c.ip)
		return authFail(protocol.ReasonAuthFailed, "Wrong username or password, or your login expired.")
	case errors.Is(err, accounts.ErrAccountDisabled):
		return authFail(protocol.ReasonAuthFailed, "This account is disabled.")
	case err != nil:
		slog.Error("authentication error", "mode", req.Auth.Mode, "err", err)
		return authFail(protocol.ReasonAccountsUnavailable, "Accounts are temporarily unavailable.")
	}

	if token == "" {
		if token, err = h.accounts.CreateSession(account.ID); err != nil {
			slog.Error("session creation failed", "err", err)
			return authFail(protocol.ReasonAccountsUnavailable, "Accounts are temporarily unavailable.")
		}
	}
	return helloAuthResult{account: &account, token: token}
}

// releaseUsernameFor disconnects whoever currently holds name so its owner can
// log in: their own stale session after a crash, or a guest that connected
// before the name was registered. Waits briefly for the cleanup to finish.
func (h *Hub) releaseUsernameFor(name string, accountID int64) {
	existing := h.state.GetUserByName(name)
	if existing == nil {
		return
	}
	h.mu.Lock()
	holder := h.clients[existing.ID]
	h.mu.Unlock()
	if holder != nil {
		reason := "Your name was claimed by its registered owner."
		if existing.AccountID == accountID {
			reason = "You signed in from another location."
		}
		slog.Info("releasing username for account login", "user", name, "previous_id", existing.ID)
		// No close handshake: the old connection is often a dead client that
		// would never answer it. Give the notice a moment to flush first.
		holder.notice("error", reason)
		go func() {
			time.Sleep(200 * time.Millisecond)
			holder.conn.CloseNow()
		}()
	}
	deadline := time.Now().Add(usernameTakeoverWait)
	for time.Now().Before(deadline) {
		if h.state.GetUserByName(name) == nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// accountInfo builds HELLO_OK's account section.
func (h *Hub) accountInfo(u *state.User) *protocol.AccountInfo {
	info := &protocol.AccountInfo{AccountID: u.AccountID, SessionToken: u.SessionToken}
	if h.discord != nil {
		if link, ok, err := h.accounts.DiscordLinkOf(u.AccountID); err == nil && ok {
			info.Discord = &protocol.DiscordAccountInfo{Linked: true, Username: link.Username, Name: link.Name}
		}
	}
	info.Email = h.emailInfo(u.AccountID)
	if h.rankedConfig != nil {
		for _, ladder := range h.rankedConfig.Ladders {
			for _, ruleset := range ladder.Rulesets {
				if standing := h.standing(u.AccountID, ladder.ID, ruleset); standing != nil {
					info.Standings = append(info.Standings, *standing)
				}
			}
		}
	}
	return info
}

// ladderInfo lists the configured ladders for HELLO_OK.
func (h *Hub) ladderInfo() []protocol.LadderInfo {
	if h.accounts == nil || h.rankedConfig == nil {
		return nil
	}
	tiers := make([]string, 0, len(h.rankedConfig.Tiers.Bands))
	for _, band := range h.rankedConfig.Tiers.Bands {
		tiers = append(tiers, band.Name)
	}
	out := make([]protocol.LadderInfo, 0, len(h.rankedConfig.Ladders))
	for _, l := range h.rankedConfig.Ladders {
		info := protocol.LadderInfo{
			ID: l.ID, Name: l.Name, ModuleID: l.ModuleID,
			Rulesets: l.Rulesets, RulesetNames: l.RulesetNames, BestOf: l.BestOf, Tiers: tiers,
			TeamSize: l.TeamSize,
		}
		for _, rom := range l.Roms {
			info.RomMD5s = append(info.RomMD5s, rom.MD5)
		}
		out = append(out, info)
	}
	return out
}

func (h *Hub) handleAccountChangePassword(c *Client, env protocol.Envelope) {
	var req protocol.AccountChangePasswordRequest
	if err := json.Unmarshal(env.Data, &req); err != nil {
		return
	}
	fail := func(reason, message string) {
		c.sendEnvelope(protocol.TypeAccountPasswordFail, env.ID,
			protocol.AccountPasswordFailPayload{Reason: reason, Message: message})
	}
	if h.accounts == nil || !c.user.Verified() {
		fail(protocol.ReasonNotAuthorized, "Log in to change your password.")
		return
	}
	now := time.Now()
	if !h.authFailures.allowed(c.ip, authFailLimit, authFailWindow, now) {
		fail(protocol.ReasonTooManyAttempts, "Too many failed attempts. Try again later.")
		return
	}
	err := h.accounts.ChangePassword(c.user.AccountID, req.CurrentPassword, req.NewPassword)
	switch {
	case errors.Is(err, accounts.ErrBadCredentials):
		h.authFailures.record(c.ip, now)
		fail(protocol.ReasonAuthFailed, "Your current password is wrong.")
		return
	case errors.Is(err, accounts.ErrWeakPassword):
		fail(protocol.ReasonWeakPassword, "Passwords must be 8 to 128 characters.")
		return
	case err != nil:
		slog.Error("password change failed", "user", c.user.Username, "err", err)
		fail(protocol.ReasonAccountsUnavailable, "Accounts are temporarily unavailable.")
		return
	}

	// every saved login was revoked, this connection keeps a fresh one
	token, err := h.accounts.CreateSession(c.user.AccountID)
	if err != nil {
		slog.Error("session creation failed", "err", err)
	}
	c.user.SessionToken = token
	slog.Info("password changed", "user", c.user.Username, "ip", c.ip)
	c.sendEnvelope(protocol.TypeAccountPasswordChanged, env.ID,
		protocol.AccountPasswordChangedPayload{SessionToken: token})
}

func (h *Hub) handleAccountLogout(c *Client, env protocol.Envelope) {
	_ = env
	if h.accounts == nil || c.user.SessionToken == "" {
		return
	}
	if err := h.accounts.RevokeSession(c.user.SessionToken); err != nil {
		slog.Warn("session revoke failed", "user", c.user.Username, "err", err)
	}
}
