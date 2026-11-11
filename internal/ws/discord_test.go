package ws

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jay-day/rmgk-lobby/internal/discord"
	"github.com/jay-day/rmgk-lobby/internal/protocol"
)

const (
	testRedirect = "http://127.0.0.1:53135/callback"
	testVerifier = "verifier-0123456789-0123456789-0123456789-0123"
)

// fakeDiscord answers any code "code-<id>-<name>" as Discord user <id>.
func fakeDiscord(t *testing.T) *discord.Client {
	t.Helper()
	codes := map[string]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth2/token":
			r.ParseForm()
			code := r.Form.Get("code")
			if code == "" || code == "bad" || r.Form.Get("code_verifier") != testVerifier {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			codes["tok-"+code] = code
			json.NewEncoder(w).Encode(map[string]string{"access_token": "tok-" + code, "token_type": "Bearer"})
		case "/users/@me":
			code := codes[r.Header.Get("Authorization")[len("Bearer "):]]
			json.NewEncoder(w).Encode(map[string]string{"id": "id-" + code, "username": "d-" + code, "global_name": "D " + code})
		}
	}))
	t.Cleanup(srv.Close)
	return discord.New(discord.Config{ClientID: "app", ClientSecret: "shh", RedirectURIs: []string{testRedirect}, APIBase: srv.URL})
}

func discordHello(f *rankedFixture, ip, code string) helloAuthResult {
	c := &Client{hub: f.hub, ip: ip}
	return f.hub.authenticateHello(c, &protocol.HelloRequest{
		Auth: &protocol.HelloAuth{Mode: "discord", Code: code, CodeVerifier: testVerifier, RedirectURI: testRedirect}})
}

func linkHello(f *rankedFixture, ip, ticket, username, password string) helloAuthResult {
	c := &Client{hub: f.hub, ip: ip}
	return f.hub.authenticateHello(c, &protocol.HelloRequest{Username: username,
		Auth: &protocol.HelloAuth{Mode: "discord_link", Ticket: ticket, Password: password}})
}

func TestDiscordSignInNeedsALinkedAccount(t *testing.T) {
	f := newRankedFixture(t)
	f.hub.EnableDiscord(fakeDiscord(t))
	f.connect("ace", true) // password123

	// an unlinked Discord account gets a ticket, not an account
	res := discordHello(f, "203.0.113.5", "alpha")
	if res.reason != protocol.ReasonDiscordUnlinked || res.ticket == "" || res.account != nil || res.discordName != "d-alpha" {
		t.Fatalf("unlinked = %+v", res)
	}
	// a wrong password doesn't link, but the ticket survives for a retry
	if bad := linkHello(f, "203.0.113.5", res.ticket, "ace", "wrong-password"); bad.reason != protocol.ReasonAuthFailed {
		t.Fatalf("wrong password = %+v", bad)
	}
	// a ticket only works from the IP that got it
	if other := linkHello(f, "198.51.100.7", res.ticket, "ace", "password123"); other.reason != protocol.ReasonTicketExpired {
		t.Fatalf("ticket from another IP = %+v", other)
	}
	// the right password links and logs in
	linked := linkHello(f, "203.0.113.5", res.ticket, "ace", "password123")
	if linked.reason != "" || linked.account == nil || linked.account.Username != "ace" || linked.token == "" {
		t.Fatalf("link = %+v", linked)
	}
	// which uses the ticket up
	if reuse := linkHello(f, "203.0.113.5", res.ticket, "ace", "password123"); reuse.reason != protocol.ReasonTicketExpired {
		t.Fatalf("reused ticket = %+v", reuse)
	}
	// from now on Discord alone signs in
	again := discordHello(f, "198.51.100.7", "alpha")
	if again.reason != "" || again.account == nil || again.account.Username != "ace" || again.token == "" {
		t.Fatalf("discord sign-in = %+v", again)
	}
	// the password still works too
	pw := f.hub.authenticateHello(&Client{hub: f.hub, ip: "198.51.100.7"}, &protocol.HelloRequest{Username: "ace",
		Auth: &protocol.HelloAuth{Mode: "login", Password: "password123"}})
	if pw.reason != "" || pw.account == nil {
		t.Fatalf("password login = %+v", pw)
	}
}

func TestDiscordRegisterLinks(t *testing.T) {
	f := newRankedFixture(t)
	f.hub.EnableDiscord(fakeDiscord(t))
	f.connect("ace", true)
	register := func(ticket, username, password string) helloAuthResult {
		return f.hub.authenticateHello(&Client{hub: f.hub, ip: "203.0.113.5"}, &protocol.HelloRequest{Username: username,
			Auth: &protocol.HelloAuth{Mode: "discord_register", Ticket: ticket, Password: password}})
	}
	res := discordHello(f, "203.0.113.5", "gamma")
	// a taken name or a short password leaves the ticket usable
	if taken := register(res.ticket, "ace", "password123"); taken.reason != protocol.ReasonUsernameRegistered {
		t.Fatalf("taken name = %+v", taken)
	}
	if weak := register(res.ticket, "newbie", "short"); weak.reason != protocol.ReasonWeakPassword {
		t.Fatalf("weak password = %+v", weak)
	}
	created := register(res.ticket, "newbie", "password123")
	if created.reason != "" || created.account == nil || created.account.Username != "newbie" || created.token == "" {
		t.Fatalf("register = %+v", created)
	}
	// the new account is linked: Discord alone signs in
	again := discordHello(f, "198.51.100.7", "gamma")
	if again.reason != "" || again.account == nil || again.account.Username != "newbie" {
		t.Fatalf("discord sign-in = %+v", again)
	}
	if reuse := register(res.ticket, "other", "password123"); reuse.reason != protocol.ReasonTicketExpired {
		t.Fatalf("reused ticket = %+v", reuse)
	}
}

func TestDiscordLinkRules(t *testing.T) {
	f := newRankedFixture(t)
	f.hub.EnableDiscord(fakeDiscord(t))
	ace := f.connect("ace", true)
	f.connect("bee", true)
	res := discordHello(f, "203.0.113.5", "alpha")
	if linkHello(f, "203.0.113.5", res.ticket, "ace", "password123").reason != "" {
		t.Fatal("link failed")
	}
	// the same Discord account can't be linked to a second account
	res = discordHello(f, "203.0.113.5", "beta")
	if linkHello(f, "203.0.113.5", res.ticket, "bee", "password123").reason != "" {
		t.Fatal("linking bee to beta failed")
	}
	f.hub.handleAccountLinkDiscord(ace, envelope(t, protocol.DiscordCodeRequest{Code: "beta", CodeVerifier: testVerifier, RedirectURI: testRedirect}))
	if fails := drain(ace, protocol.TypeAccountDiscordFail); len(fails) != 1 {
		t.Fatal("linking a Discord account that belongs to someone else didn't fail")
	}
	// a disabled account can't sign in through Discord
	if err := f.store.SetDisabled(ace.user.AccountID, true); err != nil {
		t.Fatal(err)
	}
	if res := discordHello(f, "203.0.113.5", "alpha"); res.reason != protocol.ReasonAuthFailed {
		t.Fatalf("disabled account = %+v", res)
	}
	// a rejected code fails without a ticket
	if res := discordHello(f, "203.0.113.5", "bad"); res.reason != protocol.ReasonDiscordFailed || res.ticket != "" {
		t.Fatalf("bad code = %+v", res)
	}
}

func TestDiscordLinkFromTheLobby(t *testing.T) {
	f := newRankedFixture(t)
	f.hub.EnableDiscord(fakeDiscord(t))
	ace := f.connect("ace", true)
	if info := f.hub.accountInfo(ace.user); info.Discord != nil {
		t.Fatalf("discord before linking = %+v", info.Discord)
	}
	f.hub.handleAccountLinkDiscord(ace, envelope(t, protocol.DiscordCodeRequest{Code: "alpha", CodeVerifier: testVerifier, RedirectURI: testRedirect}))
	msgs := drain(ace, protocol.TypeAccountDiscord)
	var got protocol.DiscordAccountInfo
	if len(msgs) == 1 {
		json.Unmarshal(msgs[0].Data, &got)
	}
	if !got.Linked || got.Username != "d-alpha" {
		t.Fatalf("link reply = %+v", got)
	}
	if info := f.hub.accountInfo(ace.user); info.Discord == nil || info.Discord.Username != "d-alpha" {
		t.Fatalf("account info after linking = %+v", info.Discord)
	}
	if res := discordHello(f, "203.0.113.5", "alpha"); res.account == nil || res.account.Username != "ace" {
		t.Fatalf("discord sign-in after linking = %+v", res)
	}
	f.hub.handleAccountUnlinkDiscord(ace, protocol.Envelope{})
	if msgs := drain(ace, protocol.TypeAccountDiscord); len(msgs) != 1 {
		t.Fatal("no unlink reply")
	}
	if res := discordHello(f, "203.0.113.5", "alpha"); res.reason != protocol.ReasonDiscordUnlinked {
		t.Fatalf("discord sign-in after unlinking = %+v", res)
	}
	// guests can't link
	guest := f.connect("guest", false)
	f.hub.handleAccountLinkDiscord(guest, envelope(t, protocol.DiscordCodeRequest{Code: "gamma", CodeVerifier: testVerifier, RedirectURI: testRedirect}))
	if fails := drain(guest, protocol.TypeAccountDiscordFail); len(fails) != 1 {
		t.Fatal("a guest linked Discord")
	}
}

func TestDiscordSignInOff(t *testing.T) {
	f := newRankedFixture(t)
	if res := discordHello(f, "203.0.113.5", "alpha"); res.reason != protocol.ReasonDiscordUnavailable {
		t.Fatalf("without Discord configured = %+v", res)
	}
}
