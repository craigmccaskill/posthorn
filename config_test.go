package formward

import (
	"encoding/json"
	"testing"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
)

// TestModule_JSONMarshal verifies that a populated Module marshals to a JSON
// object with the expected key names (i.e., the json: struct tags are correct).
func TestModule_JSONMarshal(t *testing.T) {
	m := &Module{Path: "/contact"}
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("unmarshal to map: %v", err)
	}

	got, ok := raw["path"]
	if !ok {
		t.Fatalf("JSON missing %q key; got %s", "path", data)
	}
	if got != "/contact" {
		t.Errorf("path = %v, want %q", got, "/contact")
	}
}

// TestModule_JSONRoundTrip verifies that marshal → unmarshal preserves all
// fields, proving the json: tags are consistent in both directions.
func TestModule_JSONRoundTrip(t *testing.T) {
	original := &Module{Path: "/test"}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var got Module
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if got.Path != original.Path {
		t.Errorf("Path = %q, want %q", got.Path, original.Path)
	}
}

// TestModule_JSONOmitEmpty verifies that a zero-value Module serializes to
// "{}" — no stray null or empty-string keys. This matters for Caddy's config
// diffing: unexpected keys can trigger unnecessary reloads.
func TestModule_JSONOmitEmpty(t *testing.T) {
	data, err := json.Marshal(&Module{})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(data) != "{}" {
		t.Errorf("got %s, want {}", data)
	}
}

// TestModule_CaddyfileToJSONEquivalence verifies that configuring the module
// via Caddyfile and via JSON produces the same struct state. This is the
// unit-level proxy for the "caddy adapt" round-trip acceptance criterion.
//
// Manual verification of the full caddy adapt output:
//
//	printf ':18080 {\n  formward /test\n}\n' | caddy adapt --pretty
//
// Expected: routes entry with match=[{path:["/test"]}] and
// handle=[{handler:"formward", path:"/test"}].
func TestModule_CaddyfileToJSONEquivalence(t *testing.T) {
	// Parse via Caddyfile path.
	d := caddyfile.NewTestDispenser(`formward /test`)
	var fromCaddyfile Module
	if err := fromCaddyfile.UnmarshalCaddyfile(d); err != nil {
		t.Fatalf("UnmarshalCaddyfile: %v", err)
	}

	// Simulate JSON config path: marshal then unmarshal.
	data, err := json.Marshal(&fromCaddyfile)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var fromJSON Module
	if err := json.Unmarshal(data, &fromJSON); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// Both paths must produce identical state.
	if fromJSON.Path != fromCaddyfile.Path {
		t.Errorf("Path mismatch: Caddyfile=%q JSON=%q", fromCaddyfile.Path, fromJSON.Path)
	}
}

// TestProvision_NoError verifies the Provision stub returns nil. The real
// implementation grows through Epics 2-4.
func TestProvision_NoError(t *testing.T) {
	m := &Module{Path: "/test"}
	if err := m.Provision(caddy.Context{}); err != nil {
		t.Errorf("Provision returned unexpected error: %v", err)
	}
}

// TestValidate_NoError verifies the Validate stub returns nil. The real
// implementation lands in Epic 2 when to/from/transport config is added.
func TestValidate_NoError(t *testing.T) {
	m := &Module{Path: "/test"}
	if err := m.Validate(); err != nil {
		t.Errorf("Validate returned unexpected error: %v", err)
	}
}
