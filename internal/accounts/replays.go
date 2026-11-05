package accounts

import (
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"time"
)

// ReplayFile returns where a set's replay is stored inside dir.
func ReplayFile(dir string, setID int64) string {
	return filepath.Join(dir, fmt.Sprintf("%d.krec", setID))
}

// ErrReplayExists is returned when a set already has a stored replay.
var ErrReplayExists = errors.New("replay already stored")

// ReplayInfo describes a stored replay file (the bytes live on disk).
type ReplayInfo struct {
	SetID      int64
	Size       int64
	SHA256     string
	Bookmarks  string // JSON array, see the lobby protocol's ReplayBookmark
	UploaderID int64
	CreatedAt  time.Time
}

// AddReplay records that a set's replay file was stored.
func (s *Store) AddReplay(info ReplayInfo) error {
	if info.Bookmarks == "" {
		info.Bookmarks = "[]"
	}
	_, err := s.db.Exec(`INSERT INTO replays (set_id, size, sha256, bookmarks, uploader_id, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		info.SetID, info.Size, info.SHA256, info.Bookmarks, info.UploaderID, s.now().Unix())
	if err != nil && isUniqueViolation(err) {
		return ErrReplayExists
	}
	return err
}

// Replay returns a set's replay info; ErrNotFound when it has none.
func (s *Store) Replay(setID int64) (ReplayInfo, error) {
	info := ReplayInfo{SetID: setID}
	var created int64
	err := s.db.QueryRow(`SELECT size, sha256, bookmarks, uploader_id, created_at FROM replays WHERE set_id = ?`,
		setID).Scan(&info.Size, &info.SHA256, &info.Bookmarks, &info.UploaderID, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return ReplayInfo{}, ErrNotFound
	}
	if err != nil {
		return ReplayInfo{}, err
	}
	info.CreatedAt = time.Unix(created, 0)
	return info, nil
}

// IsParticipant reports whether accountID played in setID.
func (s *Store) IsParticipant(setID, accountID int64) (bool, error) {
	var one int
	err := s.db.QueryRow(`SELECT 1 FROM set_players WHERE set_id = ? AND account_id = ?`, setID, accountID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// SetByID returns one set with its players; ErrNotFound when missing.
func (s *Store) SetByID(id int64) (SetSummary, error) {
	sets, err := s.querySets(`st.id = ?`, []any{id}, 1, 0)
	if err != nil {
		return SetSummary{}, err
	}
	if len(sets) == 0 {
		return SetSummary{}, ErrNotFound
	}
	return sets[0], nil
}
