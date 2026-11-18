// Package modlog is the moderators' record of what happens in the lobby:
// chat, rooms, games and moderator actions. Each is one structured line in
// the server log (journald on the servers), so moderators read history with
// journalctl and grep; how far back it goes is journald's retention.
//
// The one thing kept in a file of its own is the request limiter's report
// (which IPs hit a rate limit), so the Rate limits page survives restarts.
//
// Every method is safe on a nil *Logger: the lines are still logged, and the
// rate limit report just isn't saved.
package modlog

import (
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"sort"
	"strings"
	"sync"

	"github.com/jay-day/rmgk-lobby/internal/ratelimit"
)

// Logger saves the rate limit report; the log lines need no state.
type Logger struct {
	mu        sync.Mutex
	path      string // the rate limit report's file, "" = not saved
	rateLimit map[string]ratelimit.ActionRecord
}

// Open reads the rate limit report at path (a missing file is an empty
// report) and saves later changes there. An empty path keeps nothing.
func Open(path string) (*Logger, error) {
	l := &Logger{path: path, rateLimit: make(map[string]ratelimit.ActionRecord)}
	if path == "" {
		return l, nil
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return l, nil
	}
	if err != nil {
		return nil, err
	}
	var list []ratelimit.ActionRecord
	if len(strings.TrimSpace(string(data))) > 0 {
		if err := json.Unmarshal(data, &list); err != nil {
			return nil, err
		}
	}
	for _, r := range list {
		if r.IP != "" {
			l.rateLimit[r.IP] = r
		}
	}
	return l, nil
}

// Close is here for symmetry with Open; the report is written on every save.
func (l *Logger) Close() error { return nil }

// RateLimits returns the saved report, most recently refused first.
func (l *Logger) RateLimits() ([]ratelimit.ActionRecord, error) {
	if l == nil {
		return nil, nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]ratelimit.ActionRecord, 0, len(l.rateLimit))
	for _, r := range l.rateLimit {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].LastTrip.After(out[j].LastTrip) })
	return out, nil
}

// SaveRateLimits applies upsert's records and drops remove's IPs, then
// rewrites the file. It returns false when the write failed (the caller
// tries again with its next save). A nil Logger saves nothing and reports
// true.
func (l *Logger) SaveRateLimits(upsert []ratelimit.ActionRecord, remove []string) bool {
	if l == nil || l.path == "" || (len(upsert) == 0 && len(remove) == 0) {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, r := range upsert {
		if r.IP == "" {
			continue
		}
		r.Allowed = nil // the current windows aren't saved
		l.rateLimit[r.IP] = r
	}
	for _, ip := range remove {
		delete(l.rateLimit, ip)
	}
	list := make([]ratelimit.ActionRecord, 0, len(l.rateLimit))
	for _, r := range l.rateLimit {
		list = append(list, r)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].IP < list[j].IP })
	data, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		slog.Error("rate limit report not saved: marshal failed", "err", err)
		return false
	}
	// Same as bans.json: write beside it, then rename over it, so a crash
	// never leaves half a file.
	tmp := l.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		slog.Error("rate limit report not saved", "path", tmp, "err", err)
		return false
	}
	if err := os.Rename(tmp, l.path); err != nil {
		slog.Error("rate limit report not saved", "path", l.path, "err", err)
		return false
	}
	return true
}
