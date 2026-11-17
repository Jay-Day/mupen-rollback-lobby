package modlog

import "fmt"

// Times are unix seconds. Names are stored as shown and matched by key() (the
// *_key columns), so case doesn't matter.
//
// The first migration is the same as the ranked lobby branch's, so either
// server can open the other's file: the account, private message and ranked
// columns are never written here and keep their defaults.
var migrations = []string{
	`CREATE TABLE meta (
		key   TEXT PRIMARY KEY,
		value INTEGER NOT NULL
	);
	-- one row per lobby connection, from HELLO to disconnect (0 = still online)
	CREATE TABLE connections (
		id           INTEGER PRIMARY KEY AUTOINCREMENT,
		user_id      INTEGER NOT NULL DEFAULT 0,
		ip           TEXT NOT NULL,
		username     TEXT NOT NULL,
		account_id   INTEGER NOT NULL DEFAULT 0,
		account      TEXT NOT NULL DEFAULT '',
		account_key  TEXT NOT NULL DEFAULT '',
		version      TEXT NOT NULL DEFAULT '',
		country      TEXT NOT NULL DEFAULT '',
		connected    INTEGER NOT NULL,
		disconnected INTEGER NOT NULL DEFAULT 0
	);
	CREATE INDEX connections_ip ON connections(ip, connected);
	CREATE INDEX connections_account ON connections(account_key);
	CREATE INDEX connections_open ON connections(disconnected);
	-- every name used on a connection (here, its owner's)
	CREATE TABLE connection_names (
		conn_id  INTEGER NOT NULL REFERENCES connections(id) ON DELETE CASCADE,
		name     TEXT NOT NULL,
		name_key TEXT NOT NULL,
		local    INTEGER NOT NULL DEFAULT 0,
		added    INTEGER NOT NULL,
		PRIMARY KEY (conn_id, name_key)
	);
	CREATE INDEX connection_names_key ON connection_names(name_key);
	-- blocked: '' delivered, 'filter', 'muted' or 'flood'
	CREATE TABLE chat (
		id         INTEGER PRIMARY KEY AUTOINCREMENT,
		time       INTEGER NOT NULL,
		channel    TEXT NOT NULL,
		room_id    INTEGER NOT NULL DEFAULT 0,
		room_name  TEXT NOT NULL DEFAULT '',
		match_id   INTEGER NOT NULL DEFAULT 0,
		user_id    INTEGER NOT NULL DEFAULT 0,
		name       TEXT NOT NULL,
		name_key   TEXT NOT NULL,
		account_id INTEGER NOT NULL DEFAULT 0,
		ip         TEXT NOT NULL DEFAULT '',
		to_id      INTEGER NOT NULL DEFAULT 0,
		to_name    TEXT NOT NULL DEFAULT '',
		text       TEXT NOT NULL,
		text_fold  TEXT NOT NULL,
		admin      INTEGER NOT NULL DEFAULT 0,
		blocked    TEXT NOT NULL DEFAULT ''
	);
	CREATE INDEX chat_time ON chat(time);
	CREATE INDEX chat_name ON chat(name_key);
	CREATE INDEX chat_ip ON chat(ip);
	CREATE INDEX chat_room ON chat(room_id);
	CREATE INDEX chat_match ON chat(match_id);
	-- one row per match start; ended 0 = still running
	CREATE TABLE games (
		id         INTEGER PRIMARY KEY AUTOINCREMENT,
		match_id   INTEGER NOT NULL,
		room_id    INTEGER NOT NULL,
		room_name  TEXT NOT NULL DEFAULT '',
		game       TEXT NOT NULL DEFAULT '',
		rom_md5    TEXT NOT NULL DEFAULT '',
		ranked     INTEGER NOT NULL DEFAULT 0,
		ladder     TEXT NOT NULL DEFAULT '',
		ruleset    TEXT NOT NULL DEFAULT '',
		set_id     INTEGER NOT NULL DEFAULT 0,
		started    INTEGER NOT NULL,
		ended      INTEGER NOT NULL DEFAULT 0,
		end_reason TEXT NOT NULL DEFAULT '',
		spectators INTEGER NOT NULL DEFAULT 0
	);
	CREATE INDEX games_match ON games(match_id);
	CREATE INDEX games_room ON games(room_id);
	CREATE INDEX games_open ON games(ended);
	CREATE TABLE game_players (
		game_id     INTEGER NOT NULL REFERENCES games(id) ON DELETE CASCADE,
		user_id     INTEGER NOT NULL,
		name        TEXT NOT NULL,
		name_key    TEXT NOT NULL,
		account_id  INTEGER NOT NULL DEFAULT 0,
		account     TEXT NOT NULL DEFAULT '',
		ip          TEXT NOT NULL DEFAULT '',
		slot        INTEGER NOT NULL DEFAULT 0,
		local       INTEGER NOT NULL DEFAULT 0,
		joined      INTEGER NOT NULL,
		joined_late INTEGER NOT NULL DEFAULT 0,
		left_at     INTEGER NOT NULL DEFAULT 0,
		PRIMARY KEY (game_id, user_id)
	);
	CREATE INDEX game_players_name ON game_players(name_key);
	CREATE INDEX game_players_ip ON game_players(ip);
	-- room and game timeline, and moderator sanctions
	CREATE TABLE events (
		id       INTEGER PRIMARY KEY AUTOINCREMENT,
		time     INTEGER NOT NULL,
		type     TEXT NOT NULL,
		room_id  INTEGER NOT NULL DEFAULT 0,
		match_id INTEGER NOT NULL DEFAULT 0,
		name     TEXT NOT NULL DEFAULT '',
		name_key TEXT NOT NULL DEFAULT '',
		ip       TEXT NOT NULL DEFAULT '',
		text     TEXT NOT NULL
	);
	CREATE INDEX events_time ON events(time);
	CREATE INDEX events_room ON events(room_id);
	CREATE INDEX events_match ON events(match_id);
	CREATE INDEX events_name ON events(name_key);`,

	// The moderators' audit trail: who did what, from the website or the
	// lobby, and the automatic bans. Never pruned.
	`CREATE TABLE audit (
		id     INTEGER PRIMARY KEY AUTOINCREMENT,
		time   INTEGER NOT NULL,
		admin  TEXT NOT NULL,
		action TEXT NOT NULL,
		detail TEXT NOT NULL,
		source TEXT NOT NULL DEFAULT '',
		ip     TEXT NOT NULL DEFAULT ''
	);
	CREATE INDEX audit_time ON audit(time);`,

	// The request limiter's report (internal/ratelimit): one row per IP that
	// had a request refused, the record as JSON. The lobby saves its changes
	// every half minute and removes rows it no longer reports on.
	`CREATE TABLE rate_limits (
		ip        TEXT PRIMARY KEY,
		last_trip INTEGER NOT NULL,
		record    TEXT NOT NULL
	);
	CREATE INDEX rate_limits_last ON rate_limits(last_trip);`,

	// A blocked chat line logged as a sample stands for this many more from
	// the same address that weren't logged (the lobby samples blocked lines).
	`ALTER TABLE chat ADD COLUMN suppressed INTEGER NOT NULL DEFAULT 0;`,
}

func (l *Logger) migrate() error {
	var version int
	if err := l.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return err
	}
	for ; version < len(migrations); version++ {
		tx, err := l.db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(migrations[version]); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d: %w", version+1, err)
		}
		if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, version+1)); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}
