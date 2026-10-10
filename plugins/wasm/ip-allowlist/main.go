// Command ip-allowlist admits callers by client address.
//
//	"wasm_config": {"ip-allowlist": {
//	  "allow": ["10.0.0.0/8", "203.0.113.7"],
//	  "deny":  ["10.6.6.0/24"]
//	}}
//
// Deny wins over allow. With no allow list every address not denied is
// admitted. The client address is the gateway's (it honours
// X-Forwarded-For only from RELAYOPS_TRUSTED_PROXY_CIDRS).
package main

import (
	"fmt"
	"net/http"
	"net/netip"
	"strings"

	plugin "github.com/relayops/apim/sdk/wasmplugin"
)

type config struct {
	Allow []string `json:"allow"`
	Deny  []string `json:"deny"`

	allow, deny []netip.Prefix
}

func init() { plugin.Handle(handle) }

func main() {}

func parse(list []string) ([]netip.Prefix, error) {
	out := make([]netip.Prefix, 0, len(list))
	for _, s := range list {
		s = strings.TrimSpace(s)
		if !strings.Contains(s, "/") {
			a, err := netip.ParseAddr(s)
			if err != nil {
				return nil, fmt.Errorf("%q is not an address or CIDR", s)
			}
			out = append(out, netip.PrefixFrom(a.Unmap(), a.Unmap().BitLen()))
			continue
		}
		p, err := netip.ParsePrefix(s)
		if err != nil {
			return nil, fmt.Errorf("%q is not an address or CIDR", s)
		}
		out = append(out, p.Masked())
	}
	return out, nil
}

func build(c *config) (err error) {
	if c.allow, err = parse(c.Allow); err != nil {
		return err
	}
	c.deny, err = parse(c.Deny)
	return err
}

func contains(list []netip.Prefix, a netip.Addr) bool {
	for _, p := range list {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

func handle(r plugin.Request) plugin.Result {
	cfg, err := plugin.Config(r, build)
	if err != nil {
		return plugin.Misconfigured(err)
	}
	addr, err := netip.ParseAddr(r.ClientIP)
	if err != nil {
		return plugin.Deny(http.StatusForbidden, "ip_unknown", "client address could not be determined")
	}
	addr = addr.Unmap()
	if contains(cfg.deny, addr) || (len(cfg.allow) > 0 && !contains(cfg.allow, addr)) {
		return plugin.Deny(http.StatusForbidden, "ip_not_allowed", "client address is not allowed for this API")
	}
	return plugin.Allow()
}
