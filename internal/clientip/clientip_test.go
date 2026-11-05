package clientip

import (
	"net/http/httptest"
	"testing"
)

func TestIP(t *testing.T) {
	r, err := Parse("127.0.0.1, 10.0.0.0/8, ::1")
	if err != nil || r.Proxies() != 3 {
		t.Fatalf("Parse = %v, %d", err, r.Proxies())
	}
	cases := []struct {
		remote, xff, want string
	}{
		{"203.0.113.5:4000", "", "203.0.113.5"},
		{"203.0.113.5:4000", "1.2.3.4", "203.0.113.5"},               // untrusted peer can't claim an address
		{"127.0.0.1:4000", "198.51.100.7", "198.51.100.7"},           // through the local proxy
		{"127.0.0.1:4000", "6.6.6.6, 198.51.100.7", "198.51.100.7"},  // forged left-most hop ignored
		{"127.0.0.1:4000", "198.51.100.7, 10.1.2.3", "198.51.100.7"}, // two proxies
		{"127.0.0.1:4000", "", "127.0.0.1"},
		{"127.0.0.1:4000", "nonsense", "127.0.0.1"},
		{"[::1]:4000", "2001:db8::9", "2001:db8::9"},
	}
	for _, c := range cases {
		req := httptest.NewRequest("GET", "/", nil)
		req.RemoteAddr = c.remote
		if c.xff != "" {
			req.Header.Set("X-Forwarded-For", c.xff)
		}
		if got := r.IP(req); got != c.want {
			t.Errorf("remote %s xff %q = %s, want %s", c.remote, c.xff, got, c.want)
		}
	}

	none, _ := Parse("")
	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "127.0.0.1:1"
	req.Header.Set("X-Forwarded-For", "198.51.100.7")
	if got := none.IP(req); got != "127.0.0.1" {
		t.Errorf("no trusted proxies = %s", got)
	}
	if _, err := Parse("not-an-ip"); err == nil {
		t.Error("bad proxy accepted")
	}
}
