package modlog

import "log/slog"

// Where a moderator action came from.
const (
	SourceSite  = "site"  // the moderator website
	SourceLobby = "lobby" // a MOD_ACTION or ADMIN_AUTH in the lobby
	SourceAuto  = "auto"  // the server itself (the login guard's and rate limiter's bans)
)

// AuditEntry is one moderator action.
type AuditEntry struct {
	Admin  string // the moderator's name, "auto" for the server
	Action string // sign-in, kick, mute, timeout, ban, unban, unmute, closeroom, ...
	Detail string
	Source string // SourceSite, SourceLobby or SourceAuto
	IP     string // where the moderator was ("" when not known)
}

// Audit writes one entry to the server log.
func (l *Logger) Audit(e AuditEntry) {
	slog.Info("moderator action", "admin", e.Admin, "action", e.Action, "detail", e.Detail, "source", e.Source,
		"ip", e.IP)
}
