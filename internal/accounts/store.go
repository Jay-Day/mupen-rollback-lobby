// Package accounts persists registered users, login sessions, per-ladder
// ratings and ranked set history in an embedded SQLite database.
//
// The rest of the lobby stays in-memory (a restart wipes rooms and presence);
// only what must survive restarts lives here.
package accounts

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/jay-day/rmgk-lobby/internal/rating"

	_ "modernc.org/sqlite"
)

const (
	MinPasswordLen = 8
	MaxPasswordLen = 128

	// SessionTTL is how long a remember-me token stays valid without use.
	SessionTTL = 30 * 24 * time.Hour
)

var (
	ErrUsernameTaken   = errors.New("username already registered")
	ErrBadCredentials  = errors.New("wrong username or password")
	ErrWeakPassword    = errors.New("password must be 8-128 characters")
	ErrInvalidSession  = errors.New("session expired or invalid")
	ErrAccountDisabled = errors.New("account disabled")
)

// Account is a registered user.
type Account struct {
	ID        int64
	Username  string // canonical casing chosen at registration
	CreatedAt time.Time
}

// LadderRating is an account's rating on one ladder, with inactivity applied.
type LadderRating struct {
	Ladder string
	// Ruleset is the roster: each roster of a ladder is rated separately.
	Ruleset    string
	Rating     rating.Rating
	SetsPlayed int
	LastPlayed time.Time // zero when no sets played
}

// Store is safe for concurrent use.
type Store struct {
	db  *sql.DB
	now func() time.Time

	// homes caches HomeRegions until the next recorded set
	homesMu sync.Mutex
	homes   map[int64]string
}

// Open opens (creating if needed) the database at path and migrates it.
func Open(path string) (*Store, error) {
	dsn := "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// One connection serializes writers; the lobby's write rate is tiny.
	db.SetMaxOpenConns(1)
	s := &Store{db: db, now: time.Now}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate %s: %w", path, err)
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

// SetClock replaces the store's clock, for tools that import or generate a
// set history with its original times.
func (s *Store) SetClock(now func() time.Time) { s.now = now }

var migrations = []string{
	`CREATE TABLE accounts (
		id          INTEGER PRIMARY KEY,
		username    TEXT NOT NULL UNIQUE COLLATE NOCASE,
		pw_hash     TEXT NOT NULL,
		created_at  INTEGER NOT NULL,
		disabled    INTEGER NOT NULL DEFAULT 0
	);
	CREATE TABLE sessions (
		token_hash  TEXT PRIMARY KEY,
		account_id  INTEGER NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
		expires_at  INTEGER NOT NULL
	);
	CREATE INDEX sessions_account ON sessions(account_id);
	CREATE TABLE ratings (
		account_id  INTEGER NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
		ladder      TEXT NOT NULL,
		rating      REAL NOT NULL,
		deviation   REAL NOT NULL,
		volatility  REAL NOT NULL,
		sets_played INTEGER NOT NULL DEFAULT 0,
		last_played INTEGER NOT NULL DEFAULT 0,
		PRIMARY KEY (account_id, ladder)
	);
	CREATE TABLE sets (
		id          INTEGER PRIMARY KEY,
		ladder      TEXT NOT NULL,
		match_id    INTEGER NOT NULL,
		boot_id     TEXT NOT NULL,
		status      TEXT NOT NULL,
		reason      TEXT NOT NULL DEFAULT '',
		winner_id   INTEGER,
		result_json TEXT NOT NULL DEFAULT '',
		created_at  INTEGER NOT NULL,
		UNIQUE (boot_id, match_id)
	);
	CREATE TABLE set_players (
		set_id           INTEGER NOT NULL REFERENCES sets(id) ON DELETE CASCADE,
		account_id       INTEGER NOT NULL REFERENCES accounts(id),
		slot             INTEGER NOT NULL,
		games_won        INTEGER NOT NULL,
		rating_before    REAL NOT NULL,
		deviation_before REAL NOT NULL,
		rating_after     REAL NOT NULL,
		deviation_after  REAL NOT NULL,
		PRIMARY KEY (set_id, account_id)
	);
	CREATE INDEX set_players_account ON set_players(account_id);`,
	// 2: what a set was played with (needed to replay it), and stored replays
	`ALTER TABLE sets ADD COLUMN ruleset_id TEXT NOT NULL DEFAULT '';
	ALTER TABLE sets ADD COLUMN module_version TEXT NOT NULL DEFAULT '';
	ALTER TABLE sets ADD COLUMN rom_md5 TEXT NOT NULL DEFAULT '';
	CREATE TABLE replays (
		set_id      INTEGER PRIMARY KEY REFERENCES sets(id) ON DELETE CASCADE,
		size        INTEGER NOT NULL,
		sha256      TEXT NOT NULL,
		bookmarks   TEXT NOT NULL DEFAULT '[]',
		uploader_id INTEGER NOT NULL REFERENCES accounts(id),
		created_at  INTEGER NOT NULL
	);`,
	// 3: one-time password reset codes, from a moderator (or, since 10, by email)
	`CREATE TABLE password_resets (
		account_id  INTEGER PRIMARY KEY REFERENCES accounts(id) ON DELETE CASCADE,
		code_hash   TEXT NOT NULL,
		expires_at  INTEGER NOT NULL,
		created_by  TEXT NOT NULL
	);`,
	// 4: what moderators did (website and lobby chat)
	`CREATE TABLE admin_log (
		id          INTEGER PRIMARY KEY,
		created_at  INTEGER NOT NULL,
		admin       TEXT NOT NULL,
		action      TEXT NOT NULL,
		detail      TEXT NOT NULL DEFAULT ''
	);`,
	// 5: each roster (ruleset) of a ladder is its own rating; rebuilt from
	// the set history by rebuildRatingsByRoster
	`CREATE TABLE ratings_by_roster (
		account_id  INTEGER NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
		ladder      TEXT NOT NULL,
		ruleset     TEXT NOT NULL,
		rating      REAL NOT NULL,
		deviation   REAL NOT NULL,
		volatility  REAL NOT NULL,
		sets_played INTEGER NOT NULL DEFAULT 0,
		last_played INTEGER NOT NULL DEFAULT 0,
		PRIMARY KEY (account_id, ladder, ruleset)
	);
	DROP TABLE ratings;
	ALTER TABLE ratings_by_roster RENAME TO ratings;
	CREATE INDEX ratings_board ON ratings(ladder, ruleset);
	CREATE INDEX sets_board ON sets(ladder, ruleset_id, status);`,
	// 6: the characters each player used in a set (filled in from the
	// stored reports by backfillSetCharacters)
	`CREATE TABLE set_characters (
		set_id      INTEGER NOT NULL REFERENCES sets(id) ON DELETE CASCADE,
		account_id  INTEGER NOT NULL REFERENCES accounts(id),
		character   INTEGER NOT NULL,
		PRIMARY KEY (set_id, account_id, character)
	);
	CREATE INDEX set_characters_lookup ON set_characters(character, account_id);
	CREATE INDEX sets_recent ON sets(created_at DESC, id DESC);`,
	// 7: team sets (2v2): each player's team (1 or 2, 0 in 1v1) and the
	// winning team
	`ALTER TABLE set_players ADD COLUMN team INTEGER NOT NULL DEFAULT 0;
	ALTER TABLE sets ADD COLUMN winner_team INTEGER NOT NULL DEFAULT 0;`,
	// 8: the lobby region each player played a set from (regional rankings)
	`ALTER TABLE set_players ADD COLUMN region TEXT NOT NULL DEFAULT '';`,
	// 9: Discord accounts linked for easier sign-in (passwords stay required)
	`CREATE TABLE discord_links (
		account_id       INTEGER NOT NULL UNIQUE REFERENCES accounts(id) ON DELETE CASCADE,
		discord_id       TEXT NOT NULL UNIQUE,
		discord_username TEXT NOT NULL,
		discord_name     TEXT NOT NULL DEFAULT '',
		linked_at        INTEGER NOT NULL,
		last_login_at    INTEGER NOT NULL
	);`,
	// 10: an optional, confirmed email address for password reset codes, and
	// the address waiting for its confirmation code
	`ALTER TABLE accounts ADD COLUMN email TEXT NOT NULL DEFAULT '';
	CREATE TABLE email_confirmations (
		account_id INTEGER PRIMARY KEY REFERENCES accounts(id) ON DELETE CASCADE,
		email      TEXT NOT NULL,
		code_hash  TEXT NOT NULL,
		expires_at INTEGER NOT NULL
	);`,
}

// migrationSteps run in Go after the SQL of the same migration (1-based).
var migrationSteps = map[int]func(tx *sql.Tx) error{
	5: rebuildRatingsByRoster,
	6: backfillSetCharacters,
}

func (s *Store) migrate() error {
	var version int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return err
	}
	for ; version < len(migrations); version++ {
		tx, err := s.db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(migrations[version]); err != nil {
			tx.Rollback()
			return err
		}
		if step := migrationSteps[version+1]; step != nil {
			if err := step(tx); err != nil {
				tx.Rollback()
				return fmt.Errorf("migration %d: %w", version+1, err)
			}
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

func isUniqueViolation(err error) bool {
	return strings.Contains(err.Error(), "UNIQUE")
}

func validPassword(password string) bool {
	n := utf8.RuneCountInString(password)
	return n >= MinPasswordLen && len(password) <= MaxPasswordLen
}

// Register creates an account. The caller validates the username format.
func (s *Store) Register(username, password string) (Account, error) {
	if !validPassword(password) {
		return Account{}, ErrWeakPassword
	}
	hash, err := hashPassword(password)
	if err != nil {
		return Account{}, err
	}
	now := s.now()
	res, err := s.db.Exec(`INSERT INTO accounts (username, pw_hash, created_at) VALUES (?, ?, ?)`,
		username, hash, now.Unix())
	if err != nil {
		if isUniqueViolation(err) {
			return Account{}, ErrUsernameTaken
		}
		return Account{}, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Account{}, err
	}
	return Account{ID: id, Username: username, CreatedAt: time.Unix(now.Unix(), 0)}, nil
}

// Login checks a username (case-insensitive) and password.
func (s *Store) Login(username, password string) (Account, error) {
	var (
		account  Account
		hash     string
		created  int64
		disabled bool
	)
	err := s.db.QueryRow(`SELECT id, username, pw_hash, created_at, disabled FROM accounts WHERE username = ?`,
		username).Scan(&account.ID, &account.Username, &hash, &created, &disabled)
	if errors.Is(err, sql.ErrNoRows) {
		verifyPassword(dummyHash, password)
		return Account{}, ErrBadCredentials
	}
	if err != nil {
		return Account{}, err
	}
	ok, err := verifyPassword(hash, password)
	if err != nil {
		return Account{}, err
	}
	if !ok {
		return Account{}, ErrBadCredentials
	}
	if disabled {
		return Account{}, ErrAccountDisabled
	}
	account.CreatedAt = time.Unix(created, 0)
	return account, nil
}

// IsRegistered reports whether username (case-insensitive) belongs to an
// account, so guests can't connect under a registered name.
func (s *Store) IsRegistered(username string) (bool, error) {
	var one int
	err := s.db.QueryRow(`SELECT 1 FROM accounts WHERE username = ?`, username).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// CreateSession issues a remember-me token for account. Only its hash is
// stored, so a leaked database doesn't leak usable tokens.
func (s *Store) CreateSession(accountID int64) (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	now := s.now()
	if _, err := s.db.Exec(`DELETE FROM sessions WHERE expires_at < ?`, now.Unix()); err != nil {
		return "", err
	}
	_, err := s.db.Exec(`INSERT INTO sessions (token_hash, account_id, expires_at) VALUES (?, ?, ?)`,
		hashToken(token), accountID, now.Add(SessionTTL).Unix())
	if err != nil {
		return "", err
	}
	return token, nil
}

// SessionAccount resolves a token and slides its expiry forward.
func (s *Store) SessionAccount(token string) (Account, error) {
	if token == "" {
		return Account{}, ErrInvalidSession
	}
	now := s.now()
	var (
		account  Account
		created  int64
		expires  int64
		disabled bool
	)
	err := s.db.QueryRow(`SELECT a.id, a.username, a.created_at, a.disabled, s.expires_at
		FROM sessions s JOIN accounts a ON a.id = s.account_id WHERE s.token_hash = ?`,
		hashToken(token)).Scan(&account.ID, &account.Username, &created, &disabled, &expires)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && expires < now.Unix()) {
		return Account{}, ErrInvalidSession
	}
	if err != nil {
		return Account{}, err
	}
	if disabled {
		return Account{}, ErrAccountDisabled
	}
	if _, err := s.db.Exec(`UPDATE sessions SET expires_at = ? WHERE token_hash = ?`,
		now.Add(SessionTTL).Unix(), hashToken(token)); err != nil {
		return Account{}, err
	}
	account.CreatedAt = time.Unix(created, 0)
	return account, nil
}

// RevokeSession deletes a token (logout).
func (s *Store) RevokeSession(token string) error {
	_, err := s.db.Exec(`DELETE FROM sessions WHERE token_hash = ?`, hashToken(token))
	return err
}

type querier interface {
	QueryRow(query string, args ...any) *sql.Row
}

func (s *Store) loadRating(q querier, accountID int64, ladder, ruleset string) (LadderRating, error) {
	lr := LadderRating{Ladder: ladder, Ruleset: ruleset, Rating: rating.Default()}
	var last int64
	err := q.QueryRow(`SELECT rating, deviation, volatility, sets_played, last_played
		FROM ratings WHERE account_id = ? AND ladder = ? AND ruleset = ?`, accountID, ladder, ruleset).Scan(
		&lr.Rating.Rating, &lr.Rating.Deviation, &lr.Rating.Volatility, &lr.SetsPlayed, &last)
	if errors.Is(err, sql.ErrNoRows) {
		return lr, nil
	}
	if err != nil {
		return LadderRating{}, err
	}
	if last > 0 {
		lr.LastPlayed = time.Unix(last, 0)
		lr.Rating = lr.Rating.AfterInactivity(lr.LastPlayed, s.now())
	}
	return lr, nil
}

// Rating returns an account's current rating on one roster of a ladder
// (default if unplayed).
func (s *Store) Rating(accountID int64, ladder, ruleset string) (LadderRating, error) {
	return s.loadRating(s.db, accountID, ladder, ruleset)
}

// SeedRating sets an account's rating on one roster of a ladder directly,
// e.g. when importing ratings from an earlier system. It doesn't count as a
// set.
func (s *Store) SeedRating(accountID int64, ladder, ruleset string, r rating.Rating) error {
	_, err := s.db.Exec(`INSERT INTO ratings (account_id, ladder, ruleset, rating, deviation, volatility)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (account_id, ladder, ruleset) DO UPDATE SET rating = excluded.rating,
			deviation = excluded.deviation, volatility = excluded.volatility`,
		accountID, ladder, ruleset, r.Rating, r.Deviation, r.Volatility)
	return err
}

type execer interface {
	Exec(query string, args ...any) (sql.Result, error)
}

// saveRatedSet stores a player's rating after a rated set.
func saveRatedSet(tx execer, accountID int64, ladder, ruleset string, r rating.Rating, playedAt time.Time) error {
	_, err := tx.Exec(`INSERT INTO ratings (account_id, ladder, ruleset, rating, deviation, volatility, sets_played, last_played)
		VALUES (?, ?, ?, ?, ?, ?, 1, ?)
		ON CONFLICT (account_id, ladder, ruleset) DO UPDATE SET rating = excluded.rating,
			deviation = excluded.deviation, volatility = excluded.volatility,
			sets_played = sets_played + 1, last_played = excluded.last_played`,
		accountID, ladder, ruleset, r.Rating, r.Deviation, r.Volatility, playedAt.Unix())
	return err
}

// SetStatus is how a set ended up in the history.
type SetStatus string

const (
	SetRated    SetStatus = "rated"    // both reports agreed, ratings updated
	SetVoided   SetStatus = "voided"   // no rating change (left mid-set, timeout, ...)
	SetDisputed SetStatus = "disputed" // reports disagreed or desynced; needs review
)

// SetPlayer is one participant of a recorded set.
type SetPlayer struct {
	AccountID int64
	Slot      int
	GamesWon  int    // in a team set, the team's games
	Team      int    // 1 or 2 in a team set, 0 in 1v1
	Region    string // lobby region the player connected from
}

// SetRecord describes a finished or abandoned ranked set.
type SetRecord struct {
	Ladder  string
	MatchID uint64
	// BootID makes MatchID unique across lobby restarts.
	BootID     string
	Status     SetStatus
	Reason     string
	WinnerID   int64 // 0 unless rated (1v1)
	WinnerTeam int   // the winning team of a rated team set (1 or 2), else 0
	// GameWinners: a team set's game winners (team 1 or 2) in order, each
	// game is rated; empty counts the set as one game.
	GameWinners []int
	Players     []SetPlayer
	ResultJSON  string
	// What the set was played with; replays need the same module version.
	RulesetID     string
	ModuleVersion string
	RomMD5        string
}

// PlayerOutcome is a participant's rating before and after a recorded set.
type PlayerOutcome struct {
	AccountID int64
	Before    rating.Rating
	After     rating.Rating
}

// RecordSet stores a set and, when it is rated, applies the Glicko-2 update
// to both players atomically. Recording the same (BootID, MatchID) twice is
// an error, so a set can never be counted twice.
func (s *Store) RecordSet(rec SetRecord) ([]PlayerOutcome, error) {
	_, outcomes, err := s.RecordSetWithID(rec)
	return outcomes, err
}

// RecordSetWithID is RecordSet that also returns the new set's id.
func (s *Store) RecordSetWithID(rec SetRecord) (int64, []PlayerOutcome, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, nil, err
	}
	defer tx.Rollback()

	now := s.now()
	participants := make([]setParticipant, len(rec.Players))
	for i, p := range rec.Players {
		participants[i] = setParticipant{AccountID: p.AccountID, Team: p.Team}
	}
	outcomes, err := s.rateSet(tx, rec.Ladder, rec.RulesetID, participants, rec.WinnerID, rec.WinnerTeam,
		rec.GameWinners, rec.Status == SetRated, now)
	if err != nil {
		return 0, nil, err
	}

	var winner any
	if rec.WinnerID != 0 && rec.WinnerTeam == 0 {
		winner = rec.WinnerID
	}
	res, err := tx.Exec(`INSERT INTO sets (ladder, match_id, boot_id, status, reason, winner_id, result_json,
			created_at, ruleset_id, module_version, rom_md5, winner_team)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		rec.Ladder, rec.MatchID, rec.BootID, string(rec.Status), rec.Reason, winner, rec.ResultJSON,
		now.Unix(), rec.RulesetID, rec.ModuleVersion, rec.RomMD5, rec.WinnerTeam)
	if err != nil {
		return 0, nil, err
	}
	setID, err := res.LastInsertId()
	if err != nil {
		return 0, nil, err
	}
	slotAccounts := map[int]int64{}
	for i, p := range rec.Players {
		slotAccounts[p.Slot] = p.AccountID
		_, err := tx.Exec(`INSERT INTO set_players (set_id, account_id, slot, games_won,
			rating_before, deviation_before, rating_after, deviation_after, team, region)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			setID, p.AccountID, p.Slot, p.GamesWon,
			outcomes[i].Before.Rating, outcomes[i].Before.Deviation,
			outcomes[i].After.Rating, outcomes[i].After.Deviation, p.Team, p.Region)
		if err != nil {
			return 0, nil, err
		}
	}

	if err := insertSetCharacters(tx, setID, slotAccounts, rec.ResultJSON); err != nil {
		return 0, nil, err
	}

	if err := tx.Commit(); err != nil {
		return 0, nil, err
	}
	s.homesMu.Lock()
	s.homes = nil
	s.homesMu.Unlock()
	return setID, outcomes, nil
}
