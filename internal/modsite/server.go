// Package modsite is the moderator website: a sign-in page, then Live (who's
// online, rooms, lobby chat, with kick / mute / timeout / ban and close room),
// Chat (the chat log), People (IP tracing), Games, Sanctions, Rate limits
// (the IPs the lobby's request limits refused) and the Audit log. It is
// served on its own listener (lobby-server --admin-addr, default
// 127.0.0.1:8081), never on the players' /ws port.
//
// Security model, in the order a request meets it:
//
//   - The client IP is the TCP peer address only; no header is trusted.
//   - lobby.json adminAllowedIPs, when not empty, limits which IPs get any
//     answer at all, and a banned IP gets none (loopback always may; see
//     LoginGuard). The listener resets their connections as soon as it
//     accepts them, before reading any HTTP, so they can't hold its
//     connections either. One address holds at most 8 of its 64 (loopback
//     32), and every signed-out response closes its connection. None of this
//     replaces a host firewall.
//   - A request must name the site by IP address or localhost (there is no
//     domain): another name is a DNS rebinding attempt, and gets a bare 404.
//   - Signed out, only GET / (the sign-in page) and POST /api/admin/login
//     answer; every other path is a bare 404: Go's own, without the site's
//     headers, as on any Go server.
//   - Sign-in is the moderator password from admins.json, through the shared
//     moderation.LoginGuard: lobby.json adminLoginMaxFailures (5) wrong
//     passwords in a row ban the IP for adminLoginBanSeconds (0: for good);
//     loopback gets a 15 minute lockout instead. Every failure is one
//     generic message.
//   - A session is a 256-bit random token in an HttpOnly SameSite=Strict
//     cookie, stored hashed in memory, bound to the IP that signed in, ending
//     after 12 hours, after 60 idle minutes, at sign-out, when its admins.json
//     entry changes or goes, and when the server restarts.
//   - Every API call needs the X-RMGK-Admin header, changes are POST with a
//     JSON body, and a request whose Origin or Referer names another host is
//     refused: a cross-site page can't make a signed-in browser do anything.
//   - The sign-in page and every signed-in response carry no-store, nosniff,
//     DENY framing, no-referrer, noindex and a Content-Security-Policy; the
//     pages' inline script and
//     style are allowed by hash, nothing else loads. Player text only ever
//     reaches the page as text (no innerHTML).
//   - Bodies are capped (maxBody), queries are checked and every list is
//     paged; the HTTP server has timeouts and a connection cap.
//
// Extending it (the Rate limits tab, ratelimits.go, is built this way):
//
//  1. A JSON endpoint: Server.Handle("GET /api/admin/ratelimits", func(w, r,
//     admin) { WriteJSON(w, http.StatusOK, report) }). It is signed-in only
//     and gets the same checks as every other endpoint.
//  2. A tab: in app.html, add ["ratelimits", "Rate limits"] to TABS and a
//     ratelimits entry to PAGES that fetches it with api("GET", "ratelimits").
//  3. The data: the lobby side exposes it as a method (as ws.Hub does for
//     LiveSnapshot and RateLimitReport), which main.go wires in through
//     Config.
package modsite

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/jay-day/rmgk-lobby/internal/moderation"
	"github.com/jay-day/rmgk-lobby/internal/modlog"
	"github.com/jay-day/rmgk-lobby/internal/ws"
)

const (
	// AdminHeader must be "1" on every API call; a cross-site form or image
	// can't send it, and a cross-site fetch can't without a CORS preflight,
	// which this server never answers.
	AdminHeader = "X-RMGK-Admin"
	// backgroundHeader marks the pages' own refreshes, which don't count as
	// activity for the idle timeout.
	backgroundHeader = "X-RMGK-Background"

	maxBody = 16 << 10
	// how long one request's log queries may run
	queryTimeout = 10 * time.Second
	// how many connections the site's listener holds at once
	maxConns = 64
)

// Lobby is the running lobby (*ws.Hub).
type Lobby interface {
	LiveSnapshot() ws.LiveSnapshot
	ModerateUser(admin string, userID uint64, action string, dur time.Duration, reason string) (ws.ModOutcome, error)
	CloseRoom(admin string, roomID uint64, reason string) (string, bool)
	BanIP(admin, ip, name string, dur time.Duration, reason string) int
	MuteIP(admin, ip, name string, dur time.Duration, reason string) int
	LiftSanction(admin, action, ip string) bool
	Sanctions() (bans, mutes []moderation.Sanction)
	SanctionsOn(ip string) (ban, mute *moderation.Sanction)
	OnlineCount() int
	OnlineAt(ip string) []ws.LiveRef
	IsOnline(name string) bool
	ModeratorsAt(ip string) []string
	UserIP(userID uint64) (string, bool)
}

// Config is what the site works with.
type Config struct {
	Lobby Lobby               // nil: the live pages say the lobby isn't attached
	Mod   *moderation.Manager // credentials (admins.json) and bans; required
	Guard *moderation.LoginGuard
	Log   *modlog.Logger // nil: logging is off, the log pages say so
	// RateLimits is the lobby's request limiter (the Rate limits page); nil:
	// that page says it isn't attached.
	RateLimits RateLimits
	// AllowIP reports whether an IP may reach the site at all (lobby.json
	// adminAllowedIPs). nil allows everyone.
	AllowIP func(ip string) bool
}

// AdminHandler serves a signed-in moderator; admin is their name.
type AdminHandler func(w http.ResponseWriter, r *http.Request, admin string)

// Server is the moderator website. It is an http.Handler.
type Server struct {
	lobby   Lobby
	mod     *moderation.Manager
	guard   *moderation.LoginGuard
	log     *modlog.Logger
	rates   RateLimits
	allowIP func(string) bool

	sessions *sessionStore
	api      *http.ServeMux // the signed-in routes
	pages    *pages
}

// New builds the site.
func New(cfg Config) *Server {
	if cfg.Mod == nil {
		panic("modsite: Config.Mod is required")
	}
	s := &Server{
		lobby: cfg.Lobby, mod: cfg.Mod, guard: cfg.Guard, log: cfg.Log, rates: cfg.RateLimits, allowIP: cfg.AllowIP,
		sessions: newSessionStore(time.Now),
		api:      http.NewServeMux(),
		pages:    loadPages(),
	}
	s.Handle("GET /api/admin/session", s.handleSession)
	s.Handle("POST /api/admin/logout", s.handleLogout)
	s.registerLive()
	s.registerLogs()
	s.registerRateLimits()
	return s
}

// Handle adds a signed-in endpoint. pattern is a ServeMux pattern under
// /api/admin/ with its method, e.g. "GET /api/admin/ratelimits".
func (s *Server) Handle(pattern string, h AdminHandler) {
	s.api.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
		sess, _ := r.Context().Value(sessionKey{}).(*session)
		if sess == nil { // can't happen: ServeHTTP only routes signed-in requests here
			notFound(w)
			return
		}
		h(w, r, sess.admin)
	})
}

type sessionKey struct{}

// Admits reports whether ip may reach the site at all: loopback always;
// otherwise not when lobby.json adminAllowedIPs leaves it out or it's banned.
// The listener asks it before reading anything (Listen), and ServeHTTP again
// for every request (a ban may come while a connection is open).
func (s *Server) Admits(ip string) bool {
	if moderation.IsLoopback(ip) {
		return true
	}
	if s.allowIP != nil && !s.allowIP(ip) {
		return false
	}
	_, banned := s.mod.IsBanned(ip)
	return !banned
}

// ServeHTTP is the whole site.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ip := RemoteIP(r)
	if !s.Admits(ip) || !hostOK(r.Host) {
		notFound(w)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBody)

	switch {
	case r.URL.Path == "/" && (r.Method == http.MethodGet || r.Method == http.MethodHead):
		s.servePage(w, r, ip)
		return
	case r.URL.Path == "/api/admin/login" && r.Method == http.MethodPost:
		setSecurityHeaders(w.Header(), "")
		w.Header().Set("Connection", "close") // signed out: nothing stays open
		s.handleLogin(w, r, ip)
		return
	}
	if !strings.HasPrefix(r.URL.Path, "/api/admin/") {
		notFound(w)
		return
	}
	sess := s.sessions.get(r, ip, s.mod.AdminKeyValid, r.Header.Get(backgroundHeader) != "1")
	if sess == nil {
		notFound(w)
		return
	}
	setSecurityHeaders(w.Header(), "")
	if msg := checkAPIRequest(r); msg != "" {
		WriteError(w, http.StatusForbidden, msg)
		return
	}
	s.api.ServeHTTP(closeOn404{w}, r.WithContext(context.WithValue(r.Context(), sessionKey{}, sess)))
}

// closeOn404 closes the connection after any 404, signed in or not.
type closeOn404 struct{ http.ResponseWriter }

func (c closeOn404) WriteHeader(code int) {
	if code == http.StatusNotFound {
		c.Header().Set("Connection", "close")
	}
	c.ResponseWriter.WriteHeader(code)
}

func (c closeOn404) Unwrap() http.ResponseWriter { return c.ResponseWriter }

// hostOK reports whether the request names the site by an IP address or
// localhost, as moderators reach it (the server has no domain name). Any
// other name means a web page pointed its own domain at this address (DNS
// rebinding), say at a moderator's SSH tunnel on 127.0.0.1, to guess
// passwords from there; it gets nothing.
func hostOK(host string) bool {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	_, err := netip.ParseAddr(host)
	return err == nil
}

// RemoteIP is the request's TCP peer address, as the lobby writes IPs. No
// forwarding header is believed: the site is reached directly or through an
// SSH tunnel, never through a proxy.
func RemoteIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	return moderation.CanonicalIP(host)
}

// checkAPIRequest says why an API call can't be from the site's own page
// ("" when it can): the custom header, a JSON body on changes, and no other
// site's Origin or Referer.
func checkAPIRequest(r *http.Request) string {
	if r.Header.Get(AdminHeader) != "1" {
		return "missing " + AdminHeader + " header"
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
			return "send JSON"
		}
	}
	if !sameHost(r, r.Header.Get("Origin")) || !sameHost(r, r.Header.Get("Referer")) {
		return "cross-site request"
	}
	return ""
}

// sameHost reports whether a URL header (Origin, Referer) names the host
// the request was sent to; an absent header passes.
func sameHost(r *http.Request, raw string) bool {
	if raw == "" {
		return true
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return false // includes Origin: null
	}
	return strings.EqualFold(u.Host, r.Host)
}

// setSecurityHeaders sets what every response carries. csp "" is the
// strictest policy (for JSON and errors); pages pass their own.
func setSecurityHeaders(h http.Header, csp string) {
	if csp == "" {
		csp = "default-src 'none'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"
	}
	h.Set("Content-Security-Policy", csp)
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Robots-Tag", "noindex, nofollow")
	h.Set("Cross-Origin-Opener-Policy", "same-origin")
	h.Set("Cross-Origin-Resource-Policy", "same-origin")
}

// notFound is the bare 404 every unknown or unauthorized path gets: Go's own
// (none of the site's headers, so it looks like any Go server's), and the
// connection closes after it.
func notFound(w http.ResponseWriter) {
	w.Header().Set("Connection", "close")
	http.Error(w, "404 page not found", http.StatusNotFound)
}

// WriteJSON answers with v as JSON.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	data, err := json.Marshal(v)
	if err != nil {
		http.Error(w, "encoding error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	w.Write(data)
}

// WriteError answers with {"error": message}.
func WriteError(w http.ResponseWriter, status int, message string) {
	WriteJSON(w, status, map[string]string{"error": message})
}

// decodeBody reads a JSON body (capped by ServeHTTP). An empty body leaves v
// as it is.
func decodeBody(r *http.Request, v any) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

func (s *Server) audit(admin, action, detail, ip string) {
	s.log.Audit(modlog.AuditEntry{Admin: admin, Action: action, Detail: detail, Source: modlog.SourceSite, IP: ip})
}

//
// The listener and the HTTP server
//

// NewHTTPServer wraps the site in an http.Server with timeouts and header
// limits for a page nobody but moderators should use. A signed-out client
// never holds a connection idle: every signed-out response closes it, and a
// connection that sends nothing is dropped after ReadHeaderTimeout. Only a
// signed-in moderator's connection stays open between requests, for at most
// IdleTimeout.
func NewHTTPServer(h http.Handler) *http.Server {
	return &http.Server{
		Handler:           h,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       30 * time.Second,
		MaxHeaderBytes:    16 << 10,
		ErrorLog:          slog.NewLogLogger(slog.Default().Handler(), slog.LevelDebug),
	}
}

// How many connections the site's listener holds: at most maxConns at once,
// maxConnsPerIP from one address, maxConnsPerLoopback from loopback (every
// SSH tunnel user arrives from it). More wait in the backlog.
const (
	maxConnsPerIP       = 8
	maxConnsPerLoopback = 32
)

// Listen opens the site's TCP listener. admit (nil: everyone; pass
// Server.Admits) is asked about each connection's address as soon as it is
// accepted, before any HTTP is read; a refused connection, like one over its
// address's share, is reset at once, as a closed port would be. So an
// address outside adminAllowedIPs, or banned, can't hold the site's
// connections.
func Listen(addr string, admit func(ip string) bool) (net.Listener, error) {
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	return &limitListener{Listener: l, admit: admit, sem: make(chan struct{}, maxConns),
		perIP: map[string]int{}, done: make(chan struct{})}, nil
}

type limitListener struct {
	net.Listener
	admit     func(ip string) bool
	sem       chan struct{}
	done      chan struct{}
	closeOnce sync.Once

	mu    sync.Mutex
	perIP map[string]int
}

func (l *limitListener) Accept() (net.Conn, error) {
	for {
		select {
		case l.sem <- struct{}{}:
		case <-l.done:
			return nil, net.ErrClosed
		}
		c, err := l.Listener.Accept()
		if err != nil {
			<-l.sem
			return nil, err
		}
		ip := connIP(c)
		if (l.admit != nil && !l.admit(ip)) || !l.take(ip) {
			reset(c)
			<-l.sem
			continue
		}
		return &limitConn{Conn: c, release: sync.OnceFunc(func() {
			l.give(ip)
			<-l.sem
		})}, nil
	}
}

// take counts one more connection from ip; false when it has its share.
func (l *limitListener) take(ip string) bool {
	limit := maxConnsPerIP
	if moderation.IsLoopback(ip) {
		limit = maxConnsPerLoopback
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.perIP[ip] >= limit {
		return false
	}
	l.perIP[ip]++
	return true
}

func (l *limitListener) give(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.perIP[ip]--; l.perIP[ip] <= 0 {
		delete(l.perIP, ip)
	}
}

func connIP(c net.Conn) string {
	host, _, err := net.SplitHostPort(c.RemoteAddr().String())
	if err != nil {
		return c.RemoteAddr().String()
	}
	return moderation.CanonicalIP(host)
}

// reset closes c with a TCP reset, having sent nothing.
func reset(c net.Conn) {
	if tcp, ok := c.(*net.TCPConn); ok {
		tcp.SetLinger(0)
	}
	c.Close()
}

func (l *limitListener) Close() error {
	l.closeOnce.Do(func() { close(l.done) })
	return l.Listener.Close()
}

type limitConn struct {
	net.Conn
	release func()
}

func (c *limitConn) Close() error {
	err := c.Conn.Close()
	c.release()
	return err
}
