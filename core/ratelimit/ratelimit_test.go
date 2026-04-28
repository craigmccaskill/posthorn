package ratelimit_test

import (
	"net/http"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/craigmccaskill/posthorn/ratelimit"
)

// --- Constructor ---

func TestNew_InvalidArgs(t *testing.T) {
	tests := []struct {
		name     string
		count    int
		interval time.Duration
	}{
		{"zero_count", 0, time.Second},
		{"negative_count", -1, time.Second},
		{"zero_interval", 5, 0},
		{"negative_interval", 5, -time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := ratelimit.New(tt.count, tt.interval, 100); err == nil {
				t.Errorf("expected error for %+v", tt)
			}
		})
	}
}

func TestNew_DefaultMaxIPs(t *testing.T) {
	// maxIPs <= 0 should fall back to DefaultMaxIPs (10K). Construction
	// should succeed.
	l, err := ratelimit.New(5, time.Minute, 0)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if l == nil {
		t.Fatal("nil limiter")
	}
}

// --- Token bucket math ---

func TestAllow_FirstRequest_Succeeds(t *testing.T) {
	l, _ := ratelimit.New(5, time.Minute, 100)
	if !l.Allow("1.2.3.4") {
		t.Error("first request should be allowed")
	}
}

func TestAllow_BurstUpToCapacity(t *testing.T) {
	l, _ := ratelimit.New(5, time.Minute, 100)
	for i := 0; i < 5; i++ {
		if !l.Allow("1.2.3.4") {
			t.Errorf("request %d (within burst) should be allowed", i+1)
		}
	}
}

func TestAllow_ExceedsBurst_Denied(t *testing.T) {
	l, _ := ratelimit.New(3, time.Minute, 100)
	for i := 0; i < 3; i++ {
		l.Allow("1.2.3.4")
	}
	if l.Allow("1.2.3.4") {
		t.Error("4th request beyond burst of 3 should be denied")
	}
}

func TestAllow_DifferentIPsIndependent(t *testing.T) {
	l, _ := ratelimit.New(2, time.Minute, 100)
	for i := 0; i < 2; i++ {
		l.Allow("1.1.1.1")
	}
	// First IP exhausted, but 2.2.2.2 has its own bucket.
	if !l.Allow("2.2.2.2") {
		t.Error("different IP should have its own budget")
	}
}

func TestAllow_RefillsOverTime(t *testing.T) {
	// 60 per minute = 1 per second. After exhausting, waiting 2s should
	// allow 2 more requests.
	//
	// Use the test-only allowAt path to skip the actual sleep. (allowAt
	// is unexported but accessible via reflection — we use a different
	// strategy: wall-clock with a short interval.)
	l, _ := ratelimit.New(2, 100*time.Millisecond, 100)
	for i := 0; i < 2; i++ {
		l.Allow("1.1.1.1")
	}
	if l.Allow("1.1.1.1") {
		t.Fatal("should be exhausted")
	}
	time.Sleep(120 * time.Millisecond) // > 100ms = full refill of 2 tokens
	// Should now have ~2 tokens; allow at least one.
	if !l.Allow("1.1.1.1") {
		t.Error("after refill, request should be allowed")
	}
}

// --- LRU eviction ---

func TestAllow_LRUEvictionAtCap(t *testing.T) {
	l, _ := ratelimit.New(1, time.Minute, 3) // cap of 3 tracked IPs
	l.Allow("ip1")
	l.Allow("ip2")
	l.Allow("ip3")
	if got := l.Len(); got != 3 {
		t.Errorf("Len = %d, want 3", got)
	}
	// Adding a 4th should evict the oldest (ip1).
	l.Allow("ip4")
	if got := l.Len(); got != 3 {
		t.Errorf("Len after 4th IP = %d, want 3 (LRU should evict)", got)
	}
}

func TestAllow_EvictedIPGetsFreshBudget(t *testing.T) {
	// An evicted IP that was being limited just gets a new bucket. This
	// is the "graceful degradation" behavior documented in the architecture.
	l, _ := ratelimit.New(1, time.Minute, 2) // cap of 2

	// Exhaust ip1's bucket.
	l.Allow("ip1")
	if l.Allow("ip1") {
		t.Fatal("ip1 should be exhausted")
	}

	// Force eviction of ip1 by using ip2 and ip3.
	l.Allow("ip2")
	l.Allow("ip3") // evicts ip1

	// ip1 returns with a fresh bucket.
	if !l.Allow("ip1") {
		t.Error("evicted-and-returning IP should get fresh budget")
	}
}

// --- Concurrency ---

func TestAllow_ConcurrentSafe(t *testing.T) {
	// Smoke test for the mutex. Many goroutines hitting the same IP should
	// see the right total: either succeed (consume token) or fail (no token).
	// We just check no panics and the count makes sense.
	l, _ := ratelimit.New(100, time.Minute, 100)

	var wg sync.WaitGroup
	allowed := 0
	var mu sync.Mutex
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if l.Allow("1.1.1.1") {
				mu.Lock()
				allowed++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	// Capacity is 100; with no time passing during burst, exactly 100 should
	// have been allowed. Allow some slack for refill during the goroutine
	// scheduling (~10ms total).
	if allowed < 100 || allowed > 105 {
		t.Errorf("allowed = %d, want ~100 (capacity); concurrency-safety bug?", allowed)
	}
}

// --- Client IP extraction ---

func mustParsePrefixes(t *testing.T, cidrs ...string) []netip.Prefix {
	t.Helper()
	out, err := ratelimit.ParsePrefixes(cidrs)
	if err != nil {
		t.Fatalf("ParsePrefixes: %v", err)
	}
	return out
}

func TestClientIP_NoTrustedProxies_ReturnsRemoteAddr(t *testing.T) {
	req, _ := http.NewRequest(http.MethodPost, "/", nil)
	req.RemoteAddr = "203.0.113.5:12345"
	req.Header.Set("X-Forwarded-For", "1.1.1.1") // ignored
	if got := ratelimit.ClientIP(req, nil); got != "203.0.113.5" {
		t.Errorf("got %q, want %q", got, "203.0.113.5")
	}
}

func TestClientIP_RemoteAddrNotTrusted_IgnoresXFF(t *testing.T) {
	// Direct connection from random-IP attacker who set X-Forwarded-For
	// — must not trust their forged value.
	req, _ := http.NewRequest(http.MethodPost, "/", nil)
	req.RemoteAddr = "203.0.113.5:12345"
	req.Header.Set("X-Forwarded-For", "1.1.1.1")
	prefixes := mustParsePrefixes(t, "10.0.0.0/8")
	if got := ratelimit.ClientIP(req, prefixes); got != "203.0.113.5" {
		t.Errorf("got %q; X-F-F must be ignored when remote is not trusted", got)
	}
}

func TestClientIP_TrustedProxy_UsesXFF(t *testing.T) {
	req, _ := http.NewRequest(http.MethodPost, "/", nil)
	req.RemoteAddr = "10.0.0.1:33333"
	req.Header.Set("X-Forwarded-For", "203.0.113.5")
	prefixes := mustParsePrefixes(t, "10.0.0.0/8")
	if got := ratelimit.ClientIP(req, prefixes); got != "203.0.113.5" {
		t.Errorf("got %q, want %q", got, "203.0.113.5")
	}
}

func TestClientIP_TrustedProxy_RightmostUntrusted(t *testing.T) {
	// Multi-proxy chain: client → CDN → app proxy → us. The X-F-F looks
	// like "<real client>, <cdn>, <app proxy>". We see RemoteAddr from
	// app proxy. Walking right-to-left, we skip trusted proxies and
	// stop at the real client.
	req, _ := http.NewRequest(http.MethodPost, "/", nil)
	req.RemoteAddr = "10.0.0.1:33333" // app proxy
	req.Header.Set("X-Forwarded-For", "203.0.113.5, 192.168.1.10, 10.0.0.2")
	prefixes := mustParsePrefixes(t, "10.0.0.0/8", "192.168.0.0/16")
	if got := ratelimit.ClientIP(req, prefixes); got != "203.0.113.5" {
		t.Errorf("got %q, want %q (rightmost untrusted)", got, "203.0.113.5")
	}
}

func TestClientIP_TrustedProxy_ForgedXFF_Defended(t *testing.T) {
	// Attacker is a real client (203.0.113.5) sending a request through
	// a trusted proxy (10.0.0.1). Attacker forges X-F-F to claim they're
	// some other IP. Real proxy will APPEND attacker's real IP to X-F-F.
	// Walking right-to-left lands on attacker's real IP, ignoring the
	// forged prefix.
	req, _ := http.NewRequest(http.MethodPost, "/", nil)
	req.RemoteAddr = "10.0.0.1:33333"
	// Attacker tried to set "1.1.1.1" but the trusted proxy appended
	// the real client IP (203.0.113.5) to the right.
	req.Header.Set("X-Forwarded-For", "1.1.1.1, 203.0.113.5")
	prefixes := mustParsePrefixes(t, "10.0.0.0/8")
	if got := ratelimit.ClientIP(req, prefixes); got != "203.0.113.5" {
		t.Errorf("got %q, want %q (rightmost untrusted = real client)", got, "203.0.113.5")
	}
}

func TestClientIP_TrustedProxy_NoXFF(t *testing.T) {
	// Trusted proxy didn't set X-F-F — fall back to RemoteAddr.
	req, _ := http.NewRequest(http.MethodPost, "/", nil)
	req.RemoteAddr = "10.0.0.1:33333"
	prefixes := mustParsePrefixes(t, "10.0.0.0/8")
	if got := ratelimit.ClientIP(req, prefixes); got != "10.0.0.1" {
		t.Errorf("got %q", got)
	}
}

func TestClientIP_AllXFFEntriesTrusted(t *testing.T) {
	// All entries in X-F-F are themselves trusted proxies. Fall back to
	// the leftmost (best guess at original client).
	req, _ := http.NewRequest(http.MethodPost, "/", nil)
	req.RemoteAddr = "10.0.0.1:33333"
	req.Header.Set("X-Forwarded-For", "10.0.0.5, 10.0.0.6, 10.0.0.7")
	prefixes := mustParsePrefixes(t, "10.0.0.0/8")
	if got := ratelimit.ClientIP(req, prefixes); got != "10.0.0.5" {
		t.Errorf("got %q, want leftmost", got)
	}
}

// --- ParsePrefixes ---

func TestParsePrefixes_Valid(t *testing.T) {
	got, err := ratelimit.ParsePrefixes([]string{"10.0.0.0/8", "192.168.0.0/16", "::1/128"})
	if err != nil {
		t.Fatalf("ParsePrefixes: %v", err)
	}
	if len(got) != 3 {
		t.Errorf("len = %d, want 3", len(got))
	}
}

func TestParsePrefixes_Invalid(t *testing.T) {
	_, err := ratelimit.ParsePrefixes([]string{"not-a-cidr"})
	if err == nil {
		t.Error("expected parse error")
	}
}

func TestParsePrefixes_Empty(t *testing.T) {
	got, err := ratelimit.ParsePrefixes(nil)
	if err != nil {
		t.Errorf("ParsePrefixes(nil): %v", err)
	}
	if got != nil {
		t.Errorf("got %v, want nil for empty input", got)
	}
}
