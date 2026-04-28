package gateway

import "time"

// SetRetryDelaysForTest swaps the package-level retry/timeout variables
// to short values for tests, returning a function that restores them.
//
// Compiled only with _test.go files. External (gateway_test package)
// tests use this rather than reaching into unexported state directly.
func SetRetryDelaysForTest(transient, rateLimited, requestTO time.Duration) func() {
	oldT, oldR, oldTo := transientRetryDelay, rateLimitedRetryDelay, requestTimeout
	transientRetryDelay = transient
	rateLimitedRetryDelay = rateLimited
	requestTimeout = requestTO
	return func() {
		transientRetryDelay = oldT
		rateLimitedRetryDelay = oldR
		requestTimeout = oldTo
	}
}
