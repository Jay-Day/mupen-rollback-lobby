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
