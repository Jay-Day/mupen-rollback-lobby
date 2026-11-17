package modlog

import (
	"database/sql"
	"encoding/json"
	"log/slog"

	"github.com/jay-day/rmgk-lobby/internal/ratelimit"
)

// The request limiter's report, kept across restarts. The lobby holds the
// live copy in memory (ratelimit.ActionLimiter) and saves what changed, in
// one queued write, at most every half minute; at startup it reads it back.

// SaveRateLimits writes upsert's records and removes remove's IPs, in one
// queued write. It returns false when the queue was full (nothing written;
// try again later). A nil Logger saves nothing and reports true.
func (l *Logger) SaveRateLimits(upsert []ratelimit.ActionRecord, remove []string) bool {
	if l == nil || (len(upsert) == 0 && len(remove) == 0) {
		return true
	}
	type row struct {
		ip       string
		lastTrip int64
		record   []byte
	}
	rows := make([]row, 0, len(upsert))
	for _, r := range upsert {
		r.Allowed = nil // the current windows aren't saved
		data, err := json.Marshal(r)
		if err != nil || r.IP == "" {
			continue
		}
		rows = append(rows, row{r.IP, r.LastTrip.Unix(), data})
	}
	remove = append([]string(nil), remove...)
	return l.enqueue(func(tx *sql.Tx) error {
		for _, r := range rows {
			if _, err := tx.Exec(`INSERT INTO rate_limits (ip, last_trip, record) VALUES (?, ?, ?)
				ON CONFLICT(ip) DO UPDATE SET last_trip = excluded.last_trip, record = excluded.record`,
				r.ip, r.lastTrip, string(r.record)); err != nil {
				return err
			}
		}
		for _, ip := range remove {
			if _, err := tx.Exec(`DELETE FROM rate_limits WHERE ip = ?`, ip); err != nil {
				return err
			}
		}
		return nil
	})
}

// RateLimits reads every saved record, most recently refused first. A row
// that doesn't parse is skipped (and logged).
func (l *Logger) RateLimits() ([]ratelimit.ActionRecord, error) {
	if l == nil {
		return nil, nil
	}
	rows, err := l.db.Query(`SELECT ip, record FROM rate_limits ORDER BY last_trip DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ratelimit.ActionRecord
	for rows.Next() {
		var ip, data string
		if err := rows.Scan(&ip, &data); err != nil {
			return nil, err
		}
		var r ratelimit.ActionRecord
		if err := json.Unmarshal([]byte(data), &r); err != nil {
			slog.Warn("modlog: skipping a rate limit record that doesn't parse", "ip", ip, "err", err)
			continue
		}
		r.IP = ip
		out = append(out, r)
	}
	return out, rows.Err()
}
