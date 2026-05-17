package transport

import (
	"fmt"
	"sort"
	"sync"
)

// Registry pattern for transport types. Each transport file (postmark.go,
// resend.go, etc.) calls Register in an init() to advertise itself. The
// config layer queries the registry to dispatch validation and build, so
// adding a new transport requires no edits to config/ or cmd/posthorn/ —
// only a new file in core/transport/.
//
// This is the v1.0 block C generalization of what was originally a
// hardcoded switch on transport.Type (FR4 forward-compat commitment).

// ValidateFunc reports whether settings are well-formed for a transport
// type. Called at config-parse time before the transport is built.
// Returns nil on success or a descriptive error.
type ValidateFunc func(settings map[string]any) error

// BuildFunc constructs a Transport from validated settings. Called at
// gateway construction time, after Validate has passed.
type BuildFunc func(settings map[string]any) (Transport, error)

// Registration is one transport type's contribution to the registry.
type Registration struct {
	// Type is the TOML config value (e.g., "postmark"). Must be unique.
	Type string

	// Validate checks that settings are well-formed. Run at config parse
	// time; receives the raw settings map from the TOML config.
	Validate ValidateFunc

	// Build constructs a Transport instance from validated settings.
	// Called once per endpoint at handler-construction time.
	Build BuildFunc
}

var (
	registryMu sync.RWMutex
	registry   = map[string]Registration{}
)

// Register adds a transport to the registry. Intended to be called from a
// transport file's init() function. Panics if the type is already
// registered (a programmer error — two transport files claiming the same
// type name).
func Register(reg Registration) {
	if reg.Type == "" {
		panic("transport.Register: Type must not be empty")
	}
	if reg.Validate == nil {
		panic("transport.Register: Validate must not be nil (" + reg.Type + ")")
	}
	if reg.Build == nil {
		panic("transport.Register: Build must not be nil (" + reg.Type + ")")
	}
	registryMu.Lock()
	defer registryMu.Unlock()
	if _, exists := registry[reg.Type]; exists {
		panic("transport.Register: duplicate registration for " + reg.Type)
	}
	registry[reg.Type] = reg
}

// Lookup returns the registration for the given type, or (zero, false)
// if no transport with that type is registered.
func Lookup(typ string) (Registration, bool) {
	registryMu.RLock()
	defer registryMu.RUnlock()
	reg, ok := registry[typ]
	return reg, ok
}

// KnownTypes returns the registered transport-type names, sorted for
// stable error messages.
func KnownTypes() []string {
	registryMu.RLock()
	defer registryMu.RUnlock()
	out := make([]string, 0, len(registry))
	for t := range registry {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

// UnknownTypeError builds a consistent error message for unknown
// transport types, listing the registered alternatives.
func UnknownTypeError(typ string) error {
	known := KnownTypes()
	if len(known) == 0 {
		return fmt.Errorf("unknown transport type %q (no transports registered)", typ)
	}
	return fmt.Errorf("unknown transport type %q (known: %v)", typ, known)
}
