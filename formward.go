package formward

import (
	"net/http"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
)

func init() {
	caddy.RegisterModule(Module{})
}

func (Module) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.handlers.formward",
		New: func() caddy.Module { return new(Module) },
	}
}

func (m *Module) ServeHTTP(w http.ResponseWriter, _ *http.Request, _ caddyhttp.Handler) error {
	w.WriteHeader(http.StatusOK)
	_, err := w.Write([]byte("OK"))
	return err
}

var (
	_ caddy.Module                = (*Module)(nil)
	_ caddy.Provisioner           = (*Module)(nil)
	_ caddy.Validator             = (*Module)(nil)
	_ caddyhttp.MiddlewareHandler = (*Module)(nil)
	_ caddyfile.Unmarshaler       = (*Module)(nil)
)
