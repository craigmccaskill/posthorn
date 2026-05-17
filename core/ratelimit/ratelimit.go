// Package ratelimit implements a per-IP token-bucket rate limiter with
// LRU eviction (FR8, NFR6) and a proxy-aware client-IP extractor that
// honors a trusted_proxies CIDR list (FR9).
//
// Token bucket is the standard rate-limit primitive. Each tracked IP
// gets a bucket of `capacity` tokens that refills at
// `capacity / interval` tokens per second. Each request consumes one
// token; when the bucket is empty, requests are denied until enough
// time passes for a token to refill.
//
// Memory is bounded by an LRU cache (default 10K IPs per limiter
// instance) so a high-cardinality attack rotating through fresh IPs
// can't exhaust memory. Eviction of an IP resets its bucket on next
// request — that's acceptable: an evicted IP that was being limited
// just gets a fresh budget. Worst case is botnet spam, which the
// brief explicitly defers to v3 (captcha or proof-of-work).
//
// Per ADR-3: rolled instead of using golang.org/x/time/rate because
// x/time/rate doesn't bound memory.
package ratelimit

import (
	"errors"
	"math"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"

	lru "github.com/hashicorp/golang-lru/v2"
)

// DefaultMaxIPs caps the number of distinct IPs tracked per Limiter.
// Tuned to bound memory at ~640KB per endpoint (rough — 64 bytes per
// entry × 10K entries).
const DefaultMaxIPs = 10_000

// bucket is the per-IP token-bucket state.
type bucket struct {
	tokens float64
	last   time.Time
}

// Limiter is a per-IP token-bucket rate limiter with LRU eviction.
// The zero value is not usable; construct via [New].
type Limiter struct {
	capacity     float64
	refillPerSec float64

	mu      sync.Mutex
	buckets *lru.Cache[string, *bucket]
}

// New constructs a Limiter that allows `count` requests per `interval`.
// maxIPs caps the number of distinct keys tracked; zero or negative
// uses [DefaultMaxIPs].
//
// Returns an error if count or interval is non-positive (operator
// misconfiguration; Validate() in core/config catches this earlier).
func New(count int, interval time.Duration, maxIPs int) (*Limiter, error) {
	if count <= 0 {
		return nil, errors.New("ratelimit: count must be positive")
	}
	if interval <= 0 {
		return nil, errors.New("ratelimit: interval must be positive")
	}
	if maxIPs <= 0 {
		maxIPs = DefaultMaxIPs
	}
	cache, err := lru.New[string, *bucket](maxIPs)
	if err != nil {
		return nil, err
	}
	return &Limiter{
		capacity:     float64(count),
		refillPerSec: float64(count) / interval.Seconds(),
		buckets:      cache,
	}, nil
}

// Allow reports whether a request from clientIP is permitted now. It
// consumes one token on success and returns false (no token consumed)
// when the bucket is exhausted.
//
// Safe for concurrent use; one mutex per Limiter, held only during
// the per-IP bucket update.
func (l *Limiter) Allow(clientIP string) bool {
	return l.allowAt(clientIP, time.Now())
}

// allowAt is the testable form of Allow. Pinning the "now" timestamp
// lets tests verify bucket math without sleeping.
func (l *Limiter) allowAt(clientIP string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	b, ok := l.buckets.Get(clientIP)
	if !ok {
		// New IP: fresh full bucket.
		b = &bucket{tokens: l.capacity, last: now}
		l.buckets.Add(clientIP, b)
	}

	// Refill based on elapsed time, capped at capacity.
	elapsed := now.Sub(b.last).Seconds()
	if elapsed > 0 {
		b.tokens = math.Min(l.capacity, b.tokens+elapsed*l.refillPerSec)
		b.last = now
	}

	if b.tokens >= 1 {
		b.tokens--
		return true
	}
	return false
}

// Len returns the number of IPs currently tracked. For tests and metrics.
func (l *Limiter) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buckets.Len()
}

// --- Client IP extraction ---

// ClientIP returns the IP address Posthorn should attribute the request
// to. If trustedProxies is non-empty AND the request's RemoteAddr
// matches one of the trusted CIDRs, walks the X-Forwarded-For header
// from right to left and returns the rightmost address NOT in any
// trusted proxy range. Otherwise returns RemoteAddr's IP.
//
// Rationale: if the request came from a trusted proxy (Cloudflare,
// nginx, Caddy, etc.), the original client IP is in X-Forwarded-For.
// Walking right-to-left and stopping at the first untrusted IP gives
// the actual client even with multiple proxy hops, while preventing a
// malicious client from forging arbitrary X-Forwarded-For values
// (their forged entries would precede the trusted proxy's appended
// real-IP entry, so right-to-left walking ignores them).
func ClientIP(r *http.Request, trustedProxies []netip.Prefix) string {
	remote := remoteIP(r)
	if len(trustedProxies) == 0 {
		return remote
	}
	if !ipInPrefixes(remote, trustedProxies) {
		// Connection isn't from a trusted proxy — don't trust X-F-F.
		return remote
	}
	xff := r.Header.Get("X-Forwarded-For")
	if xff == "" {
		return remote
	}
	parts := strings.Split(xff, ",")
	// Walk right to left.
	for i := len(parts) - 1; i >= 0; i-- {
		candidate := strings.TrimSpace(parts[i])
		if candidate == "" {
			continue
		}
		if !ipInPrefixes(candidate, trustedProxies) {
			return candidate
		}
	}
	// All entries were trusted proxies — fall back to the leftmost.
	first := strings.TrimSpace(parts[0])
	if first != "" {
		return first
	}
	return remote
}

// remoteIP strips the port from r.RemoteAddr and returns the bare IP.
func remoteIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		// RemoteAddr may already be a bare IP in some test contexts.
		return r.RemoteAddr
	}
	return host
}

// ipInPrefixes reports whether ipStr is in any of the given CIDR prefixes.
// Bad inputs (unparseable IP, mismatched address family) return false.
func ipInPrefixes(ipStr string, prefixes []netip.Prefix) bool {
	addr, err := netip.ParseAddr(ipStr)
	if err != nil {
		return false
	}
	for _, p := range prefixes {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

// ParsePrefixes parses a list of CIDR strings or named presets into
// netip.Prefix values suitable for [ClientIP]. Presets (FR58) expand
// to their canonical CIDR list at parse time; operators can mix
// presets and explicit CIDRs in one list.
//
// Reports the first parse failure with the offending string.
func ParsePrefixes(items []string) ([]netip.Prefix, error) {
	if len(items) == 0 {
		return nil, nil
	}
	var out []netip.Prefix
	for _, item := range items {
		if IsPreset(item) {
			for _, c := range Presets[item] {
				p, err := netip.ParsePrefix(c)
				if err != nil {
					return nil, errors.New("preset " + item + ": invalid CIDR " + c)
				}
				out = append(out, p)
			}
			continue
		}
		p, err := netip.ParsePrefix(item)
		if err != nil {
			return nil, errors.New("invalid CIDR or unknown preset: " + item)
		}
		out = append(out, p)
	}
	return out, nil
}
