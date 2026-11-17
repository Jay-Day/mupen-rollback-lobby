// Package modlog keeps what moderators look back at: lobby connections (with
// their IPs and the name used on each), chat (blocked lines included), games
// and room events, and the moderators' own audit trail. It is a SQLite file of
// its own (RMGK_MODLOG_DB_PATH, default modlog.db next to the server; "off"
// turns it off). Chat, connections and games are pruned by age (lobby.json
// chatLogDays, connectionLogDays, gameLogDays); the audit trail is kept.
//
// The hub never waits on the disk. Every record goes into a bounded queue that
// one writer goroutine drains in batched transactions; when the queue is full
// the record is dropped and counted. Every method on a nil *Logger does
// nothing, so a lobby without logging needs no checks.
//
// The file holds IP addresses and chat, so it is created readable by its
// owner only (where the OS has such permissions).
package modlog

import (
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	_ "modernc.org/sqlite"
)

// ErrNotFound means the requested row doesn't exist.
var ErrNotFound = errors.New("not found")

// queueSize bounds what waits for the writer; a variable so tests can fill it.
var queueSize = 16384

const (
	maxBatch      = 512
	heartbeatTick = time.Minute
	pruneTick     = 3 * time.Hour
	pruneChunk    = 5000
)

// sizeTick is how often the writer checks the size cap; a variable so tests
// can shorten it.
var sizeTick = 10 * time.Minute

// Retention is how many days each kind of row is kept (0 keeps it forever),
// and how big the log may grow (MaxBytes; 0: no cap).
type Retention struct {
	ChatDays       int
	ConnectionDays int
	GameDays       int
	MaxBytes       int64
}

// DefaultRetention matches lobby.json's defaults.
var DefaultRetention = Retention{ChatDays: 90, ConnectionDays: 90, GameDays: 365}

type op struct {
	fn   func(tx *sql.Tx) error
	done chan struct{} // closed once the batch holding it is committed
}

// tables whose ids are assigned before the row is written, so later records
// (a disconnect, a player leaving) can name the row
const (
	tConnections = iota
	tGames
	numCounters
)

// Logger is safe for concurrent use.
type Logger struct {
	db    *sql.DB
	clock atomic.Pointer[func() time.Time]

	retention atomic.Pointer[func() Retention]

	mu     sync.Mutex // orders id assignment with the queue
	closed bool
	queue  chan op
	nextID [numCounters]int64

	// dbMu keeps Close from closing the file under a direct write (Audit).
	dbMu     sync.RWMutex
	dbClosed bool

	dropped  atomic.Int64
	warnMu   sync.Mutex
	lastWarn time.Time

	gmu   sync.Mutex
	games map[uint64]*openGame // by match id

	done chan struct{}
}

type openGame struct {
	id          int64
	roomID      uint64
	players     map[uint64]*openPlayer
	connected   bool
	punchFailed bool
	spectators  int
}

type openPlayer struct {
	name string
	left bool
}

// createPrivate makes sure the file at path exists and only its owner can
// read it. SQLite gives the -wal and -shm files it creates next to it the
// same permissions.
func createPrivate(path string) error {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	f.Close()
	// An older file may have been created with wider permissions.
	if fi, err := os.Stat(path); err == nil && fi.Mode().Perm()&0o077 != 0 {
		if err := os.Chmod(path, 0o600); err != nil {
			slog.Warn("modlog: couldn't make the log file private", "path", path, "err", err)
		}
	}
	return nil
}

// Open opens (creating if needed) the log at path, closes the rows a crash
// left open, and starts the writer.
func Open(path string) (*Logger, error) {
	if strings.ContainsAny(path, "?#") {
		return nil, fmt.Errorf("modlog path %q may not contain ? or #", path)
	}
	if err := createPrivate(path); err != nil {
		return nil, err
	}
	// auto_vacuum(INCREMENTAL) comes first: on a new file it makes pruning
	// give space back (see enforceSize); an existing file keeps its mode
	// (changing it would need a full VACUUM) and reuses freed pages instead.
	// journal_size_limit trims the WAL after a big prune.
	dsn := "file:" + path + "?_pragma=auto_vacuum(INCREMENTAL)&_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)" +
		"&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)&_pragma=journal_size_limit(67108864)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// The writer is one goroutine; the others are the moderator pages reading.
	db.SetMaxOpenConns(4)
	l := &Logger{
		db:    db,
		queue: make(chan op, queueSize),
		games: map[uint64]*openGame{},
		done:  make(chan struct{}),
	}
	if err := l.migrate(); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate %s: %w", path, err)
	}
	if err := l.closeOut(l.lastAlive(), "server restarted"); err != nil {
		db.Close()
		return nil, fmt.Errorf("close open rows in %s: %w", path, err)
	}
	for i, table := range []string{"connections", "games"} {
		var seq, max sql.NullInt64
		_ = db.QueryRow(`SELECT seq FROM sqlite_sequence WHERE name = ?`, table).Scan(&seq)
		if err := db.QueryRow(`SELECT MAX(id) FROM ` + table).Scan(&max); err != nil {
			db.Close()
			return nil, err
		}
		l.nextID[i] = maxInt(seq.Int64, max.Int64)
	}
	l.setAlive(l.now().Unix())
	go l.run()
	return l, nil
}

// SetClock replaces the clock, for tools and tests that make a history with
// its own times.
func (l *Logger) SetClock(now func() time.Time) { l.clock.Store(&now) }

func (l *Logger) now() time.Time {
	if c := l.clock.Load(); c != nil {
		return (*c)()
	}
	return time.Now()
}

// SetRetention sets where the retention days come from (read at every prune).
func (l *Logger) SetRetention(get func() Retention) {
	if l == nil || get == nil {
		return
	}
	l.retention.Store(&get)
}

func (l *Logger) currentRetention() Retention {
	if get := l.retention.Load(); get != nil {
		return (*get)()
	}
	return DefaultRetention
}

// Dropped counts the records lost to a full queue since Open.
func (l *Logger) Dropped() int64 {
	if l == nil {
		return 0
	}
	return l.dropped.Load()
}

// Flush waits until everything logged so far is written.
func (l *Logger) Flush() {
	if l == nil {
		return
	}
	done := make(chan struct{})
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return
	}
	l.queue <- op{done: done}
	l.mu.Unlock()
	<-done
}

// Close writes what's queued, marks every still-open connection and game as
// over, and closes the file.
func (l *Logger) Close() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return nil
	}
	l.closed = true
	close(l.queue)
	l.mu.Unlock()
	<-l.done
	if err := l.closeOut(l.now().Unix(), "server stopped"); err != nil {
		slog.Warn("modlog: closing open rows failed", "err", err)
	}
	l.setAlive(l.now().Unix())
	l.dbMu.Lock()
	defer l.dbMu.Unlock()
	l.dbClosed = true
	return l.db.Close()
}

// sendLocked queues one record. Callers hold l.mu.
func (l *Logger) sendLocked(o op) bool {
	if l.closed {
		return false
	}
	select {
	case l.queue <- o:
		return true
	default:
		l.noteDropped()
		return false
	}
}

func (l *Logger) enqueue(fn func(tx *sql.Tx) error) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.sendLocked(op{fn: fn})
}

// insert queues a row whose id is chosen now; 0 when the queue was full.
func (l *Logger) insert(table int, build func(id int64) func(tx *sql.Tx) error) int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	id := l.nextID[table] + 1
	if !l.sendLocked(op{fn: build(id)}) {
		return 0
	}
	l.nextID[table] = id
	return id
}

func (l *Logger) noteDropped() {
	n := l.dropped.Add(1)
	l.warnMu.Lock()
	defer l.warnMu.Unlock()
	if time.Since(l.lastWarn) < time.Minute {
		return
	}
	l.lastWarn = time.Now()
	slog.Warn("modlog queue full: dropping log records", "dropped_total", n)
}

func (l *Logger) run() {
	defer close(l.done)
	heartbeat := time.NewTicker(heartbeatTick)
	defer heartbeat.Stop()
	prune := time.NewTicker(pruneTick)
	defer prune.Stop()
	size := time.NewTicker(sizeTick)
	defer size.Stop()
	l.prune()
	for {
		select {
		case first, ok := <-l.queue:
			if !ok {
				return
			}
			batch := []op{first}
		fill:
			for len(batch) < maxBatch {
				select {
				case o, ok := <-l.queue:
					if !ok {
						break fill
					}
					batch = append(batch, o)
				default:
					break fill
				}
			}
			l.write(batch)
		case <-heartbeat.C:
			l.setAlive(l.now().Unix())
		case <-prune.C:
			l.prune()
		case <-size.C:
			l.enforceSize()
		}
	}
}

// write runs a batch in one transaction. A record that fails (say, a player
// row for a game whose own row was dropped) is logged and skipped; SQLite
// keeps the rest of the transaction.
func (l *Logger) write(batch []op) {
	defer func() {
		for _, o := range batch {
			if o.done != nil {
				close(o.done)
			}
		}
	}()
	tx, err := l.db.Begin()
	if err != nil {
		l.warnWrite(err)
		return
	}
	failed := 0
	var firstErr error
	for _, o := range batch {
		if o.fn == nil {
			continue
		}
		if err := o.fn(tx); err != nil {
			failed++
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	if err := tx.Commit(); err != nil {
		l.warnWrite(err)
		return
	}
	if failed > 0 {
		l.warnWrite(fmt.Errorf("%d of %d records failed: %w", failed, len(batch), firstErr))
	}
}

func (l *Logger) warnWrite(err error) {
	l.warnMu.Lock()
	defer l.warnMu.Unlock()
	if time.Since(l.lastWarn) < time.Minute {
		return
	}
	l.lastWarn = time.Now()
	slog.Warn("modlog write failed", "err", err)
}

func (l *Logger) lastAlive() int64 {
	var v int64
	if err := l.db.QueryRow(`SELECT value FROM meta WHERE key = 'last_alive'`).Scan(&v); err != nil || v == 0 {
		return l.now().Unix()
	}
	return v
}

func (l *Logger) setAlive(at int64) {
	if _, err := l.db.Exec(`INSERT INTO meta (key, value) VALUES ('last_alive', ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, at); err != nil {
		l.warnWrite(err)
	}
}

// closeOut ends every connection and game still open: the server is
// stopping, or stopped without saying (the last heartbeat is the best guess
// of when).
func (l *Logger) closeOut(at int64, reason string) error {
	if _, err := l.db.Exec(`UPDATE connections SET disconnected = MAX(connected, ?) WHERE disconnected = 0`, at); err != nil {
		return err
	}
	_, err := l.db.Exec(`UPDATE games SET ended = MAX(started, ?), end_reason = ? WHERE ended = 0`, at, reason)
	return err
}

// Prune deletes rows older than the retention days. The writer runs it at
// startup and every few hours.
func (l *Logger) Prune() {
	if l == nil {
		return
	}
	l.prune()
}

func (l *Logger) prune() {
	r := l.currentRetention()
	now := l.now()
	cutoff := func(days int) int64 { return now.Add(-time.Duration(days) * 24 * time.Hour).Unix() }
	total := map[string]int64{}
	run := func(name, query string, days int) {
		if days <= 0 {
			return
		}
		for {
			res, err := l.db.Exec(query, cutoff(days), pruneChunk)
			if err != nil {
				slog.Warn("modlog prune failed", "table", name, "err", err)
				return
			}
			n, _ := res.RowsAffected()
			total[name] += n
			if n < pruneChunk {
				return
			}
		}
	}
	run("chat", `DELETE FROM chat WHERE id IN (SELECT id FROM chat WHERE time < ? LIMIT ?)`, r.ChatDays)
	run("connections", `DELETE FROM connections WHERE id IN
		(SELECT id FROM connections WHERE disconnected != 0 AND disconnected < ? LIMIT ?)`, r.ConnectionDays)
	run("games", `DELETE FROM games WHERE id IN (SELECT id FROM games WHERE ended != 0 AND ended < ? LIMIT ?)`, r.GameDays)
	run("events", `DELETE FROM events WHERE id IN (SELECT id FROM events WHERE time < ? LIMIT ?)`, r.GameDays)
	args := []any{}
	for k, v := range total {
		if v > 0 {
			args = append(args, k, v)
		}
	}
	if len(args) > 0 {
		slog.Info("modlog pruned", args...)
	}
	l.enforceSize()
}

// MaxIDs returns the highest room and match ids logged, so a restarted lobby
// can number past them and an id never names two different rooms or games.
func (l *Logger) MaxIDs() (room, match uint64) {
	if l == nil {
		return 0, 0
	}
	max := func(query string) uint64 {
		var v sql.NullInt64
		_ = l.db.QueryRow(query).Scan(&v)
		if v.Int64 < 0 {
			return 0
		}
		return uint64(v.Int64)
	}
	room = maxU(max(`SELECT MAX(room_id) FROM games`), max(`SELECT MAX(room_id) FROM chat`), max(`SELECT MAX(room_id) FROM events`))
	match = maxU(max(`SELECT MAX(match_id) FROM games`), max(`SELECT MAX(match_id) FROM chat`), max(`SELECT MAX(match_id) FROM events`))
	return room, match
}

func maxInt(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func maxU(vs ...uint64) uint64 {
	var out uint64
	for _, v := range vs {
		if v > out {
			out = v
		}
	}
	return out
}

// key is how names are matched: the lobby compares usernames without regard
// to case (they are ASCII letters, digits and _-.), and so does the log.
func key(name string) string {
	return strings.ToLower(strings.TrimSpace(name))
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
