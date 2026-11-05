package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jay-day/rmgk-lobby/internal/accounts"
	"github.com/jay-day/rmgk-lobby/internal/ranked"
)

func postResetRequest(t *testing.T, url, contentType, body string) (*http.Response, map[string]any) {
	t.Helper()
	res, err := http.Post(url+"/api/auth/password-reset", contentType, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var out map[string]any
	json.NewDecoder(res.Body).Decode(&out)
	return res, out
}

func TestPasswordResetEmailEndpoints(t *testing.T) {
	// without email both endpoints are 404s (RMG-K hides "Email me a code")
	srv, _ := newTestServer(t)
	if code := getJSON(t, srv.URL+"/api/auth/email", nil); code != http.StatusNotFound {
		t.Fatalf("email config without email = %d", code)
	}
	if res, _ := postResetRequest(t, srv.URL, "application/json", `{"username":"ace"}`); res.StatusCode != http.StatusNotFound {
		t.Fatalf("reset request without email = %d", res.StatusCode)
	}

	store, err := accounts.Open(filepath.Join(t.TempDir(), "web.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	cfg, _ := ranked.Parse([]byte(testConfig))
	type call struct{ username, email, ip string }
	var calls []call
	var wait time.Duration
	site := New(store, cfg, "")
	site.EnableEmail(func(username, email, ip string) time.Duration {
		calls = append(calls, call{username, email, ip})
		return wait
	})
	mux := http.NewServeMux()
	site.Register(mux)
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	var config map[string]any
	if code := getJSON(t, srv.URL+"/api/auth/email", &config); code != http.StatusOK ||
		config["passwordReset"] != true || config["codeMinutes"] != float64(60) {
		t.Fatalf("email config = %d %v", code, config)
	}

	// form posts (what another site could make a browser send) are refused
	if res, _ := postResetRequest(t, srv.URL, "text/plain", `{"username":"ace","email":"ace@example.com"}`); res.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("text/plain request = %d", res.StatusCode)
	}
	if res, _ := postResetRequest(t, srv.URL, "application/json", `{"username":" ","email":"ace@example.com"}`); res.StatusCode != http.StatusBadRequest {
		t.Fatalf("empty username = %d", res.StatusCode)
	}
	// the address is required: a name alone can't send reset emails
	if res, _ := postResetRequest(t, srv.URL, "application/json", `{"username":"ace"}`); res.StatusCode != http.StatusBadRequest {
		t.Fatalf("no email = %d", res.StatusCode)
	}
	if len(calls) != 0 {
		t.Fatalf("refused requests reached the hub: %v", calls)
	}

	res, body := postResetRequest(t, srv.URL, "application/json; charset=utf-8", `{"username":"Ace","email":"ace@example.com"}`)
	if res.StatusCode != http.StatusAccepted || body["ok"] != true || res.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("reset request = %d %v", res.StatusCode, body)
	}
	if len(calls) != 1 || calls[0].username != "Ace" || calls[0].email != "ace@example.com" || calls[0].ip != "127.0.0.1" {
		t.Fatalf("hub calls = %v", calls)
	}

	wait = 90 * time.Second
	res, body = postResetRequest(t, srv.URL, "application/json", `{"username":"Ace","email":"ace@example.com"}`)
	if res.StatusCode != http.StatusTooManyRequests || res.Header.Get("Retry-After") != "90" ||
		!strings.Contains(body["error"].(string), "2 minutes") {
		t.Fatalf("rate-limited request = %d %v %q", res.StatusCode, body, res.Header.Get("Retry-After"))
	}
}
