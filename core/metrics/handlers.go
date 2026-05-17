package metrics

import (
	"net/http"
)

// HealthzHandler returns an HTTP handler that responds with `200 OK` and
// body `ok` for any GET request. The endpoint is auth-free and
// rate-limit-free; operators concerned about exposure firewall the path
// at the reverse proxy (FR54).
func HealthzHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
}

// Handler returns an HTTP handler that serves the registry's metrics in
// Prometheus text exposition format (FR55). Like /healthz, auth-free
// and rate-limit-free; firewall at the reverse proxy if needed.
//
// The Content-Type is the Prometheus exposition 0.0.4 format, recognized
// by Prometheus servers and promtool.
func (r *Registry) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_ = r.Emit(w)
	})
}
