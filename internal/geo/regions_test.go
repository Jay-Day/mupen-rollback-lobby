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

func TestEstimatedRTTAndContinents(t *testing.T) {
	if got := EstimatedRTT(RegionUSEast, RegionEUWest); got != 90 {
		t.Fatalf("us-east to eu-west = %d", got)
	}
	if EstimatedRTT(RegionOceania, RegionEUEast) != EstimatedRTT(RegionEUEast, RegionOceania) {
		t.Fatal("estimates aren't symmetric")
	}
	if got := EstimatedRTT("", RegionUSEast); got != 0 {
		t.Fatalf("unknown region = %d", got)
	}
	for region, want := range map[string]string{
		RegionUSWest: "na", RegionSA: "sa", RegionEUEast: "eu", RegionAsiaSE: "asia", RegionOceania: "oc", "": "",
	} {
		if got := ContinentOf(region); got != want {
			t.Fatalf("ContinentOf(%q) = %q, want %q", region, got, want)
		}
		if want != "" && ContinentName(want) == "" {
			t.Fatalf("continent %q has no name", want)
		}
	}
}
