package idempotency

import (
	"strings"
	"sync"
	"testing"
	"time"
)

// --- Constructor ---

func TestNew_ValidArgs(t *testing.T) {
	c, err := New(100, time.Hour)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if c == nil {
		t.Fatal("New returned nil cache without error")
	}
}

func TestNew_RejectsZeroCapacity(t *testing.T) {
	if _, err := New(0, time.Hour); err == nil {
		t.Error("New(0, h): expected error")
	}
	if _, err := New(-1, time.Hour); err == nil {
		t.Error("New(-1, h): expected error")
	}
}

func TestNew_RejectsZeroTTL(t *testing.T) {
	if _, err := New(10, 0); err == nil {
		t.Error("New(10, 0): expected error")
	}
	if _, err := New(10, -time.Second); err == nil {
		t.Error("New(10, -1s): expected error")
	}
}

// --- Lookup / ClaimInFlight / Store basic flow ---

func TestLookup_EmptyCache_Miss(t *testing.T) {
	c, _ := New(10, time.Hour)
	resp, inFlight := c.Lookup("nope")
	if resp != nil || inFlight {
		t.Errorf("Lookup empty cache: resp=%v inFlight=%v, want nil/false", resp, inFlight)
	}
}

func TestClaimInFlight_FirstClaimSucceeds(t *testing.T) {
	c, _ := New(10, time.Hour)
	if !c.ClaimInFlight("key1") {
		t.Error("first ClaimInFlight returned false")
	}
}

func TestClaimInFlight_DuplicateFails(t *testing.T) {
	c, _ := New(10, time.Hour)
	c.ClaimInFlight("key1")
	if c.ClaimInFlight("key1") {
		t.Error("second ClaimInFlight on same key returned true")
	}
}

func TestLookup_DuringInFlight_ReturnsInFlight(t *testing.T) {
	c, _ := New(10, time.Hour)
	c.ClaimInFlight("key1")
	resp, inFlight := c.Lookup("key1")
	if !inFlight || resp != nil {
		t.Errorf("Lookup during in-flight: resp=%v inFlight=%v, want nil/true", resp, inFlight)
	}
}

func TestStore_ReleasesInFlight(t *testing.T) {
	c, _ := New(10, time.Hour)
	c.ClaimInFlight("key1")
	c.Store("key1", Response{Status: 200, Body: []byte(`{"ok":true}`), ContentType: "application/json"})

	// New ClaimInFlight should fail because the key is cached.
	if c.ClaimInFlight("key1") {
		t.Error("ClaimInFlight after Store returned true (key is cached)")
	}
	// Lookup should return the stored response, not in-flight.
	resp, inFlight := c.Lookup("key1")
	if inFlight {
		t.Error("Lookup after Store reports inFlight=true")
	}
	if resp == nil {
		t.Fatal("Lookup after Store returned nil response")
	}
	if resp.Status != 200 || string(resp.Body) != `{"ok":true}` || resp.ContentType != "application/json" {
		t.Errorf("stored response not returned verbatim: %+v", resp)
	}
}

func TestAbandon_ReleasesInFlightWithoutCaching(t *testing.T) {
	c, _ := New(10, time.Hour)
	c.ClaimInFlight("key1")
	c.Abandon("key1")
	// Now a fresh claim should succeed.
	if !c.ClaimInFlight("key1") {
		t.Error("ClaimInFlight after Abandon returned false")
	}
	// And the cache is empty.
	resp, _ := c.Lookup("key1")
	if resp != nil {
		t.Errorf("Lookup after Abandon returned cached response: %+v", resp)
	}
}

// --- Replay byte-identical (NFR20) ---

func TestStore_ResponseCopiedNotShared(t *testing.T) {
	c, _ := New(10, time.Hour)
	body := []byte(`{"submission_id":"abc-123"}`)
	c.Store("key1", Response{Status: 200, Body: body, ContentType: "application/json"})

	// Mutate the caller's body buffer; the cached entry must be unaffected.
	body[1] = 'X'

	resp, _ := c.Lookup("key1")
	if resp == nil {
		t.Fatal("Lookup returned nil")
	}
	if string(resp.Body) != `{"submission_id":"abc-123"}` {
		t.Errorf("cache shared body slice with caller: got %q", string(resp.Body))
	}
}

func TestStore_LookupReturnsCopy(t *testing.T) {
	c, _ := New(10, time.Hour)
	c.Store("key1", Response{Status: 200, Body: []byte(`xyz`), ContentType: "text/plain"})

	resp1, _ := c.Lookup("key1")
	resp1.Body[0] = 'X'

	resp2, _ := c.Lookup("key1")
	if string(resp2.Body) != "xyz" {
		t.Errorf("Lookup returned shared mutable body: got %q on second lookup", string(resp2.Body))
	}
}

// --- TTL eviction ---

func TestTTL_ExpiredEntryEvicted(t *testing.T) {
	c, _ := New(10, time.Minute)

	// Inject a clock that we can advance.
	currentTime := time.Now()
	c.now = func() time.Time { return currentTime }

	c.Store("key1", Response{Status: 200, Body: []byte("ok")})

	// Within TTL: hit.
	if resp, _ := c.Lookup("key1"); resp == nil {
		t.Error("entry missing within TTL")
	}

	// Past TTL: miss, and entry evicted.
	currentTime = currentTime.Add(2 * time.Minute)
	if resp, inFlight := c.Lookup("key1"); resp != nil || inFlight {
		t.Errorf("entry survived past TTL: resp=%v inFlight=%v", resp, inFlight)
	}
	// And a fresh claim succeeds (eviction released the slot).
	if !c.ClaimInFlight("key1") {
		t.Error("ClaimInFlight after TTL eviction returned false")
	}
}

// --- LRU eviction at capacity ---

func TestLRU_EvictsOldestAtCapacity(t *testing.T) {
	c, _ := New(2, time.Hour)
	c.Store("key1", Response{Status: 200, Body: []byte("a")})
	c.Store("key2", Response{Status: 200, Body: []byte("b")})
	c.Store("key3", Response{Status: 200, Body: []byte("c")}) // should evict key1

	if resp, _ := c.Lookup("key1"); resp != nil {
		t.Error("key1 should have been LRU-evicted")
	}
	if resp, _ := c.Lookup("key2"); resp == nil {
		t.Error("key2 unexpectedly evicted")
	}
	if resp, _ := c.Lookup("key3"); resp == nil {
		t.Error("key3 not stored")
	}
}

// --- Concurrent access (race detector catches obvious bugs) ---

func TestConcurrent_ClaimAndStore(t *testing.T) {
	c, _ := New(100, time.Hour)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			key := "key-" + string(rune('a'+(i%26)))
			if c.ClaimInFlight(key) {
				c.Store(key, Response{Status: 200, Body: []byte(key)})
			} else {
				c.Lookup(key)
			}
		}(i)
	}
	wg.Wait()
}

// --- ValidateKey ---

func TestValidateKey_Valid(t *testing.T) {
	for _, k := range []string{
		"a",
		"550e8400-e29b-41d4-a716-446655440000", // UUID v4
		"abcdef0123456789",                     // hex
		"key:with:colons",
		strings.Repeat("x", 255), // max length
	} {
		t.Run(k, func(t *testing.T) {
			if err := ValidateKey(k); err != nil {
				t.Errorf("ValidateKey(%q): unexpected error %v", k, err)
			}
		})
	}
}

func TestValidateKey_Empty(t *testing.T) {
	if err := ValidateKey(""); err == nil {
		t.Error("ValidateKey(\"\"): expected error")
	}
}

func TestValidateKey_TooLong(t *testing.T) {
	tooLong := strings.Repeat("x", 256)
	if err := ValidateKey(tooLong); err == nil {
		t.Error("ValidateKey(256 chars): expected error")
	}
}

func TestValidateKey_NonPrintable(t *testing.T) {
	tests := []struct {
		name string
		key  string
	}{
		{"newline", "key\nwith\nnewline"},
		{"tab", "key\twith\ttab"},
		{"null_byte", "key\x00null"},
		{"del", "key\x7Fdel"},        // 0x7F is DEL — not in 0x20-0x7E
		{"unicode_multibyte", "kéy"}, // é is multibyte UTF-8
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := ValidateKey(tt.key); err == nil {
				t.Errorf("ValidateKey(%q): expected error", tt.key)
			}
		})
	}
}
