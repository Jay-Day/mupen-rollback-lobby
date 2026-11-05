package web

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jay-day/rmgk-lobby/internal/accounts"
	"github.com/jay-day/rmgk-lobby/internal/moderation"
	"github.com/jay-day/rmgk-lobby/internal/protocol"
	"github.com/jay-day/rmgk-lobby/internal/ranked"
)

// fakeAdmins limits failures per IP like moderation.Manager.
type fakeAdmins struct{ fails map[string]int }

func (f *fakeAdmins) AuthenticateAdminFrom(ip, password string) (string, bool, bool) {
	if f.fails[ip] >= moderation.AdminFailLimit {
		return "", false, true
	}
	if password != "modpass" {
		f.fails[ip]++
		return "", false, false
	}
	return "mod", true, false
}

type adminFixture struct {
	t        *testing.T
	srv      *httptest.Server
	store    *accounts.Store
	client   *http.Client
	resolved []int64
	dropped  []int64
}

func newAdminFixture(t *testing.T) *adminFixture {
	t.Helper()
	store, err := accounts.Open(filepath.Join(t.TempDir(), "admin.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	cfg, err := ranked.Parse([]byte(testConfig))
	if err != nil {
		t.Fatal(err)
	}
	f := &adminFixture{t: t, store: store}
	site := New(store, cfg, t.TempDir())
	site.EnableAdmin(&fakeAdmins{fails: map[string]int{}}, AdminHooks{
		SetResolved: func(set accounts.SetSummary, _ []accounts.PlayerOutcome, _ int64, _ string) {
			f.resolved = append(f.resolved, set.ID)
		},
		AccountDisabled: func(id int64) { f.dropped = append(f.dropped, id) },
	})
	mux := http.NewServeMux()
	site.Register(mux)
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	jar, _ := cookiejar.New(nil)
	f.client = &http.Client{Jar: jar}
	return f
}

// call sends an admin request (header included unless noHeader) and decodes the reply.
func (f *adminFixture) call(method, path string, body any, out any, noHeader ...bool) int {
	f.t.Helper()
	var reader *bytes.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		reader = bytes.NewReader(raw)
	} else {
		reader = bytes.NewReader(nil)
	}
	req, _ := http.NewRequest(method, f.srv.URL+path, reader)
	req.Header.Set("Content-Type", "application/json")
	if len(noHeader) == 0 {
		req.Header.Set(adminHeader, "1")
	}
	res, err := f.client.Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	defer res.Body.Close()
	if out != nil {
		json.NewDecoder(res.Body).Decode(out)
	}
	return res.StatusCode
}

func TestAdminSignIn(t *testing.T) {
	f := newAdminFixture(t)
	if code := f.call("GET", "/api/admin/session", nil, nil); code != 401 {
		t.Fatalf("signed out session = %d", code)
	}
	if code := f.call("POST", "/api/admin/login", map[string]string{"password": "modpass"}, nil, true); code != 403 {
		t.Fatalf("login without the admin header = %d", code)
	}
	if code := f.call("POST", "/api/admin/login", map[string]string{"password": "nope"}, nil); code != 401 {
		t.Fatalf("wrong password = %d", code)
	}
	var who struct {
		Name     string
		Disputes int
	}
	if code := f.call("POST", "/api/admin/login", map[string]string{"password": "modpass"}, &who); code != 200 || who.Name != "mod" {
		t.Fatalf("login = %d %+v", code, who)
	}
	if code := f.call("GET", "/api/admin/session", nil, &who); code != 200 || who.Name != "mod" || who.Disputes != 0 {
		t.Fatalf("session = %d", code)
	}
	// changes need the header even with the cookie (a cross-site form can't set it)
	if code := f.call("POST", "/api/admin/logout", nil, nil, true); code != 403 {
		t.Fatalf("logout without header = %d", code)
	}
	f.call("POST", "/api/admin/logout", nil, nil)
	if code := f.call("GET", "/api/admin/session", nil, nil); code != 401 {
		t.Fatalf("after logout = %d", code)
	}

	for i := 0; i < moderation.AdminFailLimit; i++ {
		f.call("POST", "/api/admin/login", map[string]string{"password": "guess"}, nil)
	}
	if code := f.call("POST", "/api/admin/login", map[string]string{"password": "modpass"}, nil); code != 429 {
		t.Fatalf("after %d failures = %d", moderation.AdminFailLimit, code)
	}
}

func TestAdminResolveDispute(t *testing.T) {
	f := newAdminFixture(t)
	a, _ := f.store.Register("Ace", "password123")
	b, _ := f.store.Register("Bee", "password123")
	reports, _ := json.Marshal(map[string]protocol.MatchResultRequest{
		"Ace": {WinnerSlot: 1, Score: []int{2, 1}, Integrity: "ok"},
		"Bee": {WinnerSlot: 2, Score: []int{1, 2}, Integrity: "ok"},
	})
	setID, _, err := f.store.RecordSetWithID(accounts.SetRecord{Ladder: "remix", MatchID: 1, BootID: "b",
		Status: accounts.SetDisputed, Reason: "reports_disagree", ResultJSON: string(reports),
		Players: []accounts.SetPlayer{{AccountID: a.ID, Slot: 1}, {AccountID: b.ID, Slot: 2}}})
	if err != nil {
		t.Fatal(err)
	}

	if code := f.call("GET", "/api/admin/disputes", nil, nil); code != 401 {
		t.Fatalf("disputes signed out = %d", code)
	}
	f.call("POST", "/api/admin/login", map[string]string{"password": "modpass"}, nil)

	var list struct {
		Sets []struct {
			ID      int64
			Reason  string
			Reports []struct {
				Reporter   string
				WinnerSlot int
			}
		}
	}
	f.call("GET", "/api/admin/disputes", nil, &list)
	if len(list.Sets) != 1 || len(list.Sets[0].Reports) != 2 || list.Sets[0].Reports[0].Reporter != "Ace" ||
		list.Sets[0].Reports[1].WinnerSlot != 2 {
		t.Fatalf("disputes = %+v", list)
	}
	var session struct{ Disputes int }
	if f.call("GET", "/api/admin/session", nil, &session); session.Disputes != 1 {
		t.Fatalf("session dispute count = %d", session.Disputes)
	}

	path := "/api/admin/sets/" + itoa(setID) + "/resolve"
	if code := f.call("POST", path, map[string]any{"winner": "ghost"}, nil); code != 400 {
		t.Fatalf("winner who didn't play = %d", code)
	}
	if code := f.call("POST", path, map[string]any{"winner": "bee", "void": true}, nil); code != 400 {
		t.Fatalf("winner and void = %d", code)
	}
	var set struct {
		Status string
		Winner string
	}
	if code := f.call("POST", path, map[string]any{"winner": "bee", "note": "replay checked"}, &set); code != 200 ||
		set.Status != "rated" || set.Winner != "Bee" {
		t.Fatalf("resolve = %d %+v", code, set)
	}
	if len(f.resolved) != 1 || f.resolved[0] != setID {
		t.Fatalf("hook calls = %v", f.resolved)
	}
	if code := f.call("POST", path, map[string]any{"void": true}, nil); code != 409 {
		t.Fatalf("second resolution = %d", code)
	}

	var log struct {
		Entries []struct{ Admin, Action, Detail string }
	}
	f.call("GET", "/api/admin/log", nil, &log)
	if len(log.Entries) != 1 || log.Entries[0].Admin != "mod" ||
		!strings.Contains(log.Entries[0].Detail, "Bee wins: replay checked") {
		t.Fatalf("log = %+v", log)
	}
}

func TestAdminAccounts(t *testing.T) {
	f := newAdminFixture(t)
	acct, _ := f.store.Register("Ace", "password123")
	f.store.Register("Bee", "password123")
	f.store.CreateSession(acct.ID)
	f.call("POST", "/api/admin/login", map[string]string{"password": "modpass"}, nil)

	var found struct{ Accounts []struct{ Username string } }
	f.call("GET", "/api/admin/accounts?q=ac", nil, &found)
	if len(found.Accounts) != 1 || found.Accounts[0].Username != "Ace" {
		t.Fatalf("search = %+v", found)
	}

	var reset struct {
		Username string
		Code     string
	}
	if code := f.call("POST", "/api/admin/accounts/ace/reset-code", nil, &reset); code != 200 || len(reset.Code) != 14 {
		t.Fatalf("reset code = %d %+v", code, reset)
	}
	if _, err := f.store.ResetPassword("Ace", reset.Code, "brandnew123"); err != nil {
		t.Fatalf("issued code doesn't work: %v", err)
	}

	var status struct {
		Disabled bool
		Sessions int
	}
	if code := f.call("POST", "/api/admin/accounts/Ace/disabled", map[string]any{"disabled": true, "note": "cheating"}, &status); code != 200 ||
		!status.Disabled || status.Sessions != 0 {
		t.Fatalf("disable = %d %+v", code, status)
	}
	if len(f.dropped) != 1 || f.dropped[0] != acct.ID {
		t.Fatalf("disconnect hook = %v", f.dropped)
	}
	if _, err := f.store.Login("Ace", "brandnew123"); !errors.Is(err, accounts.ErrAccountDisabled) {
		t.Fatalf("disabled account logs in: %v", err)
	}
	if code := f.call("POST", "/api/admin/accounts/Ace/reset-code", nil, nil); code != 409 {
		t.Fatalf("reset code for a disabled account = %d", code)
	}
	var detail struct {
		Account struct {
			Username string
			Disabled bool
		}
	}
	if code := f.call("GET", "/api/admin/accounts/Ace", nil, &detail); code != 200 || !detail.Account.Disabled {
		t.Fatalf("detail = %d %+v", code, detail)
	}
	f.call("POST", "/api/admin/accounts/Ace/disabled", map[string]any{"disabled": false}, &status)
	if _, err := f.store.Login("Ace", "brandnew123"); err != nil {
		t.Fatalf("re-enabled login: %v", err)
	}
	if code := f.call("GET", "/api/admin/accounts/ghost", nil, nil); code != 404 {
		t.Fatalf("unknown account = %d", code)
	}
}

func itoa(n int64) string {
	raw, _ := json.Marshal(n)
	return string(raw)
}

func TestAdminDiscordUnlink(t *testing.T) {
	f := newAdminFixture(t)
	acct, _ := f.store.Register("Ace", "password123")
	if err := f.store.LinkDiscord(acct.ID, "123", "ace_d", "Ace D"); err != nil {
		t.Fatal(err)
	}
	// moderators only
	if code := f.call("POST", "/api/admin/accounts/ace/discord/unlink", nil, nil); code != 401 {
		t.Fatalf("unlink without signing in = %d", code)
	}
	f.call("POST", "/api/admin/login", map[string]string{"password": "modpass"}, nil)
	var detail struct {
		Discord *struct{ ID, Username string }
	}
	f.call("GET", "/api/admin/accounts/ace", nil, &detail)
	if detail.Discord == nil || detail.Discord.ID != "123" || detail.Discord.Username != "ace_d" {
		t.Fatalf("account detail discord = %+v", detail.Discord)
	}
	if code := f.call("POST", "/api/admin/accounts/ace/discord/unlink", nil, nil); code != 200 {
		t.Fatalf("unlink = %d", code)
	}
	if _, linked, _ := f.store.DiscordLinkOf(acct.ID); linked {
		t.Fatal("still linked")
	}
	if code := f.call("POST", "/api/admin/accounts/ace/discord/unlink", nil, nil); code != 409 {
		t.Fatalf("second unlink = %d", code)
	}
	var log struct {
		Entries []struct{ Action, Detail string }
	}
	f.call("GET", "/api/admin/log", nil, &log)
	if len(log.Entries) == 0 || log.Entries[0].Action != "discord-unlink" {
		t.Fatalf("log = %+v", log.Entries)
	}
}
