package discord

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const testVerifier = "verifier-0123456789-0123456789-0123456789-0123"

func TestIdentify(t *testing.T) {
	var gotForm, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth2/token":
			r.ParseForm()
			gotForm = r.Form.Encode()
			user, pass, _ := r.BasicAuth()
			gotAuth = user + ":" + pass
			if r.Form.Get("code") != "good" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			w.Write([]byte(`{"access_token":"tok","token_type":"Bearer","scope":"identify"}`))
		case "/users/@me":
			if r.Header.Get("Authorization") != "Bearer tok" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			w.Write([]byte(`{"id":"123","username":"ace","global_name":"Ace!"}`))
		}
	}))
	defer srv.Close()
	c := New(Config{ClientID: "app", ClientSecret: "shh", RedirectURIs: []string{"http://127.0.0.1:53135/callback"}, APIBase: srv.URL})

	user, err := c.Identify(context.Background(), "good", testVerifier, "http://127.0.0.1:53135/callback")
	if err != nil || user.ID != "123" || user.DisplayName() != "Ace!" {
		t.Fatalf("Identify = %+v, %v", user, err)
	}
	if gotAuth != "app:shh" || !strings.Contains(gotForm, "code_verifier="+testVerifier) || !strings.Contains(gotForm, "grant_type=authorization_code") {
		t.Fatalf("token request: auth %q form %q", gotAuth, gotForm)
	}
	if _, err := c.Identify(context.Background(), "bad", testVerifier, "http://127.0.0.1:53135/callback"); err == nil {
		t.Fatal("a rejected code was accepted")
	}
	if _, err := c.Identify(context.Background(), "good", testVerifier, "http://127.0.0.1:9999/callback"); err != ErrBadRedirect {
		t.Fatalf("unregistered redirect: %v", err)
	}
	if _, err := c.Identify(context.Background(), "good", "short", "http://127.0.0.1:53135/callback"); err == nil {
		t.Fatal("a short verifier was accepted")
	}
}

func TestConfigFromEnv(t *testing.T) {
	t.Setenv("RMGK_DISCORD_CLIENT_ID", "")
	t.Setenv("RMGK_DISCORD_CLIENT_SECRET", "")
	if cfg, err := ConfigFromEnv(); cfg != nil || err != nil {
		t.Fatalf("unset = %+v, %v", cfg, err)
	}
	t.Setenv("RMGK_DISCORD_CLIENT_ID", "app")
	if _, err := ConfigFromEnv(); err == nil {
		t.Fatal("a partial configuration was accepted")
	}
	t.Setenv("RMGK_DISCORD_CLIENT_SECRET", "shh")
	t.Setenv("RMGK_DISCORD_REDIRECT_URIS", "https://example.com/callback")
	if _, err := ConfigFromEnv(); err == nil {
		t.Fatal("a non-loopback redirect was accepted")
	}
	t.Setenv("RMGK_DISCORD_REDIRECT_URIS", "http://127.0.0.1:53135/callback, http://127.0.0.1:53136/callback")
	cfg, err := ConfigFromEnv()
	if err != nil || len(cfg.RedirectURIs) != 2 {
		t.Fatalf("config = %+v, %v", cfg, err)
	}
}
