package modlog

import (
	"context"
	"database/sql"
	"errors"
	"sort"
	"strings"
)

// Reading the log, for the moderator pages. Lists are newest first unless
// stated; times are unix seconds. Every query takes a context, so a request
// that is given up (or runs too long) stops its query.

const (
	DefaultLimit = 100
	MaxLimit     = 500
	// how many rows a trace aggregates at most (the newest)
	traceScan = 5000
	// how many ips a name trace lists, and names an ip lists
	traceIPs   = 100
	traceNames = 100
	// how many timeline lines a game's page shows at most
	gameEvents = 1000
)

func clampLimit(n int) int {
	if n <= 0 {
		return DefaultLimit
	}
	if n > MaxLimit {
		return MaxLimit
	}
	return n
}

// where builds a WHERE clause from optional conditions. Conditions are fixed
// strings with ? placeholders; values only ever travel as arguments.
type where struct {
	conds []string
	args  []any
}

func (w *where) add(cond string, args ...any) {
	w.conds = append(w.conds, cond)
	w.args = append(w.args, args...)
}

func (w *where) sql() string {
	if len(w.conds) == 0 {
		return ""
	}
	return " WHERE " + strings.Join(w.conds, " AND ")
}

//
// Chat
//

// ChatQuery filters the chat log. Zero values don't filter.
type ChatQuery struct {
	Channel string
	Text    string // substring, case-insensitive
	Name    string // sender, matched by key()
	IP      string
	RoomID  uint64
	MatchID uint64
	Blocked string // "any" = only blocked messages, or one blocked value
	Before  int64  // older than this id, newest first
	After   int64  // newer than this id, oldest first (a live tail)
	Limit   int
}

// ChatRow is one logged message.
type ChatRow struct {
	ID       int64
	Time     int64
	Channel  string
	RoomID   uint64
	RoomName string
	MatchID  uint64
	From     string
	IP       string
	Text     string
	Admin    bool
	Blocked  string
	// Suppressed: blocked lines from the same address that this sample
	// stands for and weren't logged.
	Suppressed int
}

// ChatLog returns one page of messages and whether more exist past it: older
// ones normally, newer ones for an After query.
func (l *Logger) ChatLog(ctx context.Context, q ChatQuery) ([]ChatRow, bool, error) {
	limit := clampLimit(q.Limit)
	var w where
	if q.Channel != "" {
		w.add("channel = ?", q.Channel)
	}
	if q.Text != "" {
		w.add("instr(text_fold, ?) > 0", strings.ToLower(q.Text))
	}
	if q.Name != "" {
		w.add("name_key = ?", key(q.Name))
	}
	if q.IP != "" {
		w.add("ip = ?", q.IP)
	}
	if q.RoomID != 0 {
		w.add("room_id = ?", int64(q.RoomID))
	}
	if q.MatchID != 0 {
		w.add("match_id = ?", int64(q.MatchID))
	}
	switch q.Blocked {
	case "":
	case "any":
		w.add("blocked != ''")
	default:
		w.add("blocked = ?", q.Blocked)
	}
	order := "DESC"
	if q.After > 0 {
		w.add("id > ?", q.After)
		order = "ASC"
	} else if q.Before > 0 {
		w.add("id < ?", q.Before)
	}
	rows, err := l.db.QueryContext(ctx, `SELECT id, time, channel, room_id, room_name, match_id, name, ip,
		text, admin, blocked, suppressed FROM chat`+w.sql()+` ORDER BY id `+order+` LIMIT ?`, append(w.args, limit+1)...)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	out := []ChatRow{}
	for rows.Next() {
		var r ChatRow
		var room, match int64
		if err := rows.Scan(&r.ID, &r.Time, &r.Channel, &room, &r.RoomName, &match, &r.From, &r.IP,
			&r.Text, &r.Admin, &r.Blocked, &r.Suppressed); err != nil {
			return nil, false, err
		}
		r.RoomID, r.MatchID = uint64(room), uint64(match)
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

//
// Connections
//

// ConnQuery filters connection rows.
type ConnQuery struct {
	Name   string
	IP     string
	Before int64
	Limit  int
}

// ConnRow is one lobby connection.
type ConnRow struct {
	ID           int64
	IP           string
	Username     string
	Version      string
	Country      string
	Connected    int64
	Disconnected int64 // 0 = still online
}

// Connections returns one page of connections, newest first, and whether
// older ones exist.
func (l *Logger) Connections(ctx context.Context, q ConnQuery) ([]ConnRow, bool, error) {
	limit := clampLimit(q.Limit)
	var w where
	if q.Name != "" {
		w.add("id IN (SELECT conn_id FROM connection_names WHERE name_key = ?)", key(q.Name))
	}
	if q.IP != "" {
		w.add("ip = ?", q.IP)
	}
	if q.Before > 0 {
		w.add("id < ?", q.Before)
	}
	rows, err := l.db.QueryContext(ctx, `SELECT id, ip, username, version, country, connected, disconnected
		FROM connections`+w.sql()+` ORDER BY id DESC LIMIT ?`, append(w.args, limit+1)...)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	out := []ConnRow{}
	for rows.Next() {
		var r ConnRow
		if err := rows.Scan(&r.ID, &r.IP, &r.Username, &r.Version, &r.Country, &r.Connected, &r.Disconnected); err != nil {
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

func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

//
// Tracing
//

// SeenName is someone seen on an IP.
type SeenName struct {
	Name        string
	First       int64
	Last        int64
	Connections int
	OtherIPs    int // ips other than the traced one (TraceIP only)
}

// SeenIP is an IP a name used, and everyone else seen on it.
type SeenIP struct {
	IP          string
	First       int64
	Last        int64
	Connections int
	Country     string
	Names       []SeenName
}

// NameTrace is what a name was seen with.
type NameTrace struct {
	Name string
	IPs  []SeenIP
}

// IPTrace is everyone an IP was used by.
type IPTrace struct {
	IP      string
	Country string
	Names   []SeenName
}

// seenRow is one name on one connection.
type seenRow struct {
	connID       int64
	ip           string
	name, key    string
	country      string
	connected    int64
	disconnected int64
}

func (r seenRow) last(now int64) int64 {
	if r.disconnected == 0 {
		return now
	}
	return r.disconnected
}

const seenColumns = `c.id, c.ip, n.name, n.name_key, c.country, c.connected, c.disconnected
	FROM connection_names n JOIN connections c ON c.id = n.conn_id`

func (l *Logger) seen(ctx context.Context, cond string, args ...any) ([]seenRow, error) {
	rows, err := l.db.QueryContext(ctx, `SELECT `+seenColumns+` WHERE `+cond+` ORDER BY c.connected DESC, c.id DESC LIMIT ?`,
		append(args, traceScan)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []seenRow
	for rows.Next() {
		var r seenRow
		if err := rows.Scan(&r.connID, &r.ip, &r.name, &r.key, &r.country, &r.connected, &r.disconnected); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// groupNames groups rows (newest first) by name: the latest spelling, first
// and last seen.
func groupNames(rows []seenRow, now int64) []SeenName {
	byKey := map[string]*SeenName{}
	conns := map[string]map[int64]bool{}
	var order []string
	for _, r := range rows {
		n := byKey[r.key]
		if n == nil {
			n = &SeenName{Name: r.name, First: r.connected, Last: r.last(now)}
			byKey[r.key] = n
			conns[r.key] = map[int64]bool{}
			order = append(order, r.key)
		}
		if !conns[r.key][r.connID] {
			conns[r.key][r.connID] = true
			n.Connections++
		}
		if r.connected < n.First {
			n.First = r.connected
		}
		if last := r.last(now); last > n.Last {
			n.Last = last
		}
	}
	out := make([]SeenName, 0, len(order))
	for _, k := range order {
		out = append(out, *byKey[k])
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Last > out[j].Last })
	return out
}

// groupIPs groups rows (newest first) by IP.
func groupIPs(rows []seenRow, now int64) []SeenIP {
	byIP := map[string]*SeenIP{}
	conns := map[string]map[int64]bool{}
	var order []string
	for _, r := range rows {
		s := byIP[r.ip]
		if s == nil {
			s = &SeenIP{IP: r.ip, First: r.connected, Last: r.last(now), Country: r.country}
			byIP[r.ip] = s
			conns[r.ip] = map[int64]bool{}
			order = append(order, r.ip)
		}
		if !conns[r.ip][r.connID] {
			conns[r.ip][r.connID] = true
			s.Connections++
		}
		if r.connected < s.First {
			s.First = r.connected
		}
		if last := r.last(now); last > s.Last {
			s.Last = last
		}
		if s.Country == "" {
			s.Country = r.country
		}
	}
	out := make([]SeenIP, 0, len(order))
	for _, ip := range order {
		out = append(out, *byIP[ip])
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Last > out[j].Last })
	if len(out) > traceIPs {
		out = out[:traceIPs]
	}
	return out
}

// othersOnIPs fills in everyone else seen on each IP (excluding names with
// the given keys).
func (l *Logger) othersOnIPs(ctx context.Context, ips []SeenIP, exclude map[string]bool, now int64) error {
	for i := range ips {
		rows, err := l.seen(ctx, "c.ip = ?", ips[i].IP)
		if err != nil {
			return err
		}
		kept := rows[:0]
		for _, r := range rows {
			if !exclude[r.key] {
				kept = append(kept, r)
			}
		}
		names := groupNames(kept, now)
		if len(names) > traceNames {
			names = names[:traceNames]
		}
		ips[i].Names = names
	}
	return nil
}

// TraceName lists the IPs a name was used on and who else used each. ok is
// false when the name was never seen.
func (l *Logger) TraceName(ctx context.Context, name string) (NameTrace, bool, error) {
	k := key(name)
	rows, err := l.seen(ctx, "n.name_key = ?", k)
	if err != nil || len(rows) == 0 {
		return NameTrace{}, false, err
	}
	now := l.now().Unix()
	t := NameTrace{Name: rows[0].name}
	t.IPs = groupIPs(rows, now)
	return t, true, l.othersOnIPs(ctx, t.IPs, map[string]bool{k: true}, now)
}

// TraceIP lists everyone seen on an IP, with how many other IPs each used.
// ok is false when the IP was never seen.
func (l *Logger) TraceIP(ctx context.Context, ip string) (IPTrace, bool, error) {
	rows, err := l.seen(ctx, "c.ip = ?", ip)
	if err != nil || len(rows) == 0 {
		return IPTrace{IP: ip}, false, err
	}
	now := l.now().Unix()
	t := IPTrace{IP: ip}
	for _, r := range rows {
		if r.country != "" {
			t.Country = r.country
			break
		}
	}
	t.Names = groupNames(rows, now)
	if len(t.Names) > traceNames {
		t.Names = t.Names[:traceNames]
	}
	for i := range t.Names {
		if err := l.db.QueryRowContext(ctx, `SELECT COUNT(DISTINCT c.ip) FROM connection_names n JOIN connections c
			ON c.id = n.conn_id WHERE n.name_key = ? AND c.ip != ?`, key(t.Names[i].Name), ip).
			Scan(&t.Names[i].OtherIPs); err != nil {
			return t, true, err
		}
	}
	return t, true, nil
}

//
// Games
//

// GameQuery filters games.
type GameQuery struct {
	Name   string // a player, matched by key()
	IP     string // a player's IP
	Match  uint64 // a match id; 0 = any
	Before int64
	Limit  int
}

// GameRow is one match.
type GameRow struct {
	ID         int64
	MatchID    uint64
	RoomID     uint64
	RoomName   string
	Game       string
	RomMD5     string
	Started    int64
	Ended      int64 // 0 = still running
	EndReason  string
	Players    []PlayerRow
	Spectators int
}

// PlayerRow is one player of a game.
type PlayerRow struct {
	Name       string
	IP         string
	Slot       int
	Joined     int64
	JoinedLate int64 // 0 = played from the start
	Left       int64 // 0 = stayed to the end (or still playing)
}

const gameColumns = `id, match_id, room_id, room_name, game, rom_md5, started, ended, end_reason, spectators FROM games`

func scanGame(scan func(...any) error) (GameRow, error) {
	var g GameRow
	var match, room int64
	err := scan(&g.ID, &match, &room, &g.RoomName, &g.Game, &g.RomMD5, &g.Started, &g.Ended, &g.EndReason, &g.Spectators)
	g.MatchID, g.RoomID = uint64(match), uint64(room)
	g.Players = []PlayerRow{}
	return g, err
}

// Games returns one page of games, newest first, and whether older ones exist.
func (l *Logger) Games(ctx context.Context, q GameQuery) ([]GameRow, bool, error) {
	limit := clampLimit(q.Limit)
	var w where
	if q.Name != "" {
		w.add("id IN (SELECT game_id FROM game_players WHERE name_key = ?)", key(q.Name))
	}
	if q.IP != "" {
		w.add("id IN (SELECT game_id FROM game_players WHERE ip = ?)", q.IP)
	}
	if q.Match != 0 {
		w.add("match_id = ?", int64(q.Match))
	}
	if q.Before > 0 {
		w.add("id < ?", q.Before)
	}
	rows, err := l.db.QueryContext(ctx, `SELECT `+gameColumns+w.sql()+` ORDER BY id DESC LIMIT ?`, append(w.args, limit+1)...)
	if err != nil {
		return nil, false, err
	}
	out := []GameRow{}
	for rows.Next() {
		g, err := scanGame(rows.Scan)
		if err != nil {
			rows.Close()
			return nil, false, err
		}
		out = append(out, g)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	more := len(out) > limit
	if more {
		out = out[:limit]
	}
	return out, more, l.fillPlayers(ctx, out)
}

func (l *Logger) fillPlayers(ctx context.Context, games []GameRow) error {
	if len(games) == 0 {
		return nil
	}
	ids := make([]any, len(games))
	byID := make(map[int64]*GameRow, len(games))
	for i := range games {
		ids[i] = games[i].ID
		byID[games[i].ID] = &games[i]
	}
	rows, err := l.db.QueryContext(ctx, `SELECT game_id, name, ip, slot, joined, joined_late, left_at
		FROM game_players WHERE game_id IN (`+placeholders(len(ids))+`) ORDER BY slot, joined, user_id`, ids...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var p PlayerRow
		if err := rows.Scan(&id, &p.Name, &p.IP, &p.Slot, &p.Joined, &p.JoinedLate, &p.Left); err != nil {
			return err
		}
		if g := byID[id]; g != nil {
			g.Players = append(g.Players, p)
		}
	}
	return rows.Err()
}

// EventRow is one timeline line.
type EventRow struct {
	Time int64
	Type string
	Text string
}

// GameByID returns a game, its timeline (oldest first) and how many room and
// spectator chat messages were logged during it. ErrNotFound when there's no
// such game.
func (l *Logger) GameByID(ctx context.Context, id int64) (GameRow, []EventRow, int, error) {
	g, err := scanGame(l.db.QueryRowContext(ctx, `SELECT `+gameColumns+` WHERE id = ?`, id).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return GameRow{}, nil, 0, ErrNotFound
	}
	if err != nil {
		return GameRow{}, nil, 0, err
	}
	games := []GameRow{g}
	if err := l.fillPlayers(ctx, games); err != nil {
		return GameRow{}, nil, 0, err
	}
	g = games[0]
	end := g.Ended
	if end == 0 {
		end = l.now().Unix()
	}
	// the match's own lines, and the room's while it ran
	rows, err := l.db.QueryContext(ctx, `SELECT time, type, text FROM events
		WHERE (match_id = ? AND time BETWEEN ? AND ?) OR (match_id = 0 AND room_id = ? AND time BETWEEN ? AND ?)
		ORDER BY time, id LIMIT ?`,
		int64(g.MatchID), g.Started-60, end+3600, int64(g.RoomID), g.Started, end, gameEvents)
	if err != nil {
		return GameRow{}, nil, 0, err
	}
	events := []EventRow{}
	for rows.Next() {
		var e EventRow
		if err := rows.Scan(&e.Time, &e.Type, &e.Text); err != nil {
			rows.Close()
			return GameRow{}, nil, 0, err
		}
		events = append(events, e)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return GameRow{}, nil, 0, err
	}
	var chatCount int
	err = l.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM chat WHERE match_id = ? AND channel IN ('room', 'spectate')
		AND time BETWEEN ? AND ?`, int64(g.MatchID), g.Started-60, end+60).Scan(&chatCount)
	return g, events, chatCount, err
}
