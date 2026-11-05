// Package clientip finds a request's client address, also behind a reverse
// proxy.
package clientip

import (
	"fmt"
	"net"
	"net/http"
	"strings"
)

// Resolver returns client addresses. Requests from a trusted proxy use the
// X-Forwarded-For chain: the right-most address that isn't itself a trusted
// proxy (addresses further left are client-supplied and can be forged).
// With no trusted proxies, the connection's address is used as is.
type Resolver struct {
	trusted []*net.IPNet
}

// Parse reads a comma-separated list of proxy IPs or CIDRs, e.g.
// "127.0.0.1, 10.0.0.0/8". An empty list trusts no proxy.
func Parse(spec string) (*Resolver, error) {
	r := &Resolver{}
	for _, field := range strings.Split(spec, ",") {
		field = strings.TrimSpace(field)
		if field == "" {
			continue
		}
		if !strings.Contains(field, "/") {
			ip := net.ParseIP(field)
			if ip == nil {
				return nil, fmt.Errorf("trusted proxy %q isn't an IP or CIDR", field)
			}
			bits := 32
			if ip.To4() == nil {
				bits = 128
			}
			field = fmt.Sprintf("%s/%d", ip.String(), bits)
		}
		_, network, err := net.ParseCIDR(field)
		if err != nil {
			return nil, fmt.Errorf("trusted proxy %q: %w", field, err)
		}
		r.trusted = append(r.trusted, network)
	}
	return r, nil
}

// Proxies reports how many proxy ranges are trusted.
func (r *Resolver) Proxies() int {
	if r == nil {
		return 0
	}
	return len(r.trusted)
}

func (r *Resolver) isTrusted(ip net.IP) bool {
	for _, network := range r.trusted {
		if network.Contains(ip) {
			return true
		}
	}
	return false
}

// IP returns the request's client address.
func (r *Resolver) IP(req *http.Request) string {
	remote, _, err := net.SplitHostPort(req.RemoteAddr)
	if err != nil {
		remote = req.RemoteAddr
	}
	if r == nil || len(r.trusted) == 0 {
		return remote
	}
	remoteIP := net.ParseIP(remote)
	if remoteIP == nil || !r.isTrusted(remoteIP) {
		return remote
	}
	hops := []string{}
	for _, header := range req.Header.Values("X-Forwarded-For") {
		for _, hop := range strings.Split(header, ",") {
			hops = append(hops, strings.TrimSpace(hop))
		}
	}
	for i := len(hops) - 1; i >= 0; i-- {
		ip := net.ParseIP(hops[i])
		if ip == nil {
			return remote // malformed chain: don't guess
		}
		if !r.isTrusted(ip) {
			return ip.String()
		}
	}
	return remote
}
