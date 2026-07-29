package geo

import "testing"

func TestRegionFromIP(t *testing.T) {
	cases := []struct {
		name string
		ip   string
		want string
	}{
		{"empty", "", DefaultRegion},
		{"garbage", "not-an-ip", DefaultRegion},
		{"loopback", "127.0.0.1", DefaultRegion},
		{"rfc1918", "192.168.1.1", DefaultRegion},
		// Well-known public IPs whose country we can rely on remaining
		// stable across iploc db updates (Google / Cloudflare anycast).
		{"google-dns-us", "8.8.8.8", RegionUSCentral},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := RegionFromIP(tc.ip)
			if got != tc.want {
				t.Errorf("RegionFromIP(%q) = %q, want %q", tc.ip, got, tc.want)
			}
		})
	}
}
