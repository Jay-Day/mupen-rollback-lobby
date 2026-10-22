package ws

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/jay-day/rmgk-lobby/internal/accounts"
	"github.com/jay-day/rmgk-lobby/internal/mail"
	"github.com/jay-day/rmgk-lobby/internal/protocol"
)

// Account email: an optional, confirmed address the lobby can email a
// password reset code to. It exists only when SMTP is configured
// (EnableEmail); otherwise resets go through a moderator's /pwreset code.

const (
	// Confirmation emails per account.
	emailCodeLimit  = 5
	emailCodeWindow = time.Hour
	// Password reset requests per IP, and reset emails per account.
	resetRequestLimit  = 5
	resetRequestWindow = 15 * time.Minute
	resetEmailLimit    = 3
	resetEmailWindow   = time.Hour
)

// EnableEmail turns on account email.
func (h *Hub) EnableEmail(sender mail.Sender) {
	h.mailer = sender
}

// emailInfo is HELLO_OK's account email section: nil when the server can't
// send email, so clients hide the email options.
func (h *Hub) emailInfo(accountID int64) *protocol.AccountEmailInfo {
	if h.mailer == nil {
		return nil
	}
	email, err := h.accounts.AccountEmail(accountID)
	if err != nil {
		slog.Warn("account email lookup failed", "account", accountID, "err", err)
	}
	return &protocol.AccountEmailInfo{Address: email}
}

func emailCodeMinutes() int {
	return int(accounts.EmailCodeTTL / time.Minute)
}

func confirmationEmail(username, code string) (subject, body string) {
	return "Confirm your email for RMG-K", fmt.Sprintf(`Hi %s,

Enter this code in RMG-K to use this address for your lobby account:

    %s

The code expires in %d minutes. The address is only used to email you a
password reset code when you ask for one.

If you didn't ask for this, you can ignore this email.
`, username, code, emailCodeMinutes())
}

func passwordResetEmail(username, code string) (subject, body string) {
	return "Your RMG-K password reset code", fmt.Sprintf(`Hi %s,

Someone asked to reset the password of your RMG-K lobby account.
Your reset code:

    %s

In RMG-K's connect window, choose "Forgot password?", then enter this code
and a new password. The code works once and expires in %d minutes.
Resetting your password signs out your other saved logins.

If you didn't ask for this, you can ignore this email: your password
hasn't changed.
`, username, code, emailCodeMinutes())
}

// emailFail answers an account email request with ACCOUNT_EMAIL_FAIL.
func emailFail(c *Client, id string) func(reason, message string) {
	return func(reason, message string) {
		c.sendEnvelope(protocol.TypeAccountEmailFail, id,
			protocol.AccountPasswordFailPayload{Reason: reason, Message: message})
	}
}

func (h *Hub) handleAccountSetEmail(c *Client, env protocol.Envelope) {
	var req protocol.AccountSetEmailRequest
	if err := json.Unmarshal(env.Data, &req); err != nil {
		return
	}
	fail := emailFail(c, env.ID)
	if h.accounts == nil || !c.user.Verified() {
		fail(protocol.ReasonNotAuthorized, "Log in to add an email address.")
		return
	}
	if h.mailer == nil {
		fail(protocol.ReasonEmailUnavailable, "This server can't send email.")
		return
	}
	accountID, username := c.user.AccountID, c.user.Username
	key := "confirm:" + strconv.FormatInt(accountID, 10)
	now := time.Now()
	if !h.emailSends.allowed(key, emailCodeLimit, emailCodeWindow, now) {
		fail(protocol.ReasonTooManyAttempts, "You've asked for several codes already. Try again in an hour.")
		return
	}
	code, email, err := h.accounts.StartEmailChange(accountID, req.Email)
	switch {
	case errors.Is(err, accounts.ErrBadEmail):
		fail(protocol.ReasonInvalidEmail, "Enter an email address like name@example.com.")
		return
	case err != nil:
		slog.Error("email change failed", "user", username, "err", err)
		fail(protocol.ReasonAccountsUnavailable, "Accounts are temporarily unavailable.")
		return
	}
	h.emailSends.record(key, now)

	// answer once the mail server has accepted (or refused) the email
	go func() {
		subject, body := confirmationEmail(username, code)
		if err := h.mailer.Send(email, subject, body); err != nil {
			slog.Error("confirmation email failed", "user", username, "err", err)
			fail(protocol.ReasonEmailFailed, "The email couldn't be sent. Check the address, or try again later.")
			return
		}
		slog.Info("confirmation email sent", "user", username)
		c.sendEnvelope(protocol.TypeAccountEmailCodeSent, env.ID, protocol.AccountEmailInfo{Address: email})
	}()
}

func (h *Hub) handleAccountConfirmEmail(c *Client, env protocol.Envelope) {
	var req protocol.AccountConfirmEmailRequest
	if err := json.Unmarshal(env.Data, &req); err != nil {
		return
	}
	fail := emailFail(c, env.ID)
	if h.accounts == nil || !c.user.Verified() {
		fail(protocol.ReasonNotAuthorized, "Log in to add an email address.")
		return
	}
	now := time.Now()
	if !h.authFailures.allowed(c.ip, authFailLimit, authFailWindow, now) {
		fail(protocol.ReasonTooManyAttempts, "Too many failed attempts. Try again later.")
		return
	}
	email, err := h.accounts.ConfirmEmailChange(c.user.AccountID, req.Code)
	switch {
	case errors.Is(err, accounts.ErrBadEmailCode):
		h.authFailures.record(c.ip, now)
		fail(protocol.ReasonAuthFailed, "That code is wrong or has expired. Send a new one.")
		return
	case err != nil:
		slog.Error("email confirmation failed", "user", c.user.Username, "err", err)
		fail(protocol.ReasonAccountsUnavailable, "Accounts are temporarily unavailable.")
		return
	}
	slog.Info("email confirmed", "user", c.user.Username)
	c.sendEnvelope(protocol.TypeAccountEmail, env.ID, protocol.AccountEmailInfo{Address: email})
}

func (h *Hub) handleAccountRemoveEmail(c *Client, env protocol.Envelope) {
	fail := emailFail(c, env.ID)
	if h.accounts == nil || !c.user.Verified() {
		fail(protocol.ReasonNotAuthorized, "Log in to change your email address.")
		return
	}
	if err := h.accounts.RemoveEmail(c.user.AccountID); err != nil {
		slog.Error("email removal failed", "user", c.user.Username, "err", err)
		fail(protocol.ReasonAccountsUnavailable, "Accounts are temporarily unavailable.")
		return
	}
	slog.Info("email removed", "user", c.user.Username)
	c.sendEnvelope(protocol.TypeAccountEmail, env.ID, protocol.AccountEmailInfo{})
}

// RequestPasswordResetEmail emails a reset code to the account's confirmed
// address when email is that address, so a player's name alone can't send
// reset emails to their inbox. Callers give every requester the same answer,
// so nobody learns which accounts exist or what their addresses are. It
// returns a wait > 0 only when ip has made too many requests. The email is
// sent in the background.
func (h *Hub) RequestPasswordResetEmail(username, email, ip string) time.Duration {
	if h.mailer == nil || h.accounts == nil {
		return 0
	}
	now := time.Now()
	ipKey := "reset-ip:" + ip
	if !h.emailSends.allowed(ipKey, resetRequestLimit, resetRequestWindow, now) {
		return h.emailSends.retryAfter(ipKey, resetRequestLimit, resetRequestWindow, now)
	}
	h.emailSends.record(ipKey, now)

	username = strings.TrimSpace(username)
	if !isValidUsername(username) {
		return 0
	}
	account, err := h.accounts.AccountByName(username)
	if err != nil {
		slog.Info("password reset email: no such account", "user", username, "ip", ip)
		return 0
	}
	accountKey := "reset:" + strconv.FormatInt(account.ID, 10)
	if !h.emailSends.allowed(accountKey, resetEmailLimit, resetEmailWindow, now) {
		slog.Info("password reset email: account limit reached", "user", account.Username, "ip", ip)
		return 0
	}
	code, _, address, err := h.accounts.CreateEmailPasswordReset(account.Username, email)
	switch {
	case errors.Is(err, accounts.ErrNoEmail):
		slog.Info("password reset email: account has no email", "user", account.Username, "ip", ip)
		return 0
	case errors.Is(err, accounts.ErrEmailMismatch):
		slog.Info("password reset email: address doesn't match", "user", account.Username, "ip", ip)
		return 0
	case err != nil:
		slog.Error("password reset email failed", "user", username, "err", err)
		return 0
	}
	h.emailSends.record(accountKey, now)
	go func() {
		subject, body := passwordResetEmail(account.Username, code)
		if err := h.mailer.Send(address, subject, body); err != nil {
			slog.Error("password reset email failed", "user", account.Username, "err", err)
			return
		}
		slog.Info("password reset email sent", "user", account.Username, "ip", ip)
	}()
	return 0
}
