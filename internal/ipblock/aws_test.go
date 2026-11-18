package ipblock

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

const sampleRanges = `{
  "syncToken": "1",
  "prefixes": [
    {"ip_prefix": "18.216.0.0/14", "region": "us-east-2", "service": "AMAZON"},
    {"ip_prefix": "18.216.0.0/14", "region": "us-east-2", "service": "EC2"},
    {"ip_prefix": "3.144.0.0/13", "region": "us-east-2", "service": "EC2"}
  ],
  "ipv6_prefixes": [
    {"ipv6_prefix": "2600:1f00::/24", "region": "us-east-2", "service": "AMAZON"}
  ]
}`

func TestParseAWSRangesDedupes(t *testing.T) {
	list, err := parseAWSRanges([]byte(sampleRanges))
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 3 {
		t.Fatalf("got %d prefixes, want 3 (dup removed): %v", len(list), list)
	}
}

func TestParseAWSRangesRejectsEmpty(t *testing.T) {
	if _, err := parseAWSRanges([]byte(`{"prefixes":[],"ipv6_prefixes":[]}`)); err == nil {
		t.Fatal("empty document should be an error")
	}
	if _, err := parseAWSRanges([]byte(`not json`)); err == nil {
		t.Fatal("bad JSON should be an error")
	}
}

func TestContains(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(sampleRanges))
	}))
	defer srv.Close()

	a := NewAWS()
	a.url = srv.URL
	if a.Contains("18.216.142.107") {
		t.Fatal("blocked before the first refresh; must fail open")
	}
	if err := a.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	for ip, want := range map[string]bool{
		"18.216.142.107":        true,  // leak-repro wave 2
		"3.144.172.175":         true,  // leak-repro wave 3
		"::ffff:18.216.142.107": true,  // v4-mapped spelling
		"2600:1f00::1":          true,  // AWS IPv6
		"85.145.193.196":        false, // Dutch ISP, not AWS
		"127.0.0.1":             false,
		"not-an-ip":             false,
	} {
		if got := a.Contains(ip); got != want {
			t.Errorf("Contains(%q) = %v, want %v", ip, got, want)
		}
	}
}

func TestRefreshFailureKeepsPreviousList(t *testing.T) {
	fail := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(sampleRanges))
	}))
	defer srv.Close()

	a := NewAWS()
	a.url = srv.URL
	if err := a.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	fail = true
	if err := a.Refresh(context.Background()); err == nil {
		t.Fatal("expected refresh error")
	}
	if !a.Contains("18.216.142.107") {
		t.Fatal("failed refresh dropped the previous list")
	}
}
