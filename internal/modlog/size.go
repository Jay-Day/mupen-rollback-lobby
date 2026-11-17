package modlog

import (
	"log/slog"
)

// The size cap (lobby.json modLogMaxMB, Retention.MaxBytes). Age-based
// pruning alone lets a flood of chat grow the file without bound for the
// retention days, so the writer also checks the file's data every sizeTick
// and after every prune. Over the cap, it deletes the oldest chat and
// connection rows, a chunk of each at a time, until the data is 10% under
// the cap; only when those are gone does it touch games and room events. The
// audit trail and the rate-limit report are never pruned.
//
// Freed space: a file made by this version uses auto_vacuum=INCREMENTAL, so
// the freed pages are returned to the disk right away. An older file
// (auto_vacuum NONE) keeps its size but reuses the freed pages for new rows,
// so it stops growing; a VACUUM (with the server stopped) would shrink it.

const shrinkChunk = 2000

// dataBytes is how much of the file holds data: its pages, less the free ones.
func (l *Logger) dataBytes() (int64, error) {
	var pages, free, pageSize int64
	if err := l.db.QueryRow(`PRAGMA page_count`).Scan(&pages); err != nil {
		return 0, err
	}
	if err := l.db.QueryRow(`PRAGMA freelist_count`).Scan(&free); err != nil {
		return 0, err
	}
	if err := l.db.QueryRow(`PRAGMA page_size`).Scan(&pageSize); err != nil {
		return 0, err
	}
	return (pages - free) * pageSize, nil
}

// enforceSize keeps the log under its cap and gives freed space back. It
// runs on the writer goroutine.
func (l *Logger) enforceSize() {
	if capBytes := l.currentRetention().MaxBytes; capBytes > 0 {
		if size, err := l.dataBytes(); err != nil {
			slog.Warn("modlog: couldn't read its size", "err", err)
		} else if size > capBytes {
			l.shrink(size, capBytes)
		}
	}
	l.reclaim()
}

func (l *Logger) shrink(size, capBytes int64) {
	target := capBytes - capBytes/10
	slog.Warn("moderator log is over its size cap (lobby.json modLogMaxMB): pruning its oldest chat and connections",
		"size_mb", size>>20, "cap_mb", capBytes>>20)
	first := []struct{ table, query string }{
		{"chat", `DELETE FROM chat WHERE id IN (SELECT id FROM chat ORDER BY id LIMIT ?)`},
		{"connections", `DELETE FROM connections WHERE id IN
			(SELECT id FROM connections WHERE disconnected != 0 ORDER BY id LIMIT ?)`},
	}
	then := []struct{ table, query string }{
		{"events", `DELETE FROM events WHERE id IN (SELECT id FROM events ORDER BY id LIMIT ?)`},
		{"games", `DELETE FROM games WHERE id IN (SELECT id FROM games WHERE ended != 0 ORDER BY id LIMIT ?)`},
	}
	// a twentieth of the rows at a time (between 100 and shrinkChunk), so it
	// stops close to the target
	var rows int64
	l.db.QueryRow(`SELECT (SELECT COUNT(*) FROM chat) + (SELECT COUNT(*) FROM connections)`).Scan(&rows)
	chunk := min(max(rows/20, 100), shrinkChunk)
	deleted := map[string]int64{}
	run := func(steps []struct{ table, query string }) int64 {
		var n int64
		for _, st := range steps {
			res, err := l.db.Exec(st.query, chunk)
			if err != nil {
				slog.Warn("modlog: pruning to the size cap failed", "table", st.table, "err", err)
				continue
			}
			k, _ := res.RowsAffected()
			deleted[st.table] += k
			n += k
		}
		return n
	}
	for {
		if run(first) == 0 && run(then) == 0 {
			break // nothing left that may be pruned
		}
		var err error
		if size, err = l.dataBytes(); err != nil || size <= target {
			break
		}
	}
	args := []any{"size_mb", size >> 20, "cap_mb", capBytes >> 20}
	for _, t := range []string{"chat", "connections", "events", "games"} {
		if deleted[t] > 0 {
			args = append(args, t, deleted[t])
		}
	}
	if size > capBytes {
		slog.Warn("moderator log still over its size cap: what's left (the audit log, the rate-limit report, "+
			"open connections and running games) is never pruned", args...)
		return
	}
	slog.Warn("moderator log pruned to its size cap", args...)
}

// reclaim gives free pages back to the disk (files with incremental
// auto-vacuum) and trims the WAL.
func (l *Logger) reclaim() {
	var mode, free int64
	if l.db.QueryRow(`PRAGMA auto_vacuum`).Scan(&mode) != nil || l.db.QueryRow(`PRAGMA freelist_count`).Scan(&free) != nil {
		return
	}
	if mode != 2 || free == 0 {
		return
	}
	if _, err := l.db.Exec(`PRAGMA incremental_vacuum`); err != nil {
		slog.Warn("modlog: incremental vacuum failed", "err", err)
		return
	}
	if _, err := l.db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		slog.Warn("modlog: checkpoint failed", "err", err)
	}
}

// SizeStats reports the log's data size and its free pages' size, for
// tests and the server log.
func (l *Logger) SizeStats() (data, free int64, autoVacuum int, err error) {
	if l == nil {
		return 0, 0, 0, nil
	}
	if data, err = l.dataBytes(); err != nil {
		return
	}
	var pages, pageSize int64
	if err = l.db.QueryRow(`PRAGMA freelist_count`).Scan(&pages); err != nil {
		return
	}
	if err = l.db.QueryRow(`PRAGMA page_size`).Scan(&pageSize); err != nil {
		return
	}
	err = l.db.QueryRow(`PRAGMA auto_vacuum`).Scan(&autoVacuum)
	return data, pages * pageSize, autoVacuum, err
}
