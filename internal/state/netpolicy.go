package state

import (
	"fmt"
	"strings"
)

// NetPolicyMode is the enforcement mode for a VM's outbound traffic.
type NetPolicyMode int

const (
	// NetPolicyOpen applies no filter at all.
	NetPolicyOpen NetPolicyMode = iota
	// NetPolicyDeny drops every packet the guest originates. Unlike the old
	// in-guest implementation this includes DNS: a resolver the guest can
	// still reach is an exfiltration channel, so "deny" means deny.
	NetPolicyDeny
	// NetPolicyAllow drops everything except traffic to addresses that the
	// egress DNS proxy resolved for an allowlisted domain.
	NetPolicyAllow
)

func (m NetPolicyMode) String() string {
	switch m {
	case NetPolicyOpen:
		return "open"
	case NetPolicyDeny:
		return "deny"
	case NetPolicyAllow:
		return "allow"
	}
	return "unknown"
}

// ParsedNetPolicy is a validated network policy. Construct it only via
// ParseNetPolicy — downstream code interpolates the result into privileged
// commands and relies on the validation having happened.
type ParsedNetPolicy struct {
	Mode    NetPolicyMode
	Domains []string // lowercased, syntactically valid; non-empty iff Mode == NetPolicyAllow
}

// ParseNetPolicy validates the wire format used by --net-policy and the
// daemon's CreateVMRequest.NetPolicy field: "", "open", "deny", or
// "allow:<comma-separated domains>".
func ParseNetPolicy(s string) (ParsedNetPolicy, error) {
	switch {
	case s == "" || s == "open":
		return ParsedNetPolicy{Mode: NetPolicyOpen}, nil
	case s == "deny":
		return ParsedNetPolicy{Mode: NetPolicyDeny}, nil
	case strings.HasPrefix(s, "allow:"):
		var domains []string
		for _, d := range strings.Split(strings.TrimPrefix(s, "allow:"), ",") {
			d = strings.TrimSpace(d)
			if d == "" {
				continue
			}
			if !ValidDomain(d) {
				return ParsedNetPolicy{}, fmt.Errorf("invalid domain %q in network policy", d)
			}
			domains = append(domains, strings.ToLower(d))
		}
		if len(domains) == 0 {
			return ParsedNetPolicy{}, fmt.Errorf("network policy %q lists no domains", s)
		}
		return ParsedNetPolicy{Mode: NetPolicyAllow, Domains: domains}, nil
	default:
		return ParsedNetPolicy{}, fmt.Errorf("unknown network policy %q (want open, deny, or allow:<domains>)", s)
	}
}

// ValidDomain reports whether s is a DNS name safe to hand to privileged
// tooling. Deliberately stricter than RFC 1035: ASCII letters, digits,
// hyphen and dot only, so no shell metacharacter, whitespace or newline can
// survive parsing. Underscores are refused too — they are legal in some
// records but never in a name a user would allowlist for egress.
func ValidDomain(s string) bool {
	if s == "" || len(s) > 253 {
		return false
	}
	for _, label := range strings.Split(s, ".") {
		if label == "" || len(label) > 63 {
			return false
		}
		if label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for i := 0; i < len(label); i++ {
			c := label[i]
			switch {
			case c >= 'a' && c <= 'z':
			case c >= 'A' && c <= 'Z':
			case c >= '0' && c <= '9':
			case c == '-':
			default:
				return false
			}
		}
	}
	return true
}

// EgressHostPackages lists what the Firecracker host (the Lima VM) needs for
// host-side egress enforcement. iptables is already required for NAT; ipset
// backs allow: policies and conntrack provides the ESTABLISHED,RELATED match.
func EgressHostPackages() []string {
	return []string{"iptables", "ipset", "conntrack"}
}
