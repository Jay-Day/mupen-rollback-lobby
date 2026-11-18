// Package ipblock refuses connections from cloud-provider address space.
// Players connect from home and mobile ISPs; scripted floods come from rented
// servers, so blocking those ranges outright costs real users nothing.
package ipblock

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/netip"
	"sync/atomic"
	"time"
)

// AWSRangesURL is Amazon's published list of every AWS IP range.
const AWSRangesURL = "https://ip-ranges.amazonaws.com/ip-ranges.json"

// Refresh cadence. Amazon updates the list a few times a week; a failed fetch
// retries sooner so a server that starts while offline catches up quickly.
const (
	refreshInterval = 12 * time.Hour
	retryInterval   = 10 * time.Minute
	fetchTimeout    = 30 * time.Second
	maxBodyBytes    = 16 << 20 // the list is ~2 MB; cap well above it
)

// AWS holds the current AWS ranges. The zero value blocks nothing until the
// first successful refresh, so a fetch failure fails open instead of locking
// everyone out.
type AWS struct {
	url      string
	client   *http.Client
	prefixes atomic.Pointer[[]netip.Prefix]
}

func NewAWS() *AWS {
	return &AWS{url: AWSRangesURL, client: &http.Client{Timeout: fetchTimeout}}
}

// Contains reports whether ip (as written in a request's RemoteAddr host) is
// inside an AWS range.
func (a *AWS) Contains(ip string) bool {
	list := a.prefixes.Load()
	if list == nil {
		return false
	}
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return false
	}
	addr = addr.Unmap() // ::ffff:1.2.3.4 must match the IPv4 ranges
	for _, p := range *list {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

// Count is how many ranges are loaded (0 before the first refresh).
func (a *AWS) Count() int {
	if list := a.prefixes.Load(); list != nil {
		return len(*list)
	}
	return 0
}

// Run refreshes the list now and then periodically until ctx ends.
func (a *AWS) Run(ctx context.Context) {
	for {
		wait := refreshInterval
		if err := a.Refresh(ctx); err != nil {
			slog.Warn("aws ip ranges refresh failed; keeping previous list",
				"err", err, "loaded", a.Count(), "retry_in", retryInterval)
			wait = retryInterval
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// Refresh downloads and swaps in the current list.
func (a *AWS) Refresh(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.url, nil)
	if err != nil {
		return err
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status %s", resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return err
	}
	list, err := parseAWSRanges(body)
	if err != nil {
		return err
	}
	a.prefixes.Store(&list)
	slog.Info("aws ip ranges loaded", "ranges", len(list))
	return nil
}

// parseAWSRanges extracts every IPv4 and IPv6 prefix, deduplicated (the file
// lists the same prefix once per service). An empty list is an error so a
// truncated download can't silently unblock everything.
func parseAWSRanges(data []byte) ([]netip.Prefix, error) {
	var doc struct {
		Prefixes []struct {
			IPPrefix string `json:"ip_prefix"`
		} `json:"prefixes"`
		IPv6Prefixes []struct {
			IPv6Prefix string `json:"ipv6_prefix"`
		} `json:"ipv6_prefixes"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	seen := make(map[netip.Prefix]struct{})
	var out []netip.Prefix
	add := func(s string) {
		p, err := netip.ParsePrefix(s)
		if err != nil {
			return
		}
		p = p.Masked()
		if _, dup := seen[p]; dup {
			return
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}
	for _, p := range doc.Prefixes {
		add(p.IPPrefix)
	}
	for _, p := range doc.IPv6Prefixes {
		add(p.IPv6Prefix)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no ranges in AWS ip-ranges document")
	}
	return out, nil
}
