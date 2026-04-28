package formward

import (
	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/caddyconfig/httpcaddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
)

func init() {
	httpcaddyfile.RegisterDirective("formward", parseCaddyfile)
	httpcaddyfile.RegisterDirectiveOrder("formward", httpcaddyfile.Before, "file_server")
}

func (m *Module) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	d.Next()
	if !d.NextArg() {
		return d.ArgErr()
	}
	m.Path = d.Val()
	if d.NextArg() {
		return d.ArgErr()
	}
	for d.NextBlock(0) {
		return d.Errf("unrecognized subdirective %q", d.Val())
	}
	return nil
}

func parseCaddyfile(h httpcaddyfile.Helper) ([]httpcaddyfile.ConfigValue, error) {
	var m Module
	if err := m.UnmarshalCaddyfile(h.Dispenser); err != nil {
		return nil, err
	}
	matcherSet := caddy.ModuleMap{
		"path": h.JSON(caddyhttp.MatchPath{m.Path}),
	}
	return h.NewRoute(matcherSet, &m), nil
}
