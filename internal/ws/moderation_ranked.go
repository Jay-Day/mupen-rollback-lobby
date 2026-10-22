package ws

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jay-day/rmgk-lobby/internal/accounts"
	"github.com/jay-day/rmgk-lobby/internal/protocol"
)

// Moderator tools for ranked accounts and sets (see handleModAction):
//
//	disputes                         list sets waiting for review
//	resolve <setId> <winner|void>    rate a disputed set for winner, or void it
//	pwreset <username>               one-time code to set a new password

const maxListedDisputes = 20

// describeReports summarizes the players' MATCH_RESULT reports of a set.
func describeReports(resultJSON string) string {
	var reports map[string]protocol.MatchResultRequest
	if json.Unmarshal([]byte(resultJSON), &reports) != nil || len(reports) == 0 {
		return "no reports"
	}
	keys := make([]string, 0, len(reports))
	for key := range reports {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		r := reports[key]
		score := make([]string, 0, len(r.Score))
		for _, games := range r.Score {
			score = append(score, strconv.Itoa(games))
		}
		part := fmt.Sprintf("%s: P%d won %s", key, r.WinnerSlot, strings.Join(score, "-"))
		if r.Integrity != "" && r.Integrity != "ok" {
			part += " (" + r.Integrity + ")"
		}
		parts = append(parts, part)
	}
	return "reports: " + strings.Join(parts, " / ")
}

func (h *Hub) modListDisputes(c *Client) {
	if h.accounts == nil {
		c.notice("error", "This server has no ranked sets.")
		return
	}
	sets, err := h.accounts.DisputedSets(maxListedDisputes)
	if err != nil {
		slog.Error("listing disputed sets failed", "err", err)
		c.notice("error", "Couldn't list disputed sets.")
		return
	}
	if len(sets) == 0 {
		c.notice("info", "No disputed sets.")
		return
	}
	for _, set := range sets {
		players := make([]string, 0, len(set.Players))
		for _, p := range set.Players {
			players = append(players, fmt.Sprintf("P%d %s", p.Slot, p.Username))
		}
		c.notice("info", fmt.Sprintf("Set #%d (%s, %s): %s; %s; %s", set.ID, set.Ladder,
			set.CreatedAt.UTC().Format("2006-01-02 15:04 UTC"), strings.Join(players, " vs "),
			set.Reason, describeReports(set.ResultJSON)))
	}
	c.notice("info", "Settle one with: /resolve <setId> <winner username|void> [note]")
}

func (h *Hub) modResolveSet(c *Client, target, note string) {
	if h.accounts == nil {
		c.notice("error", "This server has no ranked sets.")
		return
	}
	fields := strings.Fields(target)
	setID, err := strconv.ParseInt(firstField(fields, 0), 10, 64)
	if err != nil || setID <= 0 || len(fields) != 2 {
		c.notice("error", "Usage: /resolve <setId> <winner username|void> [note]")
		return
	}
	set, err := h.accounts.SetByID(setID)
	if errors.Is(err, accounts.ErrNotFound) {
		c.notice("warn", fmt.Sprintf("Set #%d doesn't exist.", setID))
		return
	}
	if err != nil {
		c.notice("error", "Couldn't load that set.")
		return
	}

	var winnerID int64
	winnerName := ""
	if !strings.EqualFold(fields[1], "void") {
		for _, p := range set.Players {
			if strings.EqualFold(p.Username, fields[1]) {
				winnerID, winnerName = p.AccountID, p.Username
			}
		}
		if winnerID == 0 {
			c.notice("warn", fmt.Sprintf("%s didn't play set #%d.", fields[1], setID))
			return
		}
	}
	reason := "resolved by " + c.adminName
	if note != "" {
		reason += ": " + note
	}

	outcomes, err := h.accounts.ResolveSet(setID, winnerID, reason)
	switch {
	case errors.Is(err, accounts.ErrNotDisputed):
		c.notice("warn", fmt.Sprintf("Set #%d isn't disputed (%s).", setID, set.Status))
		return
	case err != nil:
		slog.Error("resolving set failed", "set", setID, "err", err)
		c.notice("error", "Couldn't resolve that set: "+err.Error())
		return
	}
	if winnerID != 0 {
		h.modConfirm(c, fmt.Sprintf("Set #%d rated: %s wins.", setID, winnerName))
		h.auditLog(c.adminName, "resolve", fmt.Sprintf("set #%d rated, %s wins (lobby chat)%s", setID, winnerName, noteSuffix(note)))
	} else {
		h.modConfirm(c, fmt.Sprintf("Set #%d voided.", setID))
		h.auditLog(c.adminName, "resolve", fmt.Sprintf("set #%d voided (lobby chat)%s", setID, noteSuffix(note)))
	}
	h.notifyResolvedSet(set, outcomes, winnerID, reason)
}

// NotifySetResolved tells the set's online players about a resolution made
// elsewhere (the website's admin pages).
func (h *Hub) NotifySetResolved(set accounts.SetSummary, outcomes []accounts.PlayerOutcome, winnerID int64, reason string) {
	h.notifyResolvedSet(set, outcomes, winnerID, reason)
}

// DisconnectAccount drops every connection logged in to the account (a
// moderator disabled it).
func (h *Hub) DisconnectAccount(accountID int64, reason string) {
	h.mu.Lock()
	var targets []*Client
	for _, client := range h.clients {
		if client.user.AccountID == accountID {
			targets = append(targets, client)
		}
	}
	h.mu.Unlock()
	for _, client := range targets {
		// let the notice flush before closing (like a session takeover)
		client.notice("error", reason)
		go func(c *Client) {
			time.Sleep(300 * time.Millisecond)
			c.disconnect("")
		}(client)
	}
}

func (h *Hub) auditLog(admin, action, detail string) {
	if h.accounts == nil {
		return
	}
	if err := h.accounts.AddAdminLog(admin, action, detail); err != nil {
		slog.Warn("admin log write failed", "err", err)
	}
}

func noteSuffix(note string) string {
	if note == "" {
		return ""
	}
	return ": " + note
}

// notifyResolvedSet sends the resolution to the set's players who are online,
// so their lobby standings update like after any ranked set.
func (h *Hub) notifyResolvedSet(set accounts.SetSummary, outcomes []accounts.PlayerOutcome, winnerID int64, reason string) {
	if h.rankedConfig == nil {
		return
	}
	payload := protocol.RankedResultPayload{SetID: set.ID, Status: string(accounts.SetVoided), Reason: reason}
	if winnerID != 0 {
		payload.Status = string(accounts.SetRated)
	}
	tiers := h.rankedConfig.Tiers
	byAccount := make(map[int64]accounts.PlayerOutcome, len(outcomes))
	for _, o := range outcomes {
		byAccount[o.AccountID] = o
	}
	for _, p := range set.Players {
		if p.AccountID == winnerID {
			if p.Team != 0 {
				payload.WinnerTeam = p.Team
			} else {
				payload.WinnerSlot = p.Slot
			}
		}
		outcome, ok := byAccount[p.AccountID]
		if !ok {
			continue
		}
		policy := h.rankedConfig.Policy(set.Ladder)
		before := standingFrom(accounts.LadderRating{Ladder: set.Ladder, Ruleset: set.RulesetID, Rating: outcome.Before}, tiers, policy)
		after := standingFrom(accounts.LadderRating{Ladder: set.Ladder, Ruleset: set.RulesetID, Rating: outcome.After}, tiers, policy)
		if current := h.standing(p.AccountID, set.Ladder, set.RulesetID); current != nil {
			before.SetsPlayed, after.SetsPlayed = current.SetsPlayed, current.SetsPlayed
			if winnerID != 0 {
				before.SetsPlayed--
			}
		}
		payload.Players = append(payload.Players, protocol.RankedResultPlayer{
			Slot: p.Slot, Team: p.Team, Username: p.Username, Before: before, After: after,
		})
	}

	h.mu.Lock()
	var recipients []*Client
	for _, client := range h.clients {
		for _, p := range set.Players {
			if client.user.AccountID == p.AccountID {
				recipients = append(recipients, client)
			}
		}
	}
	h.mu.Unlock()
	for _, client := range recipients {
		client.sendEnvelope(protocol.TypeRankedResult, "", payload)
	}
}

func (h *Hub) modPasswordReset(c *Client, username string) {
	if h.accounts == nil {
		c.notice("error", "This server has no accounts.")
		return
	}
	if username == "" {
		c.notice("error", "Usage: /pwreset <username>")
		return
	}
	code, account, err := h.accounts.CreatePasswordReset(username, c.adminName)
	if errors.Is(err, accounts.ErrNotFound) {
		c.notice("warn", "No account named "+username+".")
		return
	}
	if err != nil {
		slog.Error("password reset failed", "user", username, "err", err)
		c.notice("error", "Couldn't create a reset code.")
		return
	}
	slog.Info("mod action", "admin", c.adminName, "detail", "password reset code issued for "+account.Username)
	h.auditLog(c.adminName, "reset-code", "reset code issued for "+account.Username+" (lobby chat)")
	c.notice("info", fmt.Sprintf("Reset code for %s: %s (valid %d hours, works once). "+
		"Give it to them privately; they enter it under \"Forgot password?\" when connecting.",
		account.Username, code, int(accounts.ResetCodeTTL/time.Hour)))
}

func firstField(fields []string, i int) string {
	if i < len(fields) {
		return fields[i]
	}
	return ""
}
