package ratelimit

// Named trusted-proxies presets (FR58). An operator can include any of
// these names in `trusted_proxies` alongside literal CIDRs:
//
//	trusted_proxies = ["cloudflare", "10.0.0.0/8"]
//
// At parse time (ParsePrefixes), preset names expand into their canonical
// CIDR list. v1.0 ships the Cloudflare preset in full; the AWS / GCP /
// Azure equivalents are non-trivial to keep current and ship empty as
// reserved slots. Operators behind those load balancers should use
// explicit CIDR lists from the provider's published ranges page until a
// future release ships maintained presets.

// Presets maps preset name → list of CIDR strings. Lookup is
// case-sensitive. Adding a preset is a single map entry; no other code
// changes required.
var Presets = map[string][]string{
	// Cloudflare's published edge IPs as of 2024.
	// Source: https://www.cloudflare.com/ips/
	"cloudflare": {
		// IPv4
		"173.245.48.0/20",
		"103.21.244.0/22",
		"103.22.200.0/22",
		"103.31.4.0/22",
		"141.101.64.0/18",
		"108.162.192.0/18",
		"190.93.240.0/20",
		"188.114.96.0/20",
		"197.234.240.0/22",
		"198.41.128.0/17",
		"162.158.0.0/15",
		"104.16.0.0/13",
		"104.24.0.0/14",
		"172.64.0.0/13",
		"131.0.72.0/22",
		// IPv6
		"2400:cb00::/32",
		"2606:4700::/32",
		"2803:f800::/32",
		"2405:b500::/32",
		"2405:8100::/32",
		"2a06:98c0::/29",
		"2c0f:f248::/32",
	},
	// Reserved preset names. v1.0 ships them empty — using them in
	// trusted_proxies is a no-op (they expand to zero CIDRs). Operators
	// behind these load balancers should use explicit CIDR lists from
	// the provider until a future release populates these.
	"aws-elb":          {},
	"gcp-lb":           {},
	"azure-front-door": {},
}

// IsPreset reports whether name is a registered preset (even if its
// CIDR list is empty). Used by ParsePrefixes to distinguish "this is a
// known preset I don't have data for yet" from "this is a typo".
func IsPreset(name string) bool {
	_, ok := Presets[name]
	return ok
}
