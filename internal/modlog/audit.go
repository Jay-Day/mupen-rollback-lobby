package modlog

import (
	"context"
	"log/slog"
)

// The moderators' audit trail. Unlike the rest of the log it is written at
// once (not queued), so a full queue can never lose a moderator's action, and
// it is never pruned.

// Where an audited action came from.
const (
	SourceSite  = "site"  // the moderator website
	SourceLobby = "lobby" // a MOD_ACTION or ADMIN_AUTH in the lobby
	SourceAuto  = "auto"  // the server itself (the login guard's bans)
)

// AuditEntry is one moderator action.
type AuditEntry struct {
	Admin  string // the moderator's name, "auto" for the server
	Action string // sign-in, kick, mute, timeout, ban, unban, unmute, closeroom, ...
	Detail string
	Source string // SourceSite, SourceLobby or SourceAuto
	IP     string // where the moderator was ("" when not known)
}

// Audit writes one entry. It also goes to the server log, so the trail
// survives with logging off.
func (l *Logger) Audit(e AuditEntry) {
	slog.Info("moderator action", "admin", e.Admin, "action", e.Action, "detail", e.Detail, "source", e.Source,
		"ip", e.IP)
	if l == nil {
		return
	}
	l.dbMu.RLock()
	defer l.dbMu.RUnlock()
	if l.dbClosed {
		return
	}
	if _, err := l.db.Exec(`INSERT INTO audit (time, admin, action, detail, source, ip) VALUES (?, ?, ?, ?, ?, ?)`,
		l.now().Unix(), e.Admin, e.Action, e.Detail, e.Source, e.IP); err != nil {
		slog.Warn("modlog: audit write failed", "err", err)
	}
}

// AuditRow is one entry as read back.
type AuditRow struct {
	ID int64
	AuditEntry
	Time int64
}

// AuditLog returns one page of entries, newest first, older than before (0 =
// the newest), and whether older ones exist.
func (l *Logger) AuditLog(ctx context.Context, before int64, limit int) ([]AuditRow, bool, error) {
	limit = clampLimit(limit)
	var w where
	if before > 0 {
		w.add("id < ?", before)
	}
	rows, err := l.db.QueryContext(ctx, `SELECT id, time, admin, action, detail, source, ip FROM audit`+w.sql()+
		` ORDER BY id DESC LIMIT ?`, append(w.args, limit+1)...)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	out := []AuditRow{}
	for rows.Next() {
		var r AuditRow
		if err := rows.Scan(&r.ID, &r.Time, &r.Admin, &r.Action, &r.Detail, &r.Source, &r.IP); err != nil {
			return nil, false, err
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	more := len(out) > limit
	if more {
		out = out[:limit]
	}
	return out, more, nil
}
