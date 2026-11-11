package ws

import (
	"net/http/httptest"
	"testing"
)

func TestCrossSiteBrowser(t *testing.T) {
	cases := []struct {
		origin string
		cross  bool
	}{
		{"", false},                          // native client
		{"https://lobby.example", false},     // the lobby's own pages
		{"https://LOBBY.example", false},     // hosts compare without case
		{"https://evil.example", true},       // another site
		{"https://lobby.example.evil", true}, // look-alike
		{"null", true},                       // sandboxed iframe, file://
		{"://bad", true},                     // unparsable
	}
	for _, c := range cases {
		r := httptest.NewRequest("GET", "http://lobby.example/ws", nil)
		if c.origin != "" {
			r.Header.Set("Origin", c.origin)
		}
		if got := CrossSiteBrowser(r); got != c.cross {
			t.Errorf("Origin %q: cross = %v, want %v", c.origin, got, c.cross)
		}
	}
}
