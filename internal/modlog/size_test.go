package modlog

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fileSize(t *testing.T, path string) int64 {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Size()
}

// Over the cap, the oldest chat and connections go first, the audit trail
// never; a new file gives the space back.
func TestSizeCapPrunesOldestChatAndConnections(t *testing.T) {
	l, clock, path := openTest(t)
	if _, _, mode, _ := l.SizeStats(); mode != 2 {
		t.Fatalf("a new file's auto_vacuum = %d, want 2 (incremental)", mode)
	}
	l.Audit(AuditEntry{Admin: "Mod", Action: "ban", Detail: "kept whatever happens"})
	msg := strings.Repeat("spam spam ", 50)
	for i := 0; i < 6000; i++ {
		if i%100 == 0 {
			clock.advance(time.Second)
			c := l.ConnectionOpened(Conn{IP: "2001:db8::1", Username: "Spammer"})
			l.ConnectionClosed(c)
		}
		l.Chat(ChatMessage{Channel: ChannelLobby, Name: "Spammer", IP: "2001:db8::1", Text: msg})
	}
	l.Flush()
	before, _, _, _ := l.SizeStats()
	newest, _, _ := l.ChatLog(bg, ChatQuery{Limit: 1})
	if before < 4<<20 {
		t.Fatalf("data = %d bytes, the test needs more than the cap", before)
	}
	l.SetRetention(func() Retention { return Retention{MaxBytes: 2 << 20} })
	l.enforceSize()
	after, free, _, _ := l.SizeStats()
	if after > 2<<20 {
		t.Fatalf("data after the cap = %d bytes", after)
	}
	if free > 64<<10 {
		t.Fatalf("%d bytes of free pages kept", free)
	}
	if size := fileSize(t, path); size > 3<<20 {
		t.Fatalf("file is %d bytes after pruning to a 2 MB cap", size)
	}
	// the newest lines and the audit trail are still there
	if rows, _, _ := l.ChatLog(bg, ChatQuery{Limit: 1}); len(rows) != 1 || rows[0].ID != newest[0].ID {
		t.Fatalf("newest line pruned: %+v", rows)
	}
	if audit, _, _ := l.AuditLog(bg, 0, 0); len(audit) != 1 {
		t.Fatalf("audit = %+v", audit)
	}
	// 0 is no cap
	l.SetRetention(func() Retention { return Retention{} })
	rowsBefore, _, _ := l.ChatLog(bg, ChatQuery{Limit: MaxLimit})
	l.enforceSize()
	if rowsAfter, _, _ := l.ChatLog(bg, ChatQuery{Limit: MaxLimit}); len(rowsAfter) != len(rowsBefore) {
		t.Fatal("pruned with no cap")
	}
}

// A file made before this version (or by the ranked branch, which has only
// the first migration) opens, gets the later tables, and keeps its rows.
func TestOlderFileOpens(t *testing.T) {
	path := filepath.Join(t.TempDir(), "modlog.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(migrations[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`PRAGMA user_version = 1`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO chat (time, channel, name, name_key, text, text_fold, account_id)
		VALUES (1700000000, 'lobby', 'Old', 'old', 'from before', 'from before', 7)`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	l, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	rows, _, err := l.ChatLog(bg, ChatQuery{})
	if err != nil || len(rows) != 1 || rows[0].Text != "from before" || rows[0].Suppressed != 0 {
		t.Fatalf("old row = %+v %v", rows, err)
	}
	if _, _, mode, _ := l.SizeStats(); mode != 0 {
		t.Fatalf("an existing file's auto_vacuum changed to %d", mode)
	}
	l.Chat(ChatMessage{Channel: ChannelLobby, Name: "New", Text: "sampled", Blocked: BlockedMuted, Suppressed: 4})
	l.Audit(AuditEntry{Admin: "Mod", Action: "kick"})
	l.Flush()
	if rows, _, _ := l.ChatLog(bg, ChatQuery{Limit: 1}); rows[0].Suppressed != 4 {
		t.Fatalf("new row = %+v", rows[0])
	}
	var version int
	l.db.QueryRow(`PRAGMA user_version`).Scan(&version)
	if version != len(migrations) {
		t.Fatalf("user_version = %d, want %d", version, len(migrations))
	}
	// the size cap works on it too (freed pages are reused, not returned)
	l.SetRetention(func() Retention { return Retention{MaxBytes: 1} })
	l.enforceSize()
	if rows, _, _ := l.ChatLog(bg, ChatQuery{}); len(rows) != 0 {
		t.Fatalf("chat after pruning to the cap = %+v", rows)
	}
	if audit, _, _ := l.AuditLog(bg, 0, 0); len(audit) != 1 {
		t.Fatal("audit pruned")
	}
}
