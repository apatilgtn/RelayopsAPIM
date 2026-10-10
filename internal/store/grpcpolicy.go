package store

import (
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"
)

const (
	MaxGRPCRules          = 100
	MaxGRPCMessageBytes   = 64 << 20
	MaxGRPCDescriptorBytes = 4 << 20
)

// Normalize fills defaults.
func (p *GRPCPolicy) Normalize() {
	p.DefaultAction = strings.ToLower(strings.TrimSpace(p.DefaultAction))
	if p.DefaultAction == "" {
		p.DefaultAction = "allow"
	}
	for i := range p.Rules {
		p.Rules[i].Action = strings.ToLower(strings.TrimSpace(p.Rules[i].Action))
	}
}

// Validate reports the first invalid setting. Call Normalize first.
func (p GRPCPolicy) Validate() error {
	if p.DefaultAction != "allow" && p.DefaultAction != "deny" {
		return errors.New("grpc_policy.default_action must be allow or deny")
	}
	if p.MaxMessageSizeBytes < 0 || p.MaxMessageSizeBytes > MaxGRPCMessageBytes {
		return fmt.Errorf("grpc_policy.max_message_size_bytes must be between 0 and %d", MaxGRPCMessageBytes)
	}
	if len(p.Rules) > MaxGRPCRules {
		return fmt.Errorf("grpc_policy.rules supports at most %d rules", MaxGRPCRules)
	}
	for i, r := range p.Rules {
		if r.Action != "allow" && r.Action != "deny" {
			return fmt.Errorf("grpc_policy.rules[%d].action must be allow or deny", i)
		}
		if len(r.Methods) == 0 {
			return fmt.Errorf("grpc_policy.rules[%d].methods must name at least one method or pattern", i)
		}
		for _, m := range r.Methods {
			if _, err := path.Match(m, ""); err != nil || !strings.Contains(m, "/") {
				return fmt.Errorf("grpc_policy.rules[%d].methods has an invalid pattern %q (use package.Service/Method)", i, m)
			}
		}
	}
	return nil
}

// Allows reports whether a caller may call method ("package.Service/Method")
// and, if not, which rule refused it.
func (p GRPCPolicy) Allows(method, consumerID, plan string) (bool, string) {
	for i, r := range p.Rules {
		matched := slices.ContainsFunc(r.Methods, func(pat string) bool { ok, _ := path.Match(pat, method); return ok })
		if !matched {
			continue
		}
		if len(r.Consumers) > 0 && !slices.Contains(r.Consumers, consumerID) {
			continue
		}
		if len(r.Plans) > 0 && !slices.ContainsFunc(r.Plans, func(x string) bool { return strings.EqualFold(x, plan) }) {
			continue
		}
		return r.Action != "deny", fmt.Sprintf("rule %d", i)
	}
	return p.DefaultAction != "deny", "default_action"
}
