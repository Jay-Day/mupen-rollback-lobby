package modsite

import (
	"net/http"
	"time"

	"github.com/jay-day/rmgk-lobby/internal/modlog"
	"github.com/jay-day/rmgk-lobby/internal/ws"
)

// The lobby's request limits (internal/ratelimit) and the IPs that hit them:
// what's in effect, and per IP when and how often it was refused, the names
// it used, its automatic bans and its current ban or mute. Banning, timing
// out and unbanning use the Sanctions endpoints; forgetting a record is here.
//
//	GET  /api/admin/ratelimits?ip=&limit=
//	POST /api/admin/ratelimits/forget   {"ip": "203.0.113.7"}

// RateLimits is the lobby's request limiter (*ws.Hub).
type RateLimits interface {
	RateLimitReport() ws.RateLimitReport
	ForgetRateLimit(ip string) bool
}

func (s *Server) registerRateLimits() {
	s.Handle("GET /api/admin/ratelimits", s.handleRateLimits)
	s.Handle("POST /api/admin/ratelimits/forget", s.handleForgetRateLimit)
}

func (s *Server) requireRateLimits(w http.ResponseWriter) bool {
	if s.rates == nil {
		WriteError(w, http.StatusServiceUnavailable, "the lobby's rate limits aren't attached to this site")
		return false
	}
	return true
}

type rateKindDTO struct {
	Kind          string `json:"kind"`
	Count         int    `json:"count"` // 0: no limit
	WindowSeconds int64  `json:"windowSeconds"`
}

type rateRecordDTO struct {
	IP              string         `json:"ip"`
	FirstTrip       int64          `json:"firstTrip"`
	LastTrip        int64          `json:"lastTrip"`
	Refused         map[string]int `json:"refused"`
	Allowed         map[string]int `json:"allowed"` // in each kind's current window
	TotalRefused    int            `json:"totalRefused"`
	Usernames       []string       `json:"usernames"`
	AutoBans        int            `json:"autoBans"`
	AutoBannedAt    int64          `json:"autoBannedAt"`
	AutoBannedUntil int64          `json:"autoBannedUntil"`
	Ban             *sanctionDTO   `json:"ban"`
	Mute            *sanctionDTO   `json:"mute"`
	Online          []string       `json:"online"` // names connected from it now
}

func seconds(d time.Duration) int64 { return int64(d / time.Second) }

func (s *Server) handleRateLimits(w http.ResponseWriter, r *http.Request, admin string) {
	if !s.requireRateLimits(w) {
		return
	}
	q := query{w: w, r: r}
	ip := ""
	if raw := q.text("ip", maxQueryIP); raw != "" {
		if ip = parseIP(raw); ip == "" {
			q.fail("ip")
		}
	}
	limit := q.limit()
	if !q.ok() {
		return
	}
	if limit == 0 {
		limit = modlog.DefaultLimit
	}
	rep := s.rates.RateLimitReport()
	kinds := make([]rateKindDTO, 0, len(rep.Kinds))
	for _, k := range rep.Kinds {
		kinds = append(kinds, rateKindDTO{Kind: k.Kind, Count: k.Count, WindowSeconds: seconds(k.Window)})
	}
	records := make([]rateRecordDTO, 0, min(limit, len(rep.Records)))
	matching := 0
	for _, rec := range rep.Records {
		if ip != "" && rec.IP != ip {
			continue
		}
		matching++
		if len(records) >= limit {
			continue
		}
		d := rateRecordDTO{IP: rec.IP, FirstTrip: unixOrZero(rec.FirstTrip), LastTrip: unixOrZero(rec.LastTrip),
			Refused: map[string]int{}, Allowed: map[string]int{}, TotalRefused: rec.TotalRefused,
			Usernames: rec.Usernames, AutoBans: rec.AutoBans, AutoBannedAt: unixOrZero(rec.AutoBannedAt),
			AutoBannedUntil: unixOrZero(rec.BannedUntil), Online: []string{}}
		for k, n := range rec.Refused {
			d.Refused[string(k)] = n
		}
		for k, n := range rec.Allowed {
			d.Allowed[string(k)] = n
		}
		if d.Usernames == nil {
			d.Usernames = []string{}
		}
		if s.lobby != nil {
			ban, mute := s.lobby.SanctionsOn(rec.IP)
			d.Ban, d.Mute = toSanction(ban), toSanction(mute)
			for _, ref := range s.lobby.OnlineAt(rec.IP) {
				d.Online = append(d.Online, ref.Username)
			}
		}
		records = append(records, d)
	}
	WriteJSON(w, http.StatusOK, map[string]any{
		"attached":              rep.Attached,
		"exemptLoopback":        rep.ExemptLoopback,
		"kinds":                 kinds,
		"banStrikes":            rep.BanStrikes,
		"banWindowSeconds":      seconds(rep.BanWindow),
		"banSeconds":            seconds(rep.BanDuration),
		"reportDays":            int64(rep.ReportTTL / (24 * time.Hour)),
		"roomListMinIntervalMs": int64(rep.RoomListMinInterval / time.Millisecond),
		"total":                 matching,
		"more":                  matching > len(records),
		"records":               records,
	})
}

func (s *Server) handleForgetRateLimit(w http.ResponseWriter, r *http.Request, admin string) {
	if !s.requireRateLimits(w) {
		return
	}
	var req actionRequest
	if _, ok := req.read(w, r); !ok {
		return
	}
	ip := parseIP(req.IP)
	if ip == "" {
		WriteError(w, http.StatusBadRequest, "that isn't an IP address")
		return
	}
	if !s.rates.ForgetRateLimit(ip) {
		WriteError(w, http.StatusNotFound, "no rate limit record for "+ip)
		return
	}
	s.audit(admin, "forget-rate-limit", "Cleared the rate limit record of "+ip, RemoteIP(r))
	WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
}
