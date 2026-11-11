// Package web serves the public rankings page and its read-only JSON API.
//
// The page is a single embedded HTML file (no external assets) so it loads in
// one request; the API reads straight from the accounts store.
//
//	GET /                          rankings page (also /rankings)
//	GET /api/ranked/overview       ladders, rulesets, rules, tiers, rating constants
//	GET /api/ranked/leaderboard    ?ladder=<id>&ruleset=<roster id>
//	GET /api/ranked/player         ?name=<username>
//	GET /api/ranked/sets           ?ladder=<id>&ruleset=<roster id>&limit=<n>
//	GET /api/ranked/sets/{id}      one set with replay info and bookmarks
//	GET /api/ranked/sets/{id}/replay   the set's .krec
//	GET /api/auth/discord          Discord sign-in settings (404 when off)
//	GET /api/auth/email            whether the server can email reset codes (404 when off)
//	POST /api/auth/password-reset  {"username","email"}: email that account a reset code
package web

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"math"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jay-day/rmgk-lobby/internal/accounts"
	"github.com/jay-day/rmgk-lobby/internal/discord"
	"github.com/jay-day/rmgk-lobby/internal/geo"
	"github.com/jay-day/rmgk-lobby/internal/ranked"
	"github.com/jay-day/rmgk-lobby/internal/rating"
)

//go:embed index.html
var indexHTML []byte

// Server answers the rankings page and API.
type Server struct {
	store     *accounts.Store
	cfg       *ranked.Config
	replayDir string
	admin     *adminState // nil until EnableAdmin
	clientIP  func(*http.Request) string
	discord   *discord.Client // nil when Discord sign-in is off
	// Emails a reset code to an account's confirmed address when the given
	// address is that one; nil when the server has no email. Returns a wait
	// > 0 when the IP is over its limit.
	requestReset func(username, email, ip string) time.Duration

	indexGzip []byte
	indexETag string
}

// New builds a Server; cfg may have no ladders (the page then says so).
// replayDir holds uploaded replays ("" disables replay downloads).
func New(store *accounts.Store, cfg *ranked.Config, replayDir string) *Server {
	s := &Server{store: store, cfg: cfg, replayDir: replayDir}
	var buf bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	zw.Write(indexHTML)
	zw.Close()
	s.indexGzip = buf.Bytes()
	sum := sha256.Sum256(indexHTML)
	s.indexETag = `"` + hex.EncodeToString(sum[:8]) + `"`
	return s
}

// Register adds the page and API routes to mux.
func (s *Server) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /{$}", s.handleIndex)
	mux.HandleFunc("GET /rankings", s.handleIndex)
	mux.HandleFunc("GET /api/ranked/overview", s.handleOverview)
	mux.HandleFunc("GET /api/auth/discord", s.handleDiscordConfig)
	mux.HandleFunc("GET /api/auth/email", s.handleEmailConfig)
	mux.HandleFunc("POST /api/auth/password-reset", s.handlePasswordResetRequest)
	mux.HandleFunc("GET /api/ranked/leaderboard", s.handleLeaderboard)
	mux.HandleFunc("GET /api/ranked/player", s.handlePlayer)
	mux.HandleFunc("GET /api/ranked/sets", s.handleSets)
	mux.HandleFunc("GET /api/ranked/sets/{id}", s.handleSet)
	mux.HandleFunc("GET /api/ranked/sets/{id}/replay", s.handleReplay)
	s.registerAdmin(mux)
}

func acceptsGzip(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept-Encoding"), "gzip")
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "public, max-age=300")
	h.Set("ETag", s.indexETag)
	h.Set("Vary", "Accept-Encoding")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", "default-src 'none'; script-src 'unsafe-inline'; "+
		"style-src 'unsafe-inline'; img-src data:; connect-src 'self'; base-uri 'none'; form-action 'none'; "+
		"frame-ancestors 'none'")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "same-origin")
	if r.Header.Get("If-None-Match") == s.indexETag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	if acceptsGzip(r) {
		h.Set("Content-Encoding", "gzip")
		w.Write(s.indexGzip)
		return
	}
	w.Write(indexHTML)
}

func writeJSON(w http.ResponseWriter, r *http.Request, status int, v any) {
	data, err := json.Marshal(v)
	if err != nil {
		http.Error(w, "encoding error", http.StatusInternalServerError)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "application/json")
	if h.Get("Cache-Control") == "" { // admin responses already say no-store
		h.Set("Cache-Control", "no-cache")
	}
	h.Set("X-Content-Type-Options", "nosniff")
	if len(data) > 1024 && acceptsGzip(r) {
		h.Set("Content-Encoding", "gzip")
		h.Set("Vary", "Accept-Encoding")
		w.WriteHeader(status)
		zw := gzip.NewWriter(w)
		zw.Write(data)
		zw.Close()
		return
	}
	w.WriteHeader(status)
	w.Write(data)
}

func writeError(w http.ResponseWriter, r *http.Request, status int, message string) {
	writeJSON(w, r, status, map[string]string{"error": message})
}

//
// Overview
//

type rulesetDTO struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	// Stages in stage select order; empty when the config doesn't list them.
	Stages []string `json:"stages"`
	// Characters that have been played on this roster (ids), for filters.
	Characters []int `json:"characters"`
}

type romDTO struct {
	Name string `json:"name"`
	MD5  string `json:"md5"`
}

type ladderDTO struct {
	ID       string       `json:"id"`
	Name     string       `json:"name"`
	ModuleID string       `json:"moduleId"`
	Rules    []string     `json:"rules"`
	Rulesets []rulesetDTO `json:"rulesets"`
	BestOf   int          `json:"bestOf"`
	Roms     []romDTO     `json:"roms"`
	// TeamSize: 1 for 1v1, 2 for 2v2 team battles (rated with OpenSkill)
	TeamSize int `json:"teamSize"`
	// placement ends below this ±; tiers use rating - tierDeviations × ±
	PlacementDeviation float64 `json:"placementDeviation"`
	TierDeviations     float64 `json:"tierDeviations"`
}

// glickoDTO publishes the rating constants so the site's scoring page and
// calculator use the server's values.
type glickoDTO struct {
	Rating            float64 `json:"rating"`
	Deviation         float64 `json:"deviation"`
	Volatility        float64 `json:"volatility"`
	MinDeviation      float64 `json:"minDeviation"`
	MaxDeviation      float64 `json:"maxDeviation"`
	Tau               float64 `json:"tau"`
	RatingPeriodHours float64 `json:"ratingPeriodHours"`
}

type tierDTO struct {
	Name            string  `json:"name"`
	MinConservative float64 `json:"minConservative"`
}

func (s *Server) handleOverview(w http.ResponseWriter, r *http.Request) {
	out := struct {
		Ladders              []ladderDTO     `json:"ladders"`
		Tiers                []tierDTO       `json:"tiers"`
		ProvisionalDeviation float64         `json:"provisionalDeviation"`
		Glicko               glickoDTO       `json:"glicko"`
		Regions              []geo.Continent `json:"regions"`
	}{Ladders: []ladderDTO{}, Regions: geo.Continents, ProvisionalDeviation: rating.ProvisionalDeviation, Glicko: glickoDTO{
		Rating: rating.DefaultRating, Deviation: rating.DefaultDeviation, Volatility: rating.DefaultVolatility,
		MinDeviation: rating.MinDeviation, MaxDeviation: rating.MaxDeviation, Tau: rating.Tau,
		RatingPeriodHours: rating.RatingPeriod.Hours(),
	}}

	rosterCharacters, err := s.store.RosterCharacters()
	if err != nil {
		slog.Warn("roster characters query failed", "err", err)
	}
	for _, l := range s.cfg.Ladders {
		policy := l.Policy()
		dto := ladderDTO{ID: l.ID, Name: l.Name, ModuleID: l.ModuleID, Rules: l.Rules, BestOf: int(l.BestOf), TeamSize: l.TeamSize,
			PlacementDeviation: policy.PlacementDeviation, TierDeviations: policy.TierDeviations}
		if dto.Rules == nil {
			dto.Rules = []string{}
		}
		for _, id := range l.Rulesets {
			characters := rosterCharacters[[2]string{l.ID, id}]
			if characters == nil {
				characters = []int{}
			}
			stages := l.RulesetStages[id]
			if stages == nil {
				stages = []string{}
			}
			dto.Rulesets = append(dto.Rulesets, rulesetDTO{ID: id, Name: l.RulesetName(id),
				Description: l.RulesetDescriptions[id], Stages: stages, Characters: characters})
		}
		for _, rom := range l.Roms {
			dto.Roms = append(dto.Roms, romDTO{Name: rom.Name, MD5: rom.MD5})
		}
		out.Ladders = append(out.Ladders, dto)
	}
	for i, band := range s.cfg.Tiers.Bands {
		min := band.MinConservative
		if i == 0 {
			min = 0 // the lowest band starts at -inf; show it as "below the next"
		}
		out.Tiers = append(out.Tiers, tierDTO{Name: band.Name, MinConservative: min})
	}
	writeJSON(w, r, http.StatusOK, out)
}

//
// Leaderboard
//

type playerRowDTO struct {
	Rank        int    `json:"rank"` // 0 while provisional
	Username    string `json:"username"`
	Rating      int    `json:"rating"`
	Deviation   int    `json:"deviation"`
	Tier        string `json:"tier"`
	TierIndex   int    `json:"tierIndex"`
	Provisional bool   `json:"provisional"`
	Sets        int    `json:"sets"`
	Wins        int    `json:"wins"`
	Losses      int    `json:"losses"`
	LastPlayed  int64  `json:"lastPlayed"`          // unix seconds
	Region      string `json:"region,omitempty"`    // home continent id (geo.Continents)
	WorldRank   int    `json:"worldRank,omitempty"` // with a region filter: the worldwide rank
	// characters played on this roster, most played first (the leaderboard
	// keeps the top leaderboardCharacters and counts the rest)
	Characters     []characterShareDTO `json:"characters,omitempty"`
	MoreCharacters int                 `json:"moreCharacters,omitempty"`
}

type characterShareDTO struct {
	ID      int `json:"id"`
	Sets    int `json:"sets"`
	Percent int `json:"percent"` // of the player's sets on the roster
}

// leaderboardCharacters is how many characters a leaderboard row shows.
const leaderboardCharacters = 3

// rankedRows returns the players of one roster of a ladder: established
// players ranked by rating, then players still in placement (rank 0), also
// by rating.
// homes is HomeRegions, fetched once per request.
func (s *Server) rankedRows(ladderID, rulesetID string, homes map[int64]string) ([]playerRowDTO, error) {
	entries, err := s.store.Leaderboard(ladderID, rulesetID)
	policy := s.cfg.Policy(ladderID)
	if err != nil {
		return nil, err
	}
	shares, err := s.store.RosterCharacterShares(ladderID, rulesetID)
	if err != nil {
		return nil, err
	}
	rows := make([]playerRowDTO, 0, len(entries))
	for _, e := range entries {
		place := s.cfg.Tiers.PlaceWith(e.Rating, policy)
		rows = append(rows, playerRowDTO{
			Username:    e.Username,
			Rating:      int(math.Round(e.Rating.Rating)),
			Deviation:   int(math.Round(e.Rating.Deviation)),
			Tier:        place.Tier,
			TierIndex:   place.TierIndex,
			Provisional: place.Provisional,
			Sets:        e.SetsPlayed,
			Wins:        e.Wins,
			Losses:      e.Losses,
			LastPlayed:  e.LastPlayed.Unix(),
			Region:      geo.ContinentOf(homes[e.AccountID]),
			Characters:  characterShares(shares[e.AccountID]),
		})
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].Provisional != rows[j].Provisional {
			return !rows[i].Provisional
		}
		if rows[i].Rating != rows[j].Rating {
			return rows[i].Rating > rows[j].Rating
		}
		return strings.ToLower(rows[i].Username) < strings.ToLower(rows[j].Username)
	})
	rank := 0
	for i := range rows {
		if rows[i].Provisional {
			continue
		}
		rank++
		rows[i].Rank = rank
	}
	return rows, nil
}

func characterShares(shares []accounts.CharacterShare) []characterShareDTO {
	out := make([]characterShareDTO, 0, len(shares))
	for _, c := range shares {
		out = append(out, characterShareDTO{ID: c.Character, Sets: c.Sets, Percent: c.Percent})
	}
	return out
}

// regionRows keeps one continent's players, ranked within it; WorldRank
// keeps their worldwide place.
func regionRows(rows []playerRowDTO, region string) []playerRowDTO {
	out := make([]playerRowDTO, 0, len(rows))
	rank := 0
	for _, row := range rows {
		if row.Region != region {
			continue
		}
		row.WorldRank = row.Rank
		if !row.Provisional {
			rank++
			row.Rank = rank
		}
		out = append(out, row)
	}
	return out
}

// EnableDiscord publishes the Discord sign-in settings RMG-K needs.
func (s *Server) EnableDiscord(client *discord.Client) { s.discord = client }

// handleDiscordConfig tells RMG-K how to start a Discord sign-in: the
// application, the authorize page and the loopback redirects it may listen
// on. 404 when the server has no Discord application.
func (s *Server) handleDiscordConfig(w http.ResponseWriter, r *http.Request) {
	if s.discord == nil {
		writeError(w, r, http.StatusNotFound, "Discord sign-in isn't enabled on this server")
		return
	}
	writeJSON(w, r, http.StatusOK, map[string]any{
		"clientId":     s.discord.ClientID(),
		"authorizeUrl": s.discord.AuthorizeURL(),
		"redirectUris": s.discord.RedirectURIs(),
		"scope":        discord.Scope,
	})
}

func (s *Server) ladderOrDefault(id string) (*ranked.Ladder, bool) {
	if id == "" && len(s.cfg.Ladders) > 0 {
		return &s.cfg.Ladders[0], true
	}
	return s.cfg.Ladder(id)
}

// handleLeaderboard lists one roster's rankings: each roster of a ladder is
// rated separately (the ladder's default roster when none is given).
func (s *Server) handleLeaderboard(w http.ResponseWriter, r *http.Request) {
	ladder, ok := s.ladderOrDefault(r.URL.Query().Get("ladder"))
	if !ok {
		writeError(w, r, http.StatusNotFound, "unknown ladder")
		return
	}
	ruleset, ok := ladder.Ruleset(r.URL.Query().Get("ruleset"))
	if !ok {
		writeError(w, r, http.StatusNotFound, "unknown roster")
		return
	}
	region := r.URL.Query().Get("region")
	if region != "" && geo.ContinentName(region) == "" {
		writeError(w, r, http.StatusNotFound, "unknown region")
		return
	}
	homes, err := s.store.HomeRegions()
	if err != nil {
		slog.Error("home regions query failed", "err", err)
		writeError(w, r, http.StatusInternalServerError, "rankings are temporarily unavailable")
		return
	}
	rows, err := s.rankedRows(ladder.ID, ruleset, homes)
	if err != nil {
		slog.Error("leaderboard query failed", "ladder", ladder.ID, "ruleset", ruleset, "err", err)
		writeError(w, r, http.StatusInternalServerError, "rankings are temporarily unavailable")
		return
	}
	if region != "" {
		rows = regionRows(rows, region)
	}
	for i := range rows {
		if extra := len(rows[i].Characters) - leaderboardCharacters; extra > 0 {
			rows[i].Characters = rows[i].Characters[:leaderboardCharacters]
			rows[i].MoreCharacters = extra
		}
	}
	writeJSON(w, r, http.StatusOK, map[string]any{"ladder": ladder.ID, "ruleset": ruleset, "region": region, "players": rows})
}

//
// Sets
//

type setPlayerDTO struct {
	Username   string `json:"username"`
	Slot       int    `json:"slot"`
	Team       int    `json:"team,omitempty"` // 1 or 2 in team sets
	Games      int    `json:"games"`
	Before     int    `json:"before"`
	After      int    `json:"after"`
	Characters []int  `json:"characters"`
}

type setDTO struct {
	ID            int64          `json:"id"`
	Ladder        string         `json:"ladder"`
	Status        string         `json:"status"`
	Reason        string         `json:"reason,omitempty"`
	Time          int64          `json:"time"`
	Winner        string         `json:"winner,omitempty"`     // 1v1
	WinnerTeam    int            `json:"winnerTeam,omitempty"` // team sets
	Players       []setPlayerDTO `json:"players"`
	Ruleset       string         `json:"ruleset,omitempty"`
	RulesetName   string         `json:"rulesetName,omitempty"`
	ModuleVersion string         `json:"moduleVersion,omitempty"`
	HasReplay     bool           `json:"hasReplay"`
	ReplayURL     string         `json:"replayUrl,omitempty"`
}

// replayPath is where a set's replay downloads from.
func replayPath(setID int64) string {
	return "/api/ranked/sets/" + strconv.FormatInt(setID, 10) + "/replay"
}

func (s *Server) toSetDTO(set accounts.SetSummary) setDTO {
	chars := accounts.CharactersBySlot(set.ResultJSON)
	dto := setDTO{
		ID: set.ID, Ladder: set.Ladder, Status: string(set.Status), Reason: set.Reason,
		Time: set.CreatedAt.Unix(), Players: []setPlayerDTO{},
		Ruleset: set.RulesetID, ModuleVersion: set.ModuleVersion, HasReplay: set.HasReplay,
		WinnerTeam: set.WinnerTeam,
	}
	if set.HasReplay && s.replayDir != "" {
		dto.ReplayURL = replayPath(set.ID)
	}
	if ladder, ok := s.cfg.Ladder(set.Ladder); ok && set.RulesetID != "" {
		dto.RulesetName = ladder.RulesetName(set.RulesetID)
	}
	for _, p := range set.Players {
		if p.AccountID == set.WinnerID && set.WinnerID != 0 {
			dto.Winner = p.Username
		}
		characters := chars[p.Slot]
		if characters == nil {
			characters = []int{}
		}
		dto.Players = append(dto.Players, setPlayerDTO{
			Username: p.Username, Slot: p.Slot, Team: p.Team, Games: p.GamesWon,
			Before: int(math.Round(p.Before.Rating)), After: int(math.Round(p.After.Rating)),
			Characters: characters,
		})
	}
	return dto
}

// handleSets lists sets, newest first, with optional filters:
//
//	ladder, ruleset   one roster of a ladder
//	player            sets this player played (username)
//	opponent          with player: only against this player
//	character         a character id, played by player when given
//	result            win or loss, with player
//	offset, limit     paging (limit at most 200)
//
// The response carries the page and the total number of matching sets.
func (s *Server) handleSets(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := accounts.SetFilter{Ruleset: q.Get("ruleset")}
	if q.Get("ladder") != "" || f.Ruleset != "" {
		ladder, ok := s.ladderOrDefault(q.Get("ladder"))
		if !ok {
			writeError(w, r, http.StatusNotFound, "unknown ladder")
			return
		}
		f.Ladder = ladder.ID
		if f.Ruleset != "" {
			if _, ok := ladder.Ruleset(f.Ruleset); !ok {
				writeError(w, r, http.StatusNotFound, "unknown roster")
				return
			}
		}
	}
	for _, who := range []struct {
		param string
		id    *int64
	}{{"player", &f.AccountID}, {"opponent", &f.OpponentID}} {
		name := strings.TrimSpace(q.Get(who.param))
		if name == "" {
			continue
		}
		account, err := s.store.AccountByName(name)
		if errors.Is(err, accounts.ErrNotFound) {
			writeError(w, r, http.StatusNotFound, "no player named "+name)
			return
		}
		if err != nil {
			writeError(w, r, http.StatusInternalServerError, "sets are temporarily unavailable")
			return
		}
		*who.id = account.ID
	}
	if f.OpponentID != 0 && f.AccountID == 0 {
		writeError(w, r, http.StatusBadRequest, "opponent needs a player")
		return
	}
	if c := q.Get("character"); c != "" {
		id, err := strconv.Atoi(c)
		if err != nil || id < 0 {
			writeError(w, r, http.StatusBadRequest, "bad character")
			return
		}
		f.Character, f.HasCharacter = id, true
	}
	switch f.Result = q.Get("result"); f.Result {
	case "":
	case "win", "loss":
		if f.AccountID == 0 {
			writeError(w, r, http.StatusBadRequest, "result needs a player")
			return
		}
	default:
		writeError(w, r, http.StatusBadRequest, "result is win or loss")
		return
	}
	f.Limit, _ = strconv.Atoi(q.Get("limit"))
	f.Offset, _ = strconv.Atoi(q.Get("offset"))

	sets, total, err := s.store.FindSets(f)
	if err != nil {
		slog.Error("sets query failed", "err", err)
		writeError(w, r, http.StatusInternalServerError, "sets are temporarily unavailable")
		return
	}
	out := make([]setDTO, 0, len(sets))
	for _, set := range sets {
		out = append(out, s.toSetDTO(set))
	}
	writeJSON(w, r, http.StatusOK, map[string]any{"sets": out, "total": total})
}

func parseSetID(r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	return id, err == nil && id > 0
}

type replayDTO struct {
	Size      int64           `json:"size"`
	SHA256    string          `json:"sha256"`
	RomMD5    string          `json:"romMd5"`
	ModuleID  string          `json:"moduleId"`
	Version   string          `json:"moduleVersion"`
	Ruleset   string          `json:"ruleset"`
	Bookmarks json.RawMessage `json:"bookmarks"`
	URL       string          `json:"url"`
}

func (s *Server) handleSet(w http.ResponseWriter, r *http.Request) {
	id, ok := parseSetID(r)
	if !ok {
		writeError(w, r, http.StatusNotFound, "no such set")
		return
	}
	set, err := s.store.SetByID(id)
	if errors.Is(err, accounts.ErrNotFound) {
		writeError(w, r, http.StatusNotFound, "no such set")
		return
	}
	if err != nil {
		slog.Error("set lookup failed", "set", id, "err", err)
		writeError(w, r, http.StatusInternalServerError, "set lookup is temporarily unavailable")
		return
	}
	out := struct {
		setDTO
		Replay *replayDTO `json:"replay,omitempty"`
	}{setDTO: s.toSetDTO(set)}
	if info, err := s.store.Replay(id); err == nil {
		moduleID := ""
		if ladder, ok := s.cfg.Ladder(set.Ladder); ok {
			moduleID = ladder.ModuleID
		}
		out.Replay = &replayDTO{
			Size: info.Size, SHA256: info.SHA256, RomMD5: set.RomMD5, ModuleID: moduleID,
			Version: set.ModuleVersion, Ruleset: set.RulesetID, Bookmarks: json.RawMessage(info.Bookmarks),
			URL: replayPath(id),
		}
	}
	writeJSON(w, r, http.StatusOK, out)
}

// handleReplay serves a stored .krec. Replays never change, so they're
// cacheable forever; the hash doubles as the ETag.
func (s *Server) handleReplay(w http.ResponseWriter, r *http.Request) {
	id, ok := parseSetID(r)
	if !ok || s.replayDir == "" {
		http.NotFound(w, r)
		return
	}
	info, err := s.store.Replay(id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	data, err := os.ReadFile(accounts.ReplayFile(s.replayDir, id))
	if err != nil {
		slog.Error("replay file missing", "set", id, "err", err)
		http.NotFound(w, r)
		return
	}
	etag := `"` + info.SHA256 + `"`
	h := w.Header()
	h.Set("Content-Type", "application/octet-stream")
	h.Set("Content-Disposition", `attachment; filename="rmgk-set-`+strconv.FormatInt(id, 10)+`.krec"`)
	h.Set("Cache-Control", "public, max-age=31536000, immutable")
	h.Set("ETag", etag)
	h.Set("X-Content-Type-Options", "nosniff")
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	if acceptsGzip(r) {
		h.Set("Content-Encoding", "gzip")
		h.Set("Vary", "Accept-Encoding")
		zw := gzip.NewWriter(w)
		zw.Write(data)
		zw.Close()
		return
	}
	w.Write(data)
}

//
// Player
//

type standingDTO struct {
	Ladder      string `json:"ladder"`
	LadderName  string `json:"ladderName"`
	Ruleset     string `json:"ruleset"`
	RulesetName string `json:"rulesetName"`
	RegionRank  int    `json:"regionRank,omitempty"` // rank in the player's home continent
	playerRowDTO
}

// boardKey names one roster of a ladder in the player history map.
func boardKey(ladder, ruleset string) string { return ladder + "/" + ruleset }

type historyPointDTO struct {
	Time   int64 `json:"time"`
	Rating int   `json:"rating"`
}

func (s *Server) handlePlayer(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	account, err := s.store.AccountByName(name)
	if errors.Is(err, accounts.ErrNotFound) || name == "" {
		writeError(w, r, http.StatusNotFound, "no such player")
		return
	}
	if err != nil {
		slog.Error("player lookup failed", "err", err)
		writeError(w, r, http.StatusInternalServerError, "player lookup is temporarily unavailable")
		return
	}

	homes, err := s.store.HomeRegions()
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "player lookup is temporarily unavailable")
		return
	}
	// one standing per roster the player has played
	standings := []standingDTO{}
	for _, l := range s.cfg.Ladders {
		for _, ruleset := range l.Rulesets {
			rows, err := s.rankedRows(l.ID, ruleset, homes)
			if err != nil {
				writeError(w, r, http.StatusInternalServerError, "player lookup is temporarily unavailable")
				return
			}
			for _, row := range rows {
				if strings.EqualFold(row.Username, account.Username) {
					standing := standingDTO{Ladder: l.ID, LadderName: l.Name,
						Ruleset: ruleset, RulesetName: l.RulesetName(ruleset), playerRowDTO: row}
					if row.Region != "" {
						for _, regional := range regionRows(rows, row.Region) {
							if strings.EqualFold(regional.Username, account.Username) {
								standing.RegionRank = regional.Rank
							}
						}
					}
					standings = append(standings, standing)
				}
			}
		}
	}

	points, err := s.store.RatingHistory(account.ID)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "player lookup is temporarily unavailable")
		return
	}
	history := map[string][]historyPointDTO{}
	for _, p := range points {
		key := boardKey(p.Ladder, p.Ruleset)
		history[key] = append(history[key], historyPointDTO{Time: p.Time.Unix(), Rating: int(math.Round(p.Rating))})
	}

	uses, err := s.store.PlayerCharacters(account.ID)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "player lookup is temporarily unavailable")
		return
	}
	type characterDTO struct {
		Ladder    string `json:"ladder"`
		Ruleset   string `json:"ruleset"`
		Character int    `json:"character"`
		Sets      int    `json:"sets"`
	}
	characters := make([]characterDTO, 0, len(uses))
	for _, u := range uses {
		characters = append(characters, characterDTO{u.Ladder, u.Ruleset, u.Character, u.Sets})
	}

	writeJSON(w, r, http.StatusOK, map[string]any{
		"username":   account.Username,
		"region":     geo.ContinentOf(homes[account.ID]),
		"joined":     account.CreatedAt.Unix(),
		"standings":  standings,
		"history":    history,
		"characters": characters,
	})
}
