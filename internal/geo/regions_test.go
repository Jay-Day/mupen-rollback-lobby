package geo

import "testing"

func TestRegionFromIP(t *testing.T) {
	cases := []struct {
		name string
		ip   string
		want string
	}{
		{"empty", "", RegionUnknown},
		{"garbage", "not-an-ip", RegionUnknown},
		{"loopback", "127.0.0.1", RegionUnknown},
		{"rfc1918", "192.168.1.1", RegionUnknown},
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

func TestRegionForCountry(t *testing.T) {
	cases := []struct {
		name string
		cc   string
		want string
	}{
		// No estimate beats a wrong one: an unlisted country is unknown, not us-east.
		{"unlisted-south-africa", "ZA", RegionUnknown},
		{"unlisted-egypt", "EG", RegionUnknown},
		{"unknown-marker", "ZZ", RegionUnknown},
		{"none", "", RegionUnknown},
		// The Caribbean and Central America route through the US East Coast.
		{"caribbean-jamaica", "JM", RegionUSEast},
		{"caribbean-dominican-republic", "DO", RegionUSEast},
		{"caribbean-puerto-rico", "PR", RegionUSEast},
		{"central-america-panama", "PA", RegionUSEast},
		{"central-america-guatemala", "GT", RegionUSEast},
		// Unchanged buckets.
		{"united-states", "US", RegionUSCentral},
		{"mexico", "MX", RegionUSCentral},
		{"brazil", "BR", RegionSA},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := regionForCountry(tc.cc); got != tc.want {
				t.Errorf("regionForCountry(%q) = %q, want %q", tc.cc, got, tc.want)
			}
		})
	}
}

func TestCountryCodeDropsTheUnknownMarker(t *testing.T) {
	cases := map[string]string{"ZZ": "", "zz": "", "": "", "USA": "", "us": "US", "JM": "JM"}
	for raw, want := range cases {
		if got := countryCode(raw); got != want {
			t.Errorf("countryCode(%q) = %q, want %q", raw, got, want)
		}
	}
}

func TestRefineNorthAmerica(t *testing.T) {
	cases := []struct {
		name   string
		region string
		tzSec  int
		want   string
	}{
		{"eastern", RegionUSCentral, -5 * 3600, RegionUSEast},
		{"atlantic-canada", RegionUSCentral, -4 * 3600, RegionUSEast},
		{"central", RegionUSCentral, -6 * 3600, RegionUSCentral},
		{"mountain-goes-west", RegionUSCentral, -7 * 3600, RegionUSWest},
		{"pacific", RegionUSCentral, -8 * 3600, RegionUSWest},
		{"hawaii", RegionUSCentral, -10 * 3600, RegionUSWest},
		{"absent-offset", RegionUSCentral, 0, RegionUSCentral},
		{"non-na-untouched", RegionEUWest, -5 * 3600, RegionEUWest},
		{"caribbean-untouched", RegionUSEast, -4 * 3600, RegionUSEast},
		{"unknown-stays-unknown", RegionUnknown, -5 * 3600, RegionUnknown},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := RefineNorthAmerica(tc.region, tc.tzSec)
			if got != tc.want {
				t.Errorf("RefineNorthAmerica(%q, %d) = %q, want %q", tc.region, tc.tzSec, got, tc.want)
			}
		})
	}
}
