// Package idempotency implements the per-endpoint Idempotency-Key cache
// for api-mode endpoints (FR40–FR44, NFR20, ADR-8).
//
// Each api-mode endpoint owns one Cache instance. The cache is in-memory,
// LRU-bounded, TTL-evicted, and tracks in-flight requests so concurrent
// retries with the same key resolve to a 409 rather than a duplicate
// transport call.
//
// v2 will swap the storage backing for SQLite without changing this
// package's interface (ADR-8). Callers should treat the package as a
// black-box "remember this response under this key" primitive.
package idempotency

import (
	"errors"
	"fmt"
	"sync"
	"time"

	lru "github.com/hashicorp/golang-lru/v2"
)

// FR43 key shape constraints.
const (
	MinKeyLength = 1
	MaxKeyLength = 255
)

// DefaultCapacity is the default per-endpoint cache size when the operator
// hasn't configured idempotency_cache_size (FR42).
const DefaultCapacity = 10_000

// DefaultTTL is the cached-entry lifetime (FR40). v2 may persist beyond
// this; v1.1 is in-memory only so entries also vanish on process restart.
const DefaultTTL = 24 * time.Hour

// Response is the snapshot stored under an idempotency key. Stored fields
// are sufficient to write a byte-identical replay back to the caller
// (NFR20). Submission IDs and transport message IDs travel inside Body
// (which is already serialized JSON) — no separate field needed.
type Response struct {
	Status      int
	Body        []byte
	ContentType string
}

// Cache is the per-endpoint idempotency state. Safe for concurrent use.
type Cache struct {
	mu       sync.Mutex
	lru      *lru.Cache[string, entry]
	inflight map[string]struct{}
	ttl      time.Duration

	// now is injectable so TTL-eviction tests can drive time forward
	// without sleeping. Production: time.Now.
	now func() time.Time
}

type entry struct {
	response Response
	expires  time.Time
}

// New constructs a Cache with the given capacity and TTL. Both must be
// positive; a zero or negative capacity / TTL is a configuration error.
func New(capacity int, ttl time.Duration) (*Cache, error) {
	if capacity <= 0 {
		return nil, fmt.Errorf("idempotency: capacity must be positive, got %d", capacity)
	}
	if ttl <= 0 {
		return nil, fmt.Errorf("idempotency: ttl must be positive, got %v", ttl)
	}
	l, err := lru.New[string, entry](capacity)
	if err != nil {
		return nil, fmt.Errorf("idempotency: lru: %w", err)
	}
	return &Cache{
		lru:      l,
		inflight: make(map[string]struct{}),
		ttl:      ttl,
		now:      time.Now,
	}, nil
}

// Lookup checks the cache for a complete entry under key. The result has
// exactly one of three shapes:
//
//   - (resp != nil, false): cached response exists and is TTL-valid; the
//     caller should replay resp byte-for-byte to the client.
//   - (nil, true): another request with this key is currently in flight;
//     the caller should return HTTP 409 (FR44).
//   - (nil, false): no entry; the caller should ClaimInFlight then
//     proceed to handle the request normally.
//
// A returned *Response is a copy; mutating it has no effect on the cache.
func (c *Cache) Lookup(key string) (resp *Response, inFlight bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if _, ok := c.inflight[key]; ok {
		return nil, true
	}
	if e, ok := c.lru.Get(key); ok {
		if c.now().Before(e.expires) {
			// Defensive copy of Body — the cache owns its bytes and the
			// caller must not be able to mutate them.
			bodyCopy := make([]byte, len(e.response.Body))
			copy(bodyCopy, e.response.Body)
			return &Response{
				Status:      e.response.Status,
				Body:        bodyCopy,
				ContentType: e.response.ContentType,
			}, false
		}
		// Expired — evict so the next caller can claim in-flight.
		c.lru.Remove(key)
	}
	return nil, false
}

// ClaimInFlight reserves the key as currently being processed. Returns
// true on a successful claim; false if the key is already in flight or
// already cached (caller should re-Lookup to resolve).
//
// The caller MUST call exactly one of Store or Abandon to release the
// claim — typically via defer in the request handler.
func (c *Cache) ClaimInFlight(key string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.inflight[key]; ok {
		return false
	}
	if e, ok := c.lru.Get(key); ok && c.now().Before(e.expires) {
		return false
	}
	c.inflight[key] = struct{}{}
	return true
}

// Store records the final response under key and releases the in-flight
// claim. The cached entry expires after the configured TTL.
//
// Calling Store without a prior ClaimInFlight is permitted (the caller
// already serialized access by some other means); the in-flight delete
// is a no-op in that case.
func (c *Cache) Store(key string, resp Response) {
	c.mu.Lock()
	defer c.mu.Unlock()
	bodyCopy := make([]byte, len(resp.Body))
	copy(bodyCopy, resp.Body)
	c.lru.Add(key, entry{
		response: Response{
			Status:      resp.Status,
			Body:        bodyCopy,
			ContentType: resp.ContentType,
		},
		expires: c.now().Add(c.ttl),
	})
	delete(c.inflight, key)
}

// Abandon releases the in-flight claim without caching a response. Use
// when a request errors out before producing a final response, or when
// the caller decides the response should not be cached (e.g. 5xx, which
// the operator may want to retry).
func (c *Cache) Abandon(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.inflight, key)
}

// ValidateKey checks that key conforms to FR43 — 1-255 characters of
// printable ASCII (0x20-0x7E). Returns nil for valid keys, a descriptive
// error otherwise; the caller maps non-nil errors to HTTP 400.
func ValidateKey(key string) error {
	if len(key) < MinKeyLength {
		return errors.New("Idempotency-Key must not be empty")
	}
	if len(key) > MaxKeyLength {
		return fmt.Errorf("Idempotency-Key length %d exceeds maximum %d", len(key), MaxKeyLength)
	}
	for i := 0; i < len(key); i++ {
		b := key[i]
		if b < 0x20 || b > 0x7E {
			return fmt.Errorf("Idempotency-Key contains non-printable byte 0x%02X at position %d", b, i)
		}
	}
	return nil
}
