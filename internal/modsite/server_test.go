package modsite

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/jay-day/rmgk-lobby/internal/config"
	"github.com/jay-day/rmgk-lobby/internal/moderation"
	"github.com/jay-day/rmgk-lobby/internal/modlog"
	"github.com/jay-day/rmgk-lobby/internal/protocol"
	"github.com/jay-day/rmgk-lobby/internal/ratelimit"
	"github.com/jay-day/rmgk-lobby/internal/state"
	"github.com/jay-day/rmgk-lobby/internal/ws"
)

const (
	modPassword = "correct horse battery"
	siteHost    = "192.0.2.9:8081"
	modIP       = "198.51.100.10" // where the moderator signs in from
)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// testSite is the moderator site over a real lobby (players connect over
// WebSocket, each naming its IP) with the moderators' log and lobby.json.
type testSite struct {
	t          *testing.T
	s          *Server
	mod        *moderation.Manager
	hub        *ws.Hub
	log        *modlog.Logger
	clock      *fakeClock
	cfgPath    string
	settings   *config.Store
	adminsPath string
	wsURL      string
}

type siteOpts struct {
	noLog bool
	// limits turns on the lobby's request limiter (and the Rate limits page)
	limits *ratelimit.ActionLimits
}

func newTestSite(t *testing.T, opts ...siteOpts) *testSite {
	t.Helper()
	var o siteOpts
	if len(opts) > 0 {
		o = opts[0]
	}
	dir := t.TempDir()
	adminsPath := filepath.Join(dir, "admins.json")
	writeAdmins(t, adminsPath, moderation.Admin{Name: "Wren", Salt: "s1", Hash: moderation.HashPassword("s1", modPassword)})
	admins, err := moderation.LoadAdmins(adminsPath)
	if err != nil {
		t.Fatal(err)
	}
	mod := moderation.NewManager(moderation.NewFilter([]string{"badword"}), admins, filepath.Join(dir, "bans.json"))
	t.Cleanup(mod.FlushBans) // the ban writer is done before the directory goes
	cfgPath := filepath.Join(dir, "lobby.json")
	settings, err := config.Open(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	var log *modlog.Logger
	if !o.noLog {
		if log, err = modlog.Open(filepath.Join(dir, "modlog.db")); err != nil {
			t.Fatal(err)
		}
	}
	hub := ws.NewHub(state.New(), "", protocol.ICEConfiguration{}, mod, settings)
	hub.SetModLog(log)
	guard := moderation.NewLoginGuard(mod)
	hub.SetLoginGuard(guard)
	var rates RateLimits
	if o.limits != nil {
		limits := *o.limits
		l := ratelimit.NewActionLimiter()
		l.SetLimits(func() ratelimit.ActionLimits { return limits })
		hub.SetActionLimiter(l)
		rates = hub
	}
	ctx, cancel := context.WithCancel(context.Background())
	go hub.Run(ctx)
	lobby := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hub.ServeWS(w, r, r.Header.Get("X-Test-IP"))
	}))
	t.Cleanup(func() {
		lobby.Close()
		cancel()
		log.Close()
	})
	s := New(Config{Lobby: hub, Mod: mod, Guard: guard, Log: log, RateLimits: rates,
		AllowIP: func(ip string) bool { return settings.Get().AdminIPAllowed(ip) }})
	clock := &fakeClock{t: time.Now()}
	s.sessions.now = clock.now
	return &testSite{t: t, s: s, mod: mod, hub: hub, log: log, clock: clock, cfgPath: cfgPath, settings: settings,
		adminsPath: adminsPath, wsURL: "ws" + strings.TrimPrefix(lobby.URL, "http")}
}

func writeAdmins(t *testing.T, path string, admins ...moderation.Admin) {
	t.Helper()
	raw, _ := json.Marshal(admins)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func (ts *testSite) setConfig(data string) {
	ts.t.Helper()
	if err := os.WriteFile(ts.cfgPath, []byte(data), 0o600); err != nil {
		ts.t.Fatal(err)
	}
	ts.settings.Reload()
}

// req builds a request as the site's own page sends it (the header, JSON),
// from ip; edit changes it before it's served.
type req struct {
	method, path, ip string
	body             any
	raw              []byte // a body as is (instead of body)
	cookie           string
	edit             func(*http.Request)
}

func (ts *testSite) serve(q req) *httptest.ResponseRecorder {
	ts.t.Helper()
	var body io.Reader
	if q.raw != nil {
		body = bytes.NewReader(q.raw)
	} else if q.body != nil {
		b, _ := json.Marshal(q.body)
		body = bytes.NewReader(b)
	}
	r := httptest.NewRequest(q.method, q.path, body)
	r.Host = siteHost
	r.RemoteAddr = q.ip + ":40000"
	if strings.Contains(q.ip, ":") {
		r.RemoteAddr = "[" + q.ip + "]:40000"
	}
	r.Header.Set(AdminHeader, "1")
	if q.method != http.MethodGet && q.method != http.MethodHead {
		r.Header.Set("Content-Type", "application/json")
	}
	if q.cookie != "" {
		r.AddCookie(&http.Cookie{Name: cookieName, Value: q.cookie})
	}
	if q.edit != nil {
		q.edit(r)
	}
	w := httptest.NewRecorder()
	ts.s.ServeHTTP(w, r)
	return w
}

// login signs in from ip and returns the session cookie's value.
func (ts *testSite) login(ip string) string {
	ts.t.Helper()
	w := ts.serve(req{method: "POST", path: "/api/admin/login", ip: ip, body: map[string]string{"password": modPassword}})
	if w.Code != 200 {
		ts.t.Fatalf("sign in from %s = %d %s", ip, w.Code, w.Body)
	}
	for _, c := range w.Result().Cookies() {
		if c.Name == cookieName {
			return c.Value
		}
	}
	ts.t.Fatal("no session cookie")
	return ""
}

func (ts *testSite) api(method, path, cookie string, body any, out any) int {
	ts.t.Helper()
	w := ts.serve(req{method: method, path: path, ip: modIP, body: body, cookie: cookie})
	if out != nil && strings.HasPrefix(w.Header().Get("Content-Type"), "application/json") {
		if err := json.Unmarshal(w.Body.Bytes(), out); err != nil {
			ts.t.Fatalf("%s %s: %v: %s", method, path, err, w.Body)
		}
	}
	return w.Code
}

// isBare404 is Go's own 404: its text and headers only (none of the site's,
// so it looks like any Go server's), closing the connection.
func isBare404(w *httptest.ResponseRecorder) bool {
	h := w.Header()
	return w.Code == 404 && w.Body.String() == "404 page not found\n" &&
		h.Get("Content-Type") == "text/plain; charset=utf-8" && h.Get("Connection") == "close" &&
		h.Get("Content-Security-Policy") == "" && h.Get("X-Frame-Options") == "" && h.Get("Cache-Control") == "" &&
		h.Get("X-Robots-Tag") == "" && h.Get("Referrer-Policy") == ""
}

func checkSecurityHeaders(t *testing.T, what string, h http.Header) {
	t.Helper()
	for k, want := range map[string]string{
		"Cache-Control":          "no-store",
		"X-Content-Type-Options": "nosniff",
		"X-Frame-Options":        "DENY",
		"Referrer-Policy":        "no-referrer",
		"X-Robots-Tag":           "noindex, nofollow",
	} {
		if got := h.Get(k); got != want {
			t.Errorf("%s: %s = %q, want %q", what, k, got, want)
		}
	}
	csp := h.Get("Content-Security-Policy")
	for _, part := range []string{"default-src 'none'", "frame-ancestors 'none'", "base-uri 'none'"} {
		if !strings.Contains(csp, part) {
			t.Errorf("%s: CSP %q lacks %q", what, csp, part)
		}
	}
	if strings.Contains(csp, "unsafe-inline") || strings.Contains(csp, "unsafe-eval") || strings.Contains(csp, "*") {
		t.Errorf("%s: CSP %q is loose", what, csp)
	}
}

func TestSignedOutSeesOnlyTheSignInPage(t *testing.T) {
	ts := newTestSite(t)
	page := ts.serve(req{method: "GET", path: "/", ip: "203.0.113.50"})
	if page.Code != 200 || !strings.Contains(page.Body.String(), "<title>Sign in</title>") {
		t.Fatalf("GET / = %d", page.Code)
	}
	checkSecurityHeaders(t, "sign-in page", page.Header())
	// the sign-in page tells nothing about the server or its moderators
	body := page.Body.String()
	for _, leak := range []string{"Wren", "Moderator", "moderator", "RMG-K", "/api/admin/live", "version", "online"} {
		if strings.Contains(body, leak) {
			t.Errorf("sign-in page mentions %q", leak)
		}
	}
	if head := ts.serve(req{method: "HEAD", path: "/", ip: "203.0.113.50"}); head.Code != 200 || head.Body.Len() != 0 {
		t.Fatalf("HEAD / = %d, %d bytes", head.Code, head.Body.Len())
	}
	for _, path := range []string{"/api/admin/session", "/api/admin/live", "/api/admin/chat", "/api/admin/audit",
		"/api/admin/sanctions", "/api/admin/trace?name=a", "/api/admin/games/1", "/api/admin/", "/api/admin/login",
		"/api/", "/favicon.ico", "/index.html", "/ws", "/health", "/robots.txt", "/admin"} {
		w := ts.serve(req{method: "GET", path: path, ip: "203.0.113.50"})
		if !isBare404(w) {
			t.Errorf("GET %s signed out = %d %q", path, w.Code, w.Body)
		}
	}
	for _, path := range []string{"/api/admin/logout", "/api/admin/users/1/ban", "/api/admin/rooms/1/close",
		"/api/admin/sanctions/ban", "/"} {
		w := ts.serve(req{method: "POST", path: path, ip: "203.0.113.50", body: map[string]string{}})
		if !isBare404(w) {
			t.Errorf("POST %s signed out = %d %q", path, w.Code, w.Body)
		}
	}
	// a made-up cookie is no session
	if w := ts.serve(req{method: "GET", path: "/api/admin/session", ip: "203.0.113.50", cookie: "forged"}); !isBare404(w) {
		t.Fatalf("forged cookie = %d", w.Code)
	}
}

func TestSignInAndPageCSP(t *testing.T) {
	ts := newTestSite(t)
	w := ts.serve(req{method: "POST", path: "/api/admin/login", ip: modIP, body: map[string]string{"password": modPassword}})
	if w.Code != 200 {
		t.Fatalf("sign in = %d %s", w.Code, w.Body)
	}
	checkSecurityHeaders(t, "login", w.Header())
	var cookie *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == cookieName {
			cookie = c
		}
	}
	if cookie == nil || !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode || cookie.Path != "/" ||
		len(cookie.Value) < 43 {
		t.Fatalf("cookie = %+v", cookie)
	}
	// the server keeps only the token's hash
	if ts.s.sessions.byHash[cookie.Value] != nil || ts.s.sessions.byHash[hashToken(cookie.Value)] == nil {
		t.Fatal("session not stored by hash")
	}

	app := ts.serve(req{method: "GET", path: "/", ip: modIP, cookie: cookie.Value})
	if app.Code != 200 || !strings.Contains(app.Body.String(), "<title>Moderators</title>") {
		t.Fatalf("signed-in GET / = %d", app.Code)
	}
	checkSecurityHeaders(t, "app page", app.Header())
	// each page's CSP names exactly its inline script and style by hash
	for _, p := range []*httptest.ResponseRecorder{app, ts.serve(req{method: "GET", path: "/", ip: modIP})} {
		html := p.Body.String()
		csp := p.Header().Get("Content-Security-Policy")
		for _, tag := range []string{"script", "style"} {
			m := regexp.MustCompile(`(?s)<` + tag + `>(.*?)</` + tag + `>`).FindStringSubmatch(html)
			if m == nil {
				t.Fatalf("no <%s>", tag)
			}
			sum := sha256.Sum256([]byte(m[1]))
			want := tag + "-src 'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'"
			if !strings.Contains(csp, want) {
				t.Fatalf("CSP %q lacks %q", csp, want)
			}
		}
		if strings.Contains(html, "\r") {
			t.Fatal("page served with CR: browsers would hash it differently")
		}
		for _, sink := range []string{"innerHTML", "outerHTML", "insertAdjacentHTML", "document.write", "eval(", "new Function",
			"http://", "https://", "//cdn", "@import", "url("} {
			if strings.Contains(html, sink) {
				t.Errorf("page contains %q", sink)
			}
		}
	}
	var session struct {
		Name    string
		Online  int
		Logging bool
	}
	if code := ts.api("GET", "/api/admin/session", cookie.Value, nil, &session); code != 200 || session.Name != "Wren" ||
		!session.Logging {
		t.Fatalf("session = %d %+v", code, session)
	}
	// sign out ends it
	if code := ts.api("POST", "/api/admin/logout", cookie.Value, map[string]any{}, nil); code != 200 {
		t.Fatalf("logout = %d", code)
	}
	if w := ts.serve(req{method: "GET", path: "/api/admin/session", ip: modIP, cookie: cookie.Value}); !isBare404(w) {
		t.Fatalf("after logout = %d", w.Code)
	}
	audit, _, _ := ts.log.AuditLog(context.Background(), 0, 10)
	if len(audit) != 2 || audit[0].Action != "sign-out" || audit[1].Action != "sign-in" || audit[1].IP != modIP ||
		audit[1].Source != modlog.SourceSite {
		t.Fatalf("audit = %+v", audit)
	}
}

func TestSignInFailuresAreGeneric(t *testing.T) {
	ts := newTestSite(t)
	cases := map[string]req{
		"wrong password": {body: map[string]string{"password": "nope"}},
		"no password":    {body: map[string]string{}},
		"not JSON":       {raw: []byte("password=" + modPassword)},
		"extra field":    {body: map[string]string{"password": modPassword, "name": "Wren"}},
		"no header": {body: map[string]string{"password": modPassword},
			edit: func(r *http.Request) { r.Header.Del(AdminHeader) }},
		"form post": {raw: []byte("password=" + modPassword),
			edit: func(r *http.Request) { r.Header.Set("Content-Type", "application/x-www-form-urlencoded") }},
		"other site": {body: map[string]string{"password": modPassword},
			edit: func(r *http.Request) { r.Header.Set("Origin", "http://evil.test") }},
		"null origin": {body: map[string]string{"password": modPassword},
			edit: func(r *http.Request) { r.Header.Set("Origin", "null") }},
		"too big": {raw: []byte(`{"password":"` + strings.Repeat("x", maxBody) + `"}`)},
	}
	for name, c := range cases {
		c.method, c.path, c.ip = "POST", "/api/admin/login", "192.0.2."+string(rune('1'+len(name)%9))
		w := ts.serve(c)
		var reply struct{ Error string }
		json.Unmarshal(w.Body.Bytes(), &reply)
		if w.Code != 401 || reply.Error != loginFailed {
			t.Errorf("%s: %d %q", name, w.Code, w.Body)
		}
		if len(w.Result().Cookies()) != 0 {
			t.Errorf("%s: set a cookie", name)
		}
	}
	// only password checks count toward the ban: none of the above banned anyone
	bans, _ := ts.mod.ListSanctions()
	if len(bans) != 0 {
		t.Fatalf("bans = %+v", bans)
	}
	// the right origin is fine
	w := ts.serve(req{method: "POST", path: "/api/admin/login", ip: modIP, body: map[string]string{"password": modPassword},
		edit: func(r *http.Request) { r.Header.Set("Origin", "http://"+siteHost) }})
	if w.Code != 200 {
		t.Fatalf("same-origin sign in = %d", w.Code)
	}
}

func TestFiveFailedSignInsBanTheIP(t *testing.T) {
	ts := newTestSite(t)
	const attacker = "203.0.113.66"
	// a lobby connection from the same IP is closed by the ban
	player := ts.connect("Lurker", attacker)
	for i := 1; i <= moderation.MaxLoginFailures; i++ {
		w := ts.serve(req{method: "POST", path: "/api/admin/login", ip: attacker, body: map[string]string{"password": "guess"}})
		if w.Code != 401 {
			t.Fatalf("try %d = %d", i, w.Code)
		}
	}
	s, banned := ts.mod.IsBanned(attacker)
	if !banned || s.By != moderation.AutoBanBy || s.Reason != moderation.AutoBanReason || !s.Until.IsZero() {
		t.Fatalf("ban = %+v %v", s, banned)
	}
	if _, ok := player.awaitNotice("too many failed moderator logins"); !ok {
		t.Fatal("the lobby connection wasn't told")
	}
	player.awaitClosed()
	// the banned IP gets nothing from the site now, not even the right password's answer
	for _, q := range []req{
		{method: "GET", path: "/"},
		{method: "POST", path: "/api/admin/login", body: map[string]string{"password": modPassword}},
	} {
		q.ip = attacker
		if w := ts.serve(q); !isBare404(w) {
			t.Fatalf("%s %s from a banned IP = %d", q.method, q.path, w.Code)
		}
	}
	// it shows on the Sanctions page, and comes off there
	cookie := ts.login(modIP)
	var list struct {
		Bans []struct{ IP, By, Reason string }
	}
	ts.api("GET", "/api/admin/sanctions", cookie, nil, &list)
	if len(list.Bans) != 1 || list.Bans[0].IP != attacker || list.Bans[0].By != "auto" {
		t.Fatalf("sanctions = %+v", list)
	}
	if code := ts.api("POST", "/api/admin/sanctions/unban", cookie, map[string]string{"ip": attacker}, nil); code != 200 {
		t.Fatalf("unban = %d", code)
	}
	if w := ts.serve(req{method: "GET", path: "/", ip: attacker}); w.Code != 200 {
		t.Fatalf("after unban = %d", w.Code)
	}
	// the audit trail has the automatic ban
	audit, _, _ := ts.log.AuditLog(context.Background(), 0, 20)
	found := false
	for _, e := range audit {
		found = found || (e.Admin == "auto" && e.Action == "ban" && e.Source == modlog.SourceAuto &&
			strings.Contains(e.Detail, attacker) && strings.Contains(e.Detail, "moderator site"))
	}
	if !found {
		t.Fatalf("audit = %+v", audit)
	}
}

func TestSignInFailuresFromLoopbackLockOut(t *testing.T) {
	ts := newTestSite(t)
	for i := 0; i < moderation.MaxLoginFailures; i++ {
		ts.serve(req{method: "POST", path: "/api/admin/login", ip: "127.0.0.1", body: map[string]string{"password": "guess"}})
	}
	if _, banned := ts.mod.IsBanned("127.0.0.1"); banned {
		t.Fatal("loopback banned")
	}
	// locked out: even the right password fails, with the same answer
	w := ts.serve(req{method: "POST", path: "/api/admin/login", ip: "127.0.0.1", body: map[string]string{"password": modPassword}})
	if w.Code != 401 || !strings.Contains(w.Body.String(), loginFailed) {
		t.Fatalf("locked-out sign in = %d %s", w.Code, w.Body)
	}
	// the page still loads, and other addresses aren't affected
	if w := ts.serve(req{method: "GET", path: "/", ip: "127.0.0.1"}); w.Code != 200 {
		t.Fatalf("GET / from locked-out loopback = %d", w.Code)
	}
	ts.login("::1") // a different loopback address keeps its own count
}

func TestAllowedIPs(t *testing.T) {
	ts := newTestSite(t)
	cookie := ts.login(modIP)
	ts.setConfig(`{"adminAllowedIPs": ["192.0.2.0/24", "2001:db8::5"]}`)
	for ip, want := range map[string]bool{
		"192.0.2.77": true, "2001:db8::5": true, "127.0.0.1": true, "::1": true,
		modIP: false, "2001:db8::6": false, "203.0.113.1": false,
	} {
		for _, q := range []req{
			{method: "GET", path: "/"},
			{method: "POST", path: "/api/admin/login", body: map[string]string{"password": "x"}},
			{method: "GET", path: "/nothing"},
		} {
			q.ip = ip
			w := ts.serve(q)
			if !want && !isBare404(w) {
				t.Errorf("%s %s from %s = %d, want a bare 404", q.method, q.path, ip, w.Code)
			}
			if want && q.path == "/" && w.Code != 200 {
				t.Errorf("GET / from %s = %d", ip, w.Code)
			}
		}
	}
	// a session from an address taken off the list stops working at once
	if w := ts.serve(req{method: "GET", path: "/api/admin/session", ip: modIP, cookie: cookie}); !isBare404(w) {
		t.Fatalf("session from a disallowed IP = %d", w.Code)
	}
	ts.setConfig(`{}`)
	if w := ts.serve(req{method: "GET", path: "/", ip: modIP}); w.Code != 200 {
		t.Fatalf("after clearing the list = %d", w.Code)
	}
}

func TestOnlyIPAndLocalhostHosts(t *testing.T) {
	ts := newTestSite(t)
	cookie := ts.login(modIP)
	for host, want := range map[string]bool{
		"192.0.2.9:8081": true, "192.0.2.9": true, "[2001:db8::9]:8081": true, "127.0.0.1:8081": true,
		"localhost:8081": true, "LOCALHOST": true,
		"evil.test:8081": false, "evil.test": false, "127.0.0.1.nip.io:8081": false, "": false, "[evil]:8081": false,
	} {
		for _, q := range []req{
			{method: "GET", path: "/"},
			{method: "GET", path: "/api/admin/session", cookie: cookie},
			{method: "POST", path: "/api/admin/login", body: map[string]string{"password": modPassword}},
		} {
			q.ip = modIP
			h := host
			q.edit = func(r *http.Request) { r.Host = h }
			w := ts.serve(q)
			if !want && !isBare404(w) {
				t.Errorf("%s %s with Host %q = %d, want a bare 404", q.method, q.path, host, w.Code)
			}
			if want && q.method == "GET" && w.Code != 200 {
				t.Errorf("%s %s with Host %q = %d", q.method, q.path, host, w.Code)
			}
		}
	}
}

func TestSessionIsBoundToItsIP(t *testing.T) {
	ts := newTestSite(t)
	cookie := ts.login(modIP)
	if w := ts.serve(req{method: "GET", path: "/api/admin/session", ip: "203.0.113.99", cookie: cookie}); !isBare404(w) {
		t.Fatalf("cookie from another IP = %d", w.Code)
	}
	// and the stolen cookie's session is over, for its owner too
	if w := ts.serve(req{method: "GET", path: "/api/admin/session", ip: modIP, cookie: cookie}); !isBare404(w) {
		t.Fatalf("session after use from another IP = %d", w.Code)
	}
}

func TestSessionLifetimeAndIdle(t *testing.T) {
	ts := newTestSite(t)
	cookie := ts.login(modIP)
	get := func(background bool) int {
		return ts.serve(req{method: "GET", path: "/api/admin/session", ip: modIP, cookie: cookie, edit: func(r *http.Request) {
			if background {
				r.Header.Set(backgroundHeader, "1")
			}
		}}).Code
	}
	// the pages' own refreshes don't keep an idle session alive
	ts.clock.advance(40 * time.Minute)
	if get(true) != 200 {
		t.Fatal("refresh at 40 minutes")
	}
	ts.clock.advance(21 * time.Minute)
	if get(true) != 404 {
		t.Fatal("idle session lived past 60 minutes on refreshes alone")
	}
	// the moderator's own requests do, up to the 12 hour limit
	cookie = ts.login(modIP)
	for elapsed := time.Duration(0); elapsed < sessionLifetime-time.Hour; elapsed += 50 * time.Minute {
		ts.clock.advance(50 * time.Minute)
		if get(false) != 200 {
			t.Fatalf("active session ended after %v", elapsed+50*time.Minute)
		}
	}
	ts.clock.advance(2 * time.Hour)
	if get(false) != 404 {
		t.Fatal("session outlived 12 hours")
	}
}

func TestSessionEndsWhenTheModeratorIsRemoved(t *testing.T) {
	ts := newTestSite(t)
	cookie := ts.login(modIP)
	reload := func(admins ...moderation.Admin) {
		writeAdmins(t, ts.adminsPath, admins...)
		a, err := moderation.LoadAdmins(ts.adminsPath)
		if err != nil {
			t.Fatal(err)
		}
		ts.mod.SetAdmins(a)
	}
	// another moderator added: still signed in
	reload(moderation.Admin{Name: "Wren", Salt: "s1", Hash: moderation.HashPassword("s1", modPassword)},
		moderation.Admin{Name: "Juniper", Salt: "s2", Hash: moderation.HashPassword("s2", "other")})
	if code := ts.api("GET", "/api/admin/session", cookie, nil, nil); code != 200 {
		t.Fatalf("after adding a moderator = %d", code)
	}
	// Wren's password changed: signed out
	reload(moderation.Admin{Name: "Wren", Salt: "s3", Hash: moderation.HashPassword("s3", "new password")})
	if w := ts.serve(req{method: "GET", path: "/api/admin/session", ip: modIP, cookie: cookie}); !isBare404(w) {
		t.Fatalf("after a password change = %d", w.Code)
	}
	reload(moderation.Admin{Name: "Wren", Salt: "s1", Hash: moderation.HashPassword("s1", modPassword)})
	cookie = ts.login(modIP)
	reload() // nobody left
	if w := ts.serve(req{method: "GET", path: "/", ip: modIP, cookie: cookie}); !strings.Contains(w.Body.String(), "<title>Sign in") {
		t.Fatal("removed moderator still sees the moderator pages")
	}
}

func TestSessionCap(t *testing.T) {
	ts := newTestSite(t)
	first := ts.login(modIP)
	for i := 0; i < maxSessions+5; i++ {
		ts.clock.advance(time.Second)
		ts.login(modIP)
	}
	if n := ts.s.sessions.count(); n != maxSessions {
		t.Fatalf("%d sessions, cap %d", n, maxSessions)
	}
	if w := ts.serve(req{method: "GET", path: "/api/admin/session", ip: modIP, cookie: first}); !isBare404(w) {
		t.Fatal("the least recently used session survived the cap")
	}
}

func TestAPIRefusesCrossSiteRequests(t *testing.T) {
	ts := newTestSite(t)
	cookie := ts.login(modIP)
	cases := map[string]func(*http.Request){
		"no header":      func(r *http.Request) { r.Header.Del(AdminHeader) },
		"header not 1":   func(r *http.Request) { r.Header.Set(AdminHeader, "true") },
		"form post":      func(r *http.Request) { r.Header.Set("Content-Type", "application/x-www-form-urlencoded") },
		"text post":      func(r *http.Request) { r.Header.Set("Content-Type", "text/plain") },
		"no type":        func(r *http.Request) { r.Header.Del("Content-Type") },
		"other origin":   func(r *http.Request) { r.Header.Set("Origin", "http://evil.test") },
		"other port":     func(r *http.Request) { r.Header.Set("Origin", "http://192.0.2.9:8080") },
		"null origin":    func(r *http.Request) { r.Header.Set("Origin", "null") },
		"other referrer": func(r *http.Request) { r.Header.Set("Referer", "http://evil.test/page") },
	}
	for name, edit := range cases {
		w := ts.serve(req{method: "POST", path: "/api/admin/sanctions/ban", ip: modIP, cookie: cookie,
			body: map[string]string{"ip": "192.0.2.200"}, edit: edit})
		if w.Code != 403 {
			t.Errorf("%s: %d %s", name, w.Code, w.Body)
		}
		checkSecurityHeaders(t, name, w.Header())
	}
	if bans, _ := ts.mod.ListSanctions(); len(bans) != 0 {
		t.Fatalf("a refused request banned: %+v", bans)
	}
	// GETs need the header too, so a top-level navigation shows nothing
	if w := ts.serve(req{method: "GET", path: "/api/admin/live", ip: modIP, cookie: cookie,
		edit: func(r *http.Request) { r.Header.Del(AdminHeader) }}); w.Code != 403 {
		t.Fatalf("GET without the header = %d", w.Code)
	}
	// the site's own page: same origin, JSON, the header
	if w := ts.serve(req{method: "POST", path: "/api/admin/sanctions/ban", ip: modIP, cookie: cookie,
		body: map[string]string{"ip": "192.0.2.200"},
		edit: func(r *http.Request) { r.Header.Set("Origin", "http://"+siteHost) }}); w.Code != 200 {
		t.Fatalf("same-origin ban = %d %s", w.Code, w.Body)
	}
}

func TestQueriesAreChecked(t *testing.T) {
	ts := newTestSite(t)
	cookie := ts.login(modIP)
	for _, path := range []string{
		"/api/admin/chat?limit=501", "/api/admin/chat?limit=-1", "/api/admin/chat?before=x", "/api/admin/chat?room=1e9",
		"/api/admin/chat?channel=pm", "/api/admin/chat?blocked=2", "/api/admin/chat?q=" + strings.Repeat("a", 201),
		"/api/admin/chat?name=" + strings.Repeat("a", 65), "/api/admin/connections?before=99999999999999999999",
		"/api/admin/games?match=-5", "/api/admin/audit?limit=1000", "/api/admin/trace", "/api/admin/trace?name=a&ip=192.0.2.1",
		"/api/admin/trace?ip=not-an-ip",
	} {
		if code := ts.api("GET", path, cookie, nil, nil); code != 400 {
			t.Errorf("GET %s = %d", path, code)
		}
	}
	for path, body := range map[string]any{
		"/api/admin/sanctions/ban":    map[string]string{"ip": "not-an-ip"},
		"/api/admin/sanctions/mute":   map[string]string{"ip": "127.0.0.1"},
		"/api/admin/sanctions/unban":  map[string]string{},
		"/api/admin/users/1/timeout":  map[string]string{"duration": "soon"},
		"/api/admin/rooms/1/close":    map[string]string{"reason": strings.Repeat("é", 201)},
		"/api/admin/sanctions/ban?":   map[string]any{"ip": 5},
		"/api/admin/users/1/kick?x=1": map[string]string{"unknown": "field"},
	} {
		if code := ts.api("POST", path, cookie, body, nil); code != 400 {
			t.Errorf("POST %s %v = %d", path, body, code)
		}
	}
	if code := ts.api("POST", "/api/admin/users/1/nuke", cookie, map[string]string{}, nil); code != 404 {
		t.Errorf("unknown action = %d", code)
	}
	if code := ts.api("GET", "/api/admin/nothing", cookie, nil, nil); code != 404 {
		t.Errorf("unknown path signed in = %d", code)
	}
}

func TestExtensionEndpoint(t *testing.T) {
	ts := newTestSite(t)
	// any path; the real Rate limits endpoint has /api/admin/ratelimits
	ts.s.Handle("GET /api/admin/extension-example", func(w http.ResponseWriter, r *http.Request, admin string) {
		WriteJSON(w, http.StatusOK, map[string]string{"by": admin})
	})
	if w := ts.serve(req{method: "GET", path: "/api/admin/extension-example", ip: modIP}); !isBare404(w) {
		t.Fatalf("signed out = %d", w.Code)
	}
	var reply struct{ By string }
	if code := ts.api("GET", "/api/admin/extension-example", ts.login(modIP), nil, &reply); code != 200 || reply.By != "Wren" {
		t.Fatalf("signed in = %d %+v", code, reply)
	}
}

func TestLimitListener(t *testing.T) {
	var refuse atomic.Bool
	l, err := Listen("127.0.0.1:0", func(ip string) bool { return !refuse.Load() })
	if err != nil {
		t.Fatal(err)
	}
	srv := NewHTTPServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) }))
	go srv.Serve(l)
	defer srv.Close()
	plain := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	res, err := plain.Get("http://" + l.Addr().String() + "/")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if srv.ReadHeaderTimeout == 0 || srv.ReadTimeout == 0 || srv.WriteTimeout == 0 || srv.IdleTimeout == 0 ||
		srv.MaxHeaderBytes == 0 {
		t.Fatalf("server limits = %+v", srv)
	}

	// a refused address is reset before any HTTP: no answer at all
	refuse.Store(true)
	conn, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	conn.SetDeadline(time.Now().Add(2 * time.Second))
	fmt.Fprintf(conn, "GET / HTTP/1.1\r\nHost: 127.0.0.1\r\n\r\n")
	if n, err := conn.Read(make([]byte, 64)); n != 0 || err == nil || errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("refused address read %d bytes, err %v", n, err)
	}
	conn.Close()
	refuse.Store(false)

	// one address holds at most its share; the rest are reset
	if got := holdConns(t, l.Addr().String(), maxConnsPerLoopback+5); got != maxConnsPerLoopback {
		t.Fatalf("loopback held %d connections, cap %d", got, maxConnsPerLoopback)
	}
}

// holdConns opens n connections, keeps them open, and counts how many the
// listener still holds a moment later (the others were reset).
func holdConns(t *testing.T, addr string, n int) int {
	t.Helper()
	var conns []net.Conn
	defer func() {
		for _, c := range conns {
			c.Close()
		}
	}()
	for i := 0; i < n; i++ {
		c, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		conns = append(conns, c)
	}
	time.Sleep(200 * time.Millisecond) // the listener has taken or reset each by now
	alive := 0
	deadline := time.Now().Add(500 * time.Millisecond) // well inside the server's header timeout
	for _, c := range conns {
		// half a request, as a slow client would send: a held connection
		// waits for the rest, a reset one fails
		if _, err := c.Write([]byte("GET / HTTP/1.1\r\n")); err != nil {
			continue
		}
		c.SetReadDeadline(deadline)
		if _, err := c.Read(make([]byte, 1)); errors.Is(err, os.ErrDeadlineExceeded) {
			alive++ // still open, nothing to read
		}
	}
	return alive
}

//
// Lobby connections for the API tests
//

type player struct {
	t    *testing.T
	conn *websocket.Conn
	id   uint64
}

func (ts *testSite) connect(name, ip string) *player {
	ts.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, ts.wsURL, &websocket.DialOptions{HTTPHeader: http.Header{"X-Test-IP": {ip}}})
	if err != nil {
		ts.t.Fatalf("dial %s: %v", name, err)
	}
	ts.t.Cleanup(func() { conn.CloseNow() })
	p := &player{t: ts.t, conn: conn}
	before := ts.hub.OnlineCount()
	p.send(protocol.TypeHello, protocol.HelloRequest{Username: name, ClientVersion: "vdev-1"})
	env, ok := p.await(protocol.TypeHelloOK)
	if !ok {
		ts.t.Fatalf("hello %s refused", name)
	}
	var hello protocol.HelloOKResponse
	json.Unmarshal(env.Data, &hello)
	p.id = hello.UserID
	ts.waitOnline(before + 1)
	return p
}

func (ts *testSite) waitOnline(n int) {
	ts.t.Helper()
	for deadline := time.Now().Add(3 * time.Second); ts.hub.OnlineCount() != n; {
		if time.Now().After(deadline) {
			ts.t.Fatalf("online = %d, want %d", ts.hub.OnlineCount(), n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (p *player) send(msgType string, data any) {
	p.t.Helper()
	payload, _ := json.Marshal(data)
	raw, _ := json.Marshal(protocol.Envelope{Type: msgType, Data: payload})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := p.conn.Write(ctx, websocket.MessageText, raw); err != nil {
		p.t.Fatalf("write: %v", err)
	}
}

func (p *player) await(msgType string) (protocol.Envelope, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		_, raw, err := p.conn.Read(ctx)
		if err != nil {
			return protocol.Envelope{}, false
		}
		var env protocol.Envelope
		if json.Unmarshal(raw, &env) == nil && env.Type == msgType {
			return env, true
		}
	}
}

func (p *player) awaitNotice(text string) (string, bool) {
	for {
		env, ok := p.await(protocol.TypeModNotice)
		if !ok {
			return "", false
		}
		var n protocol.ModNoticePayload
		json.Unmarshal(env.Data, &n)
		if strings.Contains(n.Text, text) {
			return n.Text, true
		}
	}
}

func (p *player) awaitClosed() {
	p.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		if _, _, err := p.conn.Read(ctx); err != nil {
			if ctx.Err() != nil {
				p.t.Fatal("still connected")
			}
			return
		}
	}
}

func (p *player) sync() {
	p.t.Helper()
	p.send(protocol.TypeHeartbeat, protocol.HeartbeatRequest{})
	if _, ok := p.await(protocol.TypeHeartbeatAck); !ok {
		p.t.Fatal("closed before HEARTBEAT_ACK")
	}
}
