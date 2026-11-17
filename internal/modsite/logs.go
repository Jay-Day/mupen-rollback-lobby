package modsite

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/jay-day/rmgk-lobby/internal/modlog"
)

// The moderators' log (internal/modlog): chat, IP tracing, connections,
// games and the audit trail. Each answers 503 when the server keeps no log.
// Every list is a page of at most modlog.MaxLimit rows with a "more" flag;
// the next page is asked for with before= (the last row's id).
//
//	GET /api/admin/chat?channel=&q=&name=&ip=&room=&match=&blocked=&before=&after=&limit=
//	GET /api/admin/trace?name= | ?ip=
//	GET /api/admin/connections?name=&ip=&before=&limit=
//	GET /api/admin/games?name=&ip=&match=&before=&limit=
//	GET /api/admin/games/{id}
//	GET /api/admin/audit?before=&limit=

// longest search text and name a query may hold
const (
	maxQueryText = 200
	maxQueryName = 64
	maxQueryIP   = 64
)

func (s *Server) registerLogs() {
	s.Handle("GET /api/admin/chat", s.handleChat)
	s.Handle("GET /api/admin/trace", s.handleTrace)
	s.Handle("GET /api/admin/connections", s.handleConnections)
	s.Handle("GET /api/admin/games", s.handleGames)
	s.Handle("GET /api/admin/games/{id}", s.handleGame)
	s.Handle("GET /api/admin/audit", s.handleAudit)
}

func (s *Server) requireLog(w http.ResponseWriter) bool {
	if s.log == nil {
		WriteError(w, http.StatusServiceUnavailable, "logging is off on this server")
		return false
	}
	return true
}

// query reads the request's parameters, checking each one. The first
// problem is answered (400) and later reads become no-ops; ok() says
// whether to go on.
type query struct {
	w   http.ResponseWriter
	r   *http.Request
	bad bool
}

func (q *query) fail(name string) {
	if !q.bad {
		WriteError(q.w, http.StatusBadRequest, "bad "+name)
		q.bad = true
	}
}

func (q *query) ok() bool { return !q.bad }

// text reads a string parameter of at most max characters.
func (q *query) text(name string, max int) string {
	v := strings.TrimSpace(q.r.URL.Query().Get(name))
	if !utf8.ValidString(v) || utf8.RuneCountInString(v) > max {
		q.fail(name)
		return ""
	}
	return v
}

// id reads an optional non-negative integer (a row, room or match id, or a
// limit); 0 when absent.
func (q *query) id(name string) int64 {
	raw := strings.TrimSpace(q.r.URL.Query().Get(name))
	if raw == "" {
		return 0
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n < 0 || n > 1<<53 {
		q.fail(name)
		return 0
	}
	return n
}

// limit reads limit= (1 to modlog.MaxLimit; 0 or absent: the default).
func (q *query) limit() int {
	n := q.id("limit")
	if n > modlog.MaxLimit {
		q.fail("limit")
		return 0
	}
	return int(n)
}

// ip reads an optional IP parameter as the lobby writes IPs (as given when
// it doesn't parse, so odd old entries still match exactly).
func (q *query) ip() string {
	raw := q.text("ip", maxQueryIP)
	if ip := parseIP(raw); ip != "" {
		return ip
	}
	return raw
}

func (s *Server) logFailed(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		WriteError(w, http.StatusServiceUnavailable, "that search took too long; narrow it down")
		return
	}
	slog.Warn("moderator log query failed", "path", r.URL.Path, "err", err)
	WriteError(w, http.StatusInternalServerError, "couldn't read the log")
}

func queryContext(r *http.Request) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), queryTimeout)
}

type chatDTO struct {
	ID       int64  `json:"id"`
	Time     int64  `json:"time"`
	Channel  string `json:"channel"`
	RoomID   uint64 `json:"roomId"`
	RoomName string `json:"roomName"`
	MatchID  uint64 `json:"matchId"`
	From     string `json:"from"`
	IP       string `json:"ip"`
	Text     string `json:"text"`
	Admin    bool   `json:"admin"`
	Blocked  string `json:"blocked"`
	// Suppressed: a blocked line logged as a sample stands for this many
	// more from the same address that weren't logged.
	Suppressed int `json:"suppressed"`
}

func (s *Server) handleChat(w http.ResponseWriter, r *http.Request, admin string) {
	if !s.requireLog(w) {
		return
	}
	q := &query{w: w, r: r}
	cq := modlog.ChatQuery{
		Channel: q.text("channel", 16),
		Text:    q.text("q", maxQueryText),
		Name:    q.text("name", maxQueryName),
		IP:      q.ip(),
		RoomID:  uint64(q.id("room")),
		MatchID: uint64(q.id("match")),
		Before:  q.id("before"),
		After:   q.id("after"),
		Limit:   q.limit(),
	}
	switch cq.Channel {
	case "", modlog.ChannelLobby, modlog.ChannelRoom, modlog.ChannelSpectate:
	default:
		q.fail("channel")
	}
	switch blocked := q.text("blocked", 16); blocked {
	case "", "0":
	case "1":
		cq.Blocked = "any"
	case modlog.BlockedFilter, modlog.BlockedMuted, modlog.BlockedFlood:
		cq.Blocked = blocked
	default:
		q.fail("blocked")
	}
	if !q.ok() {
		return
	}
	ctx, cancel := queryContext(r)
	defer cancel()
	rows, more, err := s.log.ChatLog(ctx, cq)
	if err != nil {
		s.logFailed(w, r, err)
		return
	}
	out := make([]chatDTO, 0, len(rows))
	for _, m := range rows {
		out = append(out, chatDTO{ID: m.ID, Time: m.Time, Channel: m.Channel, RoomID: m.RoomID, RoomName: m.RoomName,
			MatchID: m.MatchID, From: m.From, IP: m.IP, Text: m.Text, Admin: m.Admin, Blocked: m.Blocked,
			Suppressed: m.Suppressed})
	}
	WriteJSON(w, http.StatusOK, map[string]any{"messages": out, "more": more})
}

// seenNameDTO is someone else seen on an IP a name trace lists.
type seenNameDTO struct {
	Name        string `json:"name"`
	Last        int64  `json:"last"`
	Connections int    `json:"connections"`
}

// ipNameDTO is someone an IP trace lists.
type ipNameDTO struct {
	Name        string `json:"name"`
	First       int64  `json:"first"`
	Last        int64  `json:"last"`
	Connections int    `json:"connections"`
	OtherIPs    int    `json:"otherIps"`
}

type seenIPDTO struct {
	IP          string        `json:"ip"`
	First       int64         `json:"first"`
	Last        int64         `json:"last"`
	Connections int           `json:"connections"`
	Country     string        `json:"country"`
	Ban         *sanctionDTO  `json:"ban"`
	Mute        *sanctionDTO  `json:"mute"`
	Names       []seenNameDTO `json:"names"`
}

func (s *Server) sanctionsOn(ip string) (*sanctionDTO, *sanctionDTO) {
	if s.lobby == nil {
		return nil, nil
	}
	ban, mute := s.lobby.SanctionsOn(ip)
	return toSanction(ban), toSanction(mute)
}

func (s *Server) handleTrace(w http.ResponseWriter, r *http.Request, admin string) {
	if !s.requireLog(w) {
		return
	}
	q := &query{w: w, r: r}
	name, rawIP := q.text("name", maxQueryName), q.text("ip", maxQueryIP)
	if !q.ok() {
		return
	}
	if (name == "") == (rawIP == "") {
		WriteError(w, http.StatusBadRequest, "trace one name or IP")
		return
	}
	ctx, cancel := queryContext(r)
	defer cancel()
	if rawIP != "" {
		s.traceIP(ctx, w, r, rawIP)
		return
	}
	trace, found, err := s.log.TraceName(ctx, name)
	if err != nil {
		s.logFailed(w, r, err)
		return
	}
	if !found {
		WriteError(w, http.StatusNotFound, "nobody by that name in the log")
		return
	}
	ips := make([]seenIPDTO, 0, len(trace.IPs))
	for _, ip := range trace.IPs {
		ban, mute := s.sanctionsOn(ip.IP)
		names := make([]seenNameDTO, 0, len(ip.Names))
		for _, n := range ip.Names {
			names = append(names, seenNameDTO{Name: n.Name, Last: n.Last, Connections: n.Connections})
		}
		ips = append(ips, seenIPDTO{IP: ip.IP, First: ip.First, Last: ip.Last, Connections: ip.Connections,
			Country: ip.Country, Ban: ban, Mute: mute, Names: names})
	}
	online := s.lobby != nil && s.lobby.IsOnline(trace.Name)
	WriteJSON(w, http.StatusOK, map[string]any{"name": trace.Name, "ips": ips, "online": online})
}

func (s *Server) traceIP(ctx context.Context, w http.ResponseWriter, r *http.Request, raw string) {
	ip := parseIP(raw)
	if ip == "" {
		WriteError(w, http.StatusBadRequest, "that isn't an IP address")
		return
	}
	trace, found, err := s.log.TraceIP(ctx, ip)
	if err != nil {
		s.logFailed(w, r, err)
		return
	}
	ban, mute := s.sanctionsOn(ip)
	type liveRefDTO struct {
		ID       uint64 `json:"id"`
		Username string `json:"username"`
	}
	online := []liveRefDTO{}
	if s.lobby != nil {
		for _, ref := range s.lobby.OnlineAt(ip) {
			online = append(online, liveRefDTO{ID: ref.ID, Username: ref.Username})
		}
	}
	if !found && ban == nil && mute == nil && len(online) == 0 {
		WriteError(w, http.StatusNotFound, "that IP isn't in the log")
		return
	}
	names := make([]ipNameDTO, 0, len(trace.Names))
	for _, n := range trace.Names {
		names = append(names, ipNameDTO{Name: n.Name, First: n.First, Last: n.Last, Connections: n.Connections,
			OtherIPs: n.OtherIPs})
	}
	WriteJSON(w, http.StatusOK, map[string]any{"ip": ip, "country": trace.Country, "ban": ban, "mute": mute,
		"names": names, "online": online})
}

type connectionDTO struct {
	ID           int64  `json:"id"`
	IP           string `json:"ip"`
	Username     string `json:"username"`
	Version      string `json:"version"`
	Country      string `json:"country"`
	Connected    int64  `json:"connected"`
	Disconnected int64  `json:"disconnected"`
}

func (s *Server) handleConnections(w http.ResponseWriter, r *http.Request, admin string) {
	if !s.requireLog(w) {
		return
	}
	q := &query{w: w, r: r}
	cq := modlog.ConnQuery{Name: q.text("name", maxQueryName), IP: q.ip(), Before: q.id("before"), Limit: q.limit()}
	if !q.ok() {
		return
	}
	ctx, cancel := queryContext(r)
	defer cancel()
	rows, more, err := s.log.Connections(ctx, cq)
	if err != nil {
		s.logFailed(w, r, err)
		return
	}
	out := make([]connectionDTO, 0, len(rows))
	for _, c := range rows {
		out = append(out, connectionDTO{ID: c.ID, IP: c.IP, Username: c.Username, Version: c.Version,
			Country: c.Country, Connected: c.Connected, Disconnected: c.Disconnected})
	}
	WriteJSON(w, http.StatusOK, map[string]any{"connections": out, "more": more})
}

type gamePlayerDTO struct {
	Name       string `json:"name"`
	IP         string `json:"ip"`
	Slot       int    `json:"slot"`
	JoinedLate int64  `json:"joinedLate"`
	Left       int64  `json:"left"`
}

type gameDTO struct {
	ID         int64           `json:"id"`
	MatchID    uint64          `json:"matchId"`
	RoomID     uint64          `json:"roomId"`
	RoomName   string          `json:"roomName"`
	Game       string          `json:"game"`
	Started    int64           `json:"started"`
	Ended      int64           `json:"ended"`
	EndReason  string          `json:"endReason"`
	Players    []gamePlayerDTO `json:"players"`
	Spectators int             `json:"spectators"`
}

func toGameDTO(g modlog.GameRow) gameDTO {
	players := make([]gamePlayerDTO, 0, len(g.Players))
	for _, p := range g.Players {
		players = append(players, gamePlayerDTO{Name: p.Name, IP: p.IP, Slot: p.Slot, JoinedLate: p.JoinedLate, Left: p.Left})
	}
	return gameDTO{ID: g.ID, MatchID: g.MatchID, RoomID: g.RoomID, RoomName: g.RoomName, Game: g.Game,
		Started: g.Started, Ended: g.Ended, EndReason: g.EndReason, Players: players, Spectators: g.Spectators}
}

func (s *Server) handleGames(w http.ResponseWriter, r *http.Request, admin string) {
	if !s.requireLog(w) {
		return
	}
	q := &query{w: w, r: r}
	gq := modlog.GameQuery{Name: q.text("name", maxQueryName), IP: q.ip(), Match: uint64(q.id("match")),
		Before: q.id("before"), Limit: q.limit()}
	if !q.ok() {
		return
	}
	ctx, cancel := queryContext(r)
	defer cancel()
	rows, more, err := s.log.Games(ctx, gq)
	if err != nil {
		s.logFailed(w, r, err)
		return
	}
	out := make([]gameDTO, 0, len(rows))
	for _, g := range rows {
		out = append(out, toGameDTO(g))
	}
	WriteJSON(w, http.StatusOK, map[string]any{"games": out, "more": more})
}

func (s *Server) handleGame(w http.ResponseWriter, r *http.Request, admin string) {
	if !s.requireLog(w) {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		WriteError(w, http.StatusNotFound, "no such game")
		return
	}
	ctx, cancel := queryContext(r)
	defer cancel()
	game, events, chatCount, err := s.log.GameByID(ctx, id)
	if errors.Is(err, modlog.ErrNotFound) {
		WriteError(w, http.StatusNotFound, "no such game")
		return
	}
	if err != nil {
		s.logFailed(w, r, err)
		return
	}
	type eventDTO struct {
		Time int64  `json:"time"`
		Type string `json:"type"`
		Text string `json:"text"`
	}
	out := make([]eventDTO, 0, len(events))
	for _, e := range events {
		out = append(out, eventDTO{Time: e.Time, Type: e.Type, Text: e.Text})
	}
	WriteJSON(w, http.StatusOK, map[string]any{"game": toGameDTO(game), "events": out, "chatCount": chatCount})
}

func (s *Server) handleAudit(w http.ResponseWriter, r *http.Request, admin string) {
	if !s.requireLog(w) {
		return
	}
	q := &query{w: w, r: r}
	before, limit := q.id("before"), q.limit()
	if !q.ok() {
		return
	}
	ctx, cancel := queryContext(r)
	defer cancel()
	rows, more, err := s.log.AuditLog(ctx, before, limit)
	if err != nil {
		s.logFailed(w, r, err)
		return
	}
	type entryDTO struct {
		ID     int64  `json:"id"`
		Time   int64  `json:"time"`
		Admin  string `json:"admin"`
		Action string `json:"action"`
		Detail string `json:"detail"`
		Source string `json:"source"`
		IP     string `json:"ip"`
	}
	out := make([]entryDTO, 0, len(rows))
	for _, e := range rows {
		out = append(out, entryDTO{ID: e.ID, Time: e.Time, Admin: e.Admin, Action: e.Action, Detail: e.Detail,
			Source: e.Source, IP: e.IP})
	}
	WriteJSON(w, http.StatusOK, map[string]any{"entries": out, "more": more})
}
