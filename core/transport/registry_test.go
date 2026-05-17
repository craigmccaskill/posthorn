package transport

import (
	"strings"
	"testing"
)

// Test-fixture registrations use type names prefixed "test-9_1-" so they
// can't collide with real transports. The registry retains entries
// across tests in the same package — that's fine because real-transport
// tests don't grep KnownTypes for completeness.

func TestRegister_LookupRoundTrip(t *testing.T) {
	const typ = "test-9_1-roundtrip"
	called := false
	Register(Registration{
		Type:     typ,
		Validate: func(map[string]any) error { return nil },
		Build: func(map[string]any) (Transport, error) {
			called = true
			return nil, nil
		},
	})

	reg, ok := Lookup(typ)
	if !ok {
		t.Fatal("Lookup did not find registered transport")
	}
	if reg.Type != typ {
		t.Errorf("reg.Type = %q, want %q", reg.Type, typ)
	}
	if _, err := reg.Build(nil); err != nil {
		t.Errorf("Build: %v", err)
	}
	if !called {
		t.Error("Build closure not invoked")
	}
}

func TestRegister_DuplicateTypePanics(t *testing.T) {
	const typ = "test-9_1-dup"
	Register(Registration{
		Type:     typ,
		Validate: func(map[string]any) error { return nil },
		Build:    func(map[string]any) (Transport, error) { return nil, nil },
	})

	defer func() {
		r := recover()
		if r == nil {
			t.Error("duplicate Register did not panic")
		}
		msg, _ := r.(string)
		if !strings.Contains(msg, "duplicate") {
			t.Errorf("panic message = %q, want it to mention duplicate", msg)
		}
	}()
	Register(Registration{
		Type:     typ,
		Validate: func(map[string]any) error { return nil },
		Build:    func(map[string]any) (Transport, error) { return nil, nil },
	})
}

func TestRegister_EmptyTypePanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("empty Type did not panic")
		}
	}()
	Register(Registration{
		Type:     "",
		Validate: func(map[string]any) error { return nil },
		Build:    func(map[string]any) (Transport, error) { return nil, nil },
	})
}

func TestRegister_NilValidatePanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("nil Validate did not panic")
		}
	}()
	Register(Registration{
		Type:     "test-9_1-nil-validate",
		Validate: nil,
		Build:    func(map[string]any) (Transport, error) { return nil, nil },
	})
}

func TestRegister_NilBuildPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("nil Build did not panic")
		}
	}()
	Register(Registration{
		Type:     "test-9_1-nil-build",
		Validate: func(map[string]any) error { return nil },
		Build:    nil,
	})
}

func TestLookup_UnknownReturnsFalse(t *testing.T) {
	_, ok := Lookup("test-9_1-does-not-exist")
	if ok {
		t.Error("Lookup of unregistered type returned ok=true")
	}
}

func TestKnownTypes_IncludesPostmark(t *testing.T) {
	// Postmark registers in its init() — it's always present.
	known := KnownTypes()
	found := false
	for _, k := range known {
		if k == "postmark" {
			found = true
		}
	}
	if !found {
		t.Errorf("KnownTypes() does not include postmark: %v", known)
	}
}

func TestKnownTypes_Sorted(t *testing.T) {
	known := KnownTypes()
	for i := 1; i < len(known); i++ {
		if known[i] < known[i-1] {
			t.Errorf("KnownTypes() not sorted: %v", known)
			break
		}
	}
}

func TestUnknownTypeError_NamesType(t *testing.T) {
	err := UnknownTypeError("nonsense-transport")
	if err == nil {
		t.Fatal("nil error")
	}
	if !strings.Contains(err.Error(), "nonsense-transport") {
		t.Errorf("error %q should name the unknown type", err.Error())
	}
}

// TestPostmark_RegisteredAtPackageLoad confirms that postmark.go's init()
// successfully registered the transport — verifying the registration path
// end-to-end against a real transport, not just test fixtures.
func TestPostmark_RegisteredAtPackageLoad(t *testing.T) {
	reg, ok := Lookup("postmark")
	if !ok {
		t.Fatal("postmark not registered after package init")
	}
	if err := reg.Validate(map[string]any{}); err == nil {
		t.Error("postmark Validate should reject empty settings (missing api_key)")
	}
	if err := reg.Validate(map[string]any{"api_key": "x"}); err != nil {
		t.Errorf("postmark Validate rejected valid settings: %v", err)
	}
	tp, err := reg.Build(map[string]any{"api_key": "x"})
	if err != nil {
		t.Fatalf("postmark Build: %v", err)
	}
	if _, ok := tp.(*PostmarkTransport); !ok {
		t.Errorf("postmark Build returned %T, want *PostmarkTransport", tp)
	}
}

// TestPostmark_BuildErrorOnEmptyAPIKey covers the defensive check inside
// buildPostmarkFromSettings — should never fire in practice (Validate
// catches it first) but the Build function is independently safe.
func TestPostmark_BuildErrorOnEmptyAPIKey(t *testing.T) {
	reg, _ := Lookup("postmark")
	_, err := reg.Build(map[string]any{})
	if err == nil {
		t.Fatal("expected error from Build with empty api_key")
	}
}
