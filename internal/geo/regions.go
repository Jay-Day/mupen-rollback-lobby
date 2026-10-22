// Package geo maps a public IP address to a coarse region code that the
// lobby exposes to other clients for ping estimation, plus the ISO country
// code that drives the client's flag display (Fightcade-style).
//
// Privacy: the underlying lookup uses an embedded country-level database
// (phuslu/iploc). We deliberately do NOT use a city-level database — the
// most precise thing other clients ever see is the country flag; nothing
// below country level is looked up or stored.
package geo

import (
	"net"
	"strings"

	"github.com/phuslu/iploc"
)

// Region codes are the canonical strings the lobby protocol uses. They must
// stay in sync with the client's LobbyRegions matrix.
const (
	RegionUSEast    = "us-east"
	RegionUSCentral = "us-central"
	RegionUSWest    = "us-west"
	RegionSA        = "sa"
	RegionEUWest    = "eu-west"
	RegionEUEast    = "eu-east"
	RegionAsiaEast  = "asia-east"
	RegionAsiaSE    = "asia-se"
	RegionOceania   = "oceania"

	// DefaultRegion is returned when the IP is private, malformed, or maps
	// to a country we haven't mapped. us-east is the most populated bucket
	// for this game's audience.
	DefaultRegion = RegionUSEast
)

// countryToRegion buckets ISO 3166-1 alpha-2 codes into the lobby's regions.
// We can't subdivide the US into east/west/central from country data alone
// (that would need a city-level db), so all US IPs collapse to us-central
// as a midpoint compromise.
var countryToRegion = map[string]string{
	// North America — collapsed to us-central since country-only data can't
	// tell us east vs west coast. Better to be 40ms off either way than
	// 80ms off in one direction.
	"US": RegionUSCentral,
	"CA": RegionUSCentral,
	"MX": RegionUSCentral,

	// South America
	"BR": RegionSA, "AR": RegionSA, "CL": RegionSA, "CO": RegionSA,
	"PE": RegionSA, "VE": RegionSA, "EC": RegionSA, "BO": RegionSA,
	"PY": RegionSA, "UY": RegionSA, "GY": RegionSA, "SR": RegionSA,
	"GF": RegionSA, "FK": RegionSA,

	// Western / Northern / Southern Europe
	"GB": RegionEUWest, "IE": RegionEUWest, "FR": RegionEUWest,
	"BE": RegionEUWest, "NL": RegionEUWest, "LU": RegionEUWest,
	"DE": RegionEUWest, "AT": RegionEUWest, "CH": RegionEUWest,
	"IT": RegionEUWest, "ES": RegionEUWest, "PT": RegionEUWest,
	"DK": RegionEUWest, "NO": RegionEUWest, "SE": RegionEUWest,
	"FI": RegionEUWest, "IS": RegionEUWest, "MT": RegionEUWest,
	"MC": RegionEUWest, "LI": RegionEUWest, "AD": RegionEUWest,
	"SM": RegionEUWest, "VA": RegionEUWest, "GI": RegionEUWest,
	"FO": RegionEUWest, "GL": RegionEUWest,

	// Eastern Europe + Balkans + Turkey
	"PL": RegionEUEast, "CZ": RegionEUEast, "SK": RegionEUEast,
	"HU": RegionEUEast, "RO": RegionEUEast, "BG": RegionEUEast,
	"GR": RegionEUEast, "RU": RegionEUEast, "UA": RegionEUEast,
	"BY": RegionEUEast, "MD": RegionEUEast, "LT": RegionEUEast,
	"LV": RegionEUEast, "EE": RegionEUEast, "RS": RegionEUEast,
	"HR": RegionEUEast, "SI": RegionEUEast, "BA": RegionEUEast,
	"MK": RegionEUEast, "AL": RegionEUEast, "ME": RegionEUEast,
	"XK": RegionEUEast, "TR": RegionEUEast, "CY": RegionEUEast,
	"GE": RegionEUEast, "AM": RegionEUEast, "AZ": RegionEUEast,

	// East Asia
	"JP": RegionAsiaEast, "KR": RegionAsiaEast, "CN": RegionAsiaEast,
	"HK": RegionAsiaEast, "TW": RegionAsiaEast, "MO": RegionAsiaEast,
	"MN": RegionAsiaEast, "KP": RegionAsiaEast,

	// Southeast Asia + Indian subcontinent (closer to SE than East routing-wise)
	"TH": RegionAsiaSE, "VN": RegionAsiaSE, "SG": RegionAsiaSE,
	"MY": RegionAsiaSE, "ID": RegionAsiaSE, "PH": RegionAsiaSE,
	"KH": RegionAsiaSE, "LA": RegionAsiaSE, "MM": RegionAsiaSE,
	"BN": RegionAsiaSE, "TL": RegionAsiaSE, "IN": RegionAsiaSE,
	"PK": RegionAsiaSE, "BD": RegionAsiaSE, "LK": RegionAsiaSE,
	"NP": RegionAsiaSE, "BT": RegionAsiaSE, "MV": RegionAsiaSE,

	// Oceania
	"AU": RegionOceania, "NZ": RegionOceania, "FJ": RegionOceania,
	"PG": RegionOceania, "SB": RegionOceania, "NC": RegionOceania,
	"VU": RegionOceania, "WS": RegionOceania, "TO": RegionOceania,
	"KI": RegionOceania, "TV": RegionOceania, "NR": RegionOceania,
	"PW": RegionOceania, "FM": RegionOceania, "MH": RegionOceania,

	// Africa, Middle East, and anything not above falls through to
	// DefaultRegion in RegionFromIP — keeping the table small.
}

// RegionFromIP resolves a string IP (as captured from the client's WS
// connection) to a region code. Empty/invalid/private IPs map to
// DefaultRegion. The country code is consulted internally but never
// returned — callers only see the bucketed region.
func RegionFromIP(ipStr string) string {
	if ipStr == "" {
		return DefaultRegion
	}
	ip := net.ParseIP(ipStr)
	if ip == nil {
		return DefaultRegion
	}
	if !isPublicUnicast(ip) {
		return DefaultRegion
	}
	raw := iploc.Country(ip)
	if len(raw) != 2 {
		return DefaultRegion
	}
	cc := strings.ToUpper(string(raw))
	if region, ok := countryToRegion[cc]; ok {
		return region
	}
	return DefaultRegion
}

// RefineNorthAmerica splits the country-level North America bucket into
// east/central/west using the client's self-reported standard (non-DST) UTC
// offset — country-level IP data cannot tell coasts apart, but the OS clock
// can: New York -5h, Chicago -6h, Denver/Phoenix -7h, Los Angeles -8h.
// Mountain (-7h) goes west: a mountain player mislabelled "central" makes an
// east-coast viewer expect ~40 ms where reality is ~70; "west" predicts 80.
// Offsets are seconds east of UTC (negative in the Americas); an absent
// offset (0) leaves the bucket unchanged. Only the collapsed us-central
// bucket is ever refined, so other regions are untouched.
func RefineNorthAmerica(region string, tzOffsetSeconds int) string {
	if region != RegionUSCentral || tzOffsetSeconds == 0 {
		return region
	}
	switch {
	case tzOffsetSeconds >= -5*3600:
		return RegionUSEast
	case tzOffsetSeconds >= -6*3600:
		return RegionUSCentral
	default:
		return RegionUSWest
	}
}

// CountryFromIP resolves a string IP to its ISO 3166-1 alpha-2 country code
// (upper-case), or "" when the IP is empty, invalid, private, or unknown.
// Clients render this as a flag next to the player's name.
func CountryFromIP(ipStr string) string {
	if ipStr == "" {
		return ""
	}
	ip := net.ParseIP(ipStr)
	if ip == nil || !isPublicUnicast(ip) {
		return ""
	}
	raw := iploc.Country(ip)
	if len(raw) != 2 {
		return ""
	}
	return strings.ToUpper(string(raw))
}

// isPublicUnicast filters out loopback, link-local, multicast, and RFC1918
// ranges that would never have a useful geo answer anyway.
func isPublicUnicast(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsMulticast() ||
		ip.IsUnspecified() || ip.IsPrivate() {
		return false
	}
	return true
}

// regionOrder indexes rttMatrix; it mirrors the client's LobbyRegions.
var regionOrder = []string{
	RegionUSEast, RegionUSCentral, RegionUSWest, RegionSA, RegionEUWest,
	RegionEUEast, RegionAsiaEast, RegionAsiaSE, RegionOceania,
}

// rttMatrix is the client's rough round-trip estimate (ms) between regions
// (LobbyRegions.hpp kRttMatrix); keep the two in sync.
var rttMatrix = [9][9]int{
	{30, 40, 80, 140, 90, 120, 180, 220, 200},
	{40, 30, 50, 150, 110, 140, 170, 210, 190},
	{80, 50, 30, 170, 140, 170, 140, 180, 160},
	{140, 150, 170, 30, 180, 210, 240, 270, 220},
	{90, 110, 140, 180, 30, 50, 220, 200, 280},
	{120, 140, 170, 210, 50, 30, 200, 180, 300},
	{180, 170, 140, 240, 220, 200, 30, 60, 110},
	{220, 210, 180, 270, 200, 180, 60, 30, 100},
	{200, 190, 160, 220, 280, 300, 110, 100, 30},
}

func regionIndex(region string) int {
	for i, r := range regionOrder {
		if r == region {
			return i
		}
	}
	return -1
}

// EstimatedRTT is the rough round trip between two regions in ms, or 0 when
// either is unknown (callers treat 0 as "no estimate").
func EstimatedRTT(a, b string) int {
	ia, ib := regionIndex(a), regionIndex(b)
	if ia < 0 || ib < 0 {
		return 0
	}
	return rttMatrix[ia][ib]
}

// Continent groups regions for regional rankings.
type Continent struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Continents are the regional rankings, in display order.
var Continents = []Continent{
	{"na", "North America"}, {"sa", "South America"}, {"eu", "Europe"}, {"asia", "Asia"}, {"oc", "Oceania"},
}

// ContinentOf maps a lobby region to its continent id, "" when unknown.
func ContinentOf(region string) string {
	switch region {
	case RegionUSEast, RegionUSCentral, RegionUSWest:
		return "na"
	case RegionSA:
		return "sa"
	case RegionEUWest, RegionEUEast:
		return "eu"
	case RegionAsiaEast, RegionAsiaSE:
		return "asia"
	case RegionOceania:
		return "oc"
	}
	return ""
}

// ContinentName is a continent's display name, "" when unknown.
func ContinentName(id string) string {
	for _, c := range Continents {
		if c.ID == id {
			return c.Name
		}
	}
	return ""
}
