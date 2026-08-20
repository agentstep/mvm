package egressdns

import (
	"net"
	"testing"

	"github.com/agentstep/mvm/internal/state"
	"github.com/miekg/dns"
)

// TestAllowedMatchesOnLabelBoundaries is the load-bearing test in this package.
// A naive strings.HasSuffix match would let an attacker register
// evilgithub.com and reach it under an allowlist of github.com.
func TestAllowedMatchesOnLabelBoundaries(t *testing.T) {
	domains := []string{"github.com", "registry.npmjs.org"}

	for _, q := range []string{
		"github.com.", "github.com", "api.github.com.",
		"codeload.github.com.", "GitHub.COM.", "registry.npmjs.org.",
	} {
		if !Allowed(q, domains) {
			t.Errorf("Allowed(%q) = false, want true", q)
		}
	}

	for _, q := range []string{
		"evilgithub.com.", "github.com.evil.com.", "notgithub.com.",
		"github.como.", "npmjs.org.", "xregistry.npmjs.org.", "", ".",
	} {
		if Allowed(q, domains) {
			t.Errorf("Allowed(%q) = true, want false", q)
		}
	}
}

func TestAllowedWithEmptyAllowlistDeniesEverything(t *testing.T) {
	for _, q := range []string{"github.com.", "anything.example."} {
		if Allowed(q, nil) {
			t.Errorf("Allowed(%q) with no allowlist = true, want false", q)
		}
	}
}

func TestHandlerRefusesNamesOffTheAllowlist(t *testing.T) {
	var upstreamCalls int
	r := &Resolver{
		domains: map[int][]string{0: {"github.com"}},
		sets:    map[int]SetPair{0: {V4: "mvm-allow-0", V6: "mvm-allow6-0"}},
		exchange: func(*dns.Msg, string) (*dns.Msg, error) {
			upstreamCalls++
			return nil, nil
		},
		ipsetAdd: func(string, net.IP, uint32) error { return nil },
	}

	q := new(dns.Msg)
	q.SetQuestion("evil.example.", dns.TypeA)
	resp := r.handle(0, q)

	if resp.Rcode != dns.RcodeRefused {
		t.Errorf("Rcode = %v, want REFUSED", dns.RcodeToString[resp.Rcode])
	}
	if upstreamCalls != 0 {
		t.Error("a refused name must never be forwarded upstream — that leaks the query itself")
	}
}

func TestHandlerPopulatesIPSetFromAnswers(t *testing.T) {
	type added struct {
		set string
		ip  net.IP
		ttl uint32
	}
	var got []added

	r := &Resolver{
		domains: map[int][]string{0: {"github.com"}},
		sets:    map[int]SetPair{0: {V4: "mvm-allow-0", V6: "mvm-allow6-0"}},
		exchange: func(m *dns.Msg, _ string) (*dns.Msg, error) {
			resp := new(dns.Msg)
			resp.SetReply(m)
			rr, _ := dns.NewRR("api.github.com. 30 IN A 140.82.114.6")
			resp.Answer = []dns.RR{rr}
			return resp, nil
		},
		ipsetAdd: func(set string, ip net.IP, ttl uint32) error {
			got = append(got, added{set, ip, ttl})
			return nil
		},
	}

	q := new(dns.Msg)
	q.SetQuestion("api.github.com.", dns.TypeA)
	resp := r.handle(0, q)

	if resp.Rcode != dns.RcodeSuccess {
		t.Fatalf("Rcode = %v, want NOERROR", dns.RcodeToString[resp.Rcode])
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 ipset add, got %d", len(got))
	}
	if got[0].set != "mvm-allow-0" {
		t.Errorf("added to %q, want mvm-allow-0", got[0].set)
	}
	if !got[0].ip.Equal(net.ParseIP("140.82.114.6")) {
		t.Errorf("added IP %v, want 140.82.114.6", got[0].ip)
	}
	// A TTL shorter than the floor would expire the ipset entry before the
	// client finished using the address it was just handed.
	if got[0].ttl < minIPSetTTL {
		t.Errorf("ttl = %d, want at least the %d-second floor", got[0].ttl, minIPSetTTL)
	}
}

// TestHandlerRoutesAAAAToTheV6Set pins that an AAAA answer lands in the inet6
// set. Sending it to the inet set would fail at the ipset layer rather than
// store it, leaving every IPv6 address the proxy resolved unreachable — a
// failure invisible until a dual-stack host prefers the v6 route.
func TestHandlerRoutesAAAAToTheV6Set(t *testing.T) {
	var gotSets []string
	r := &Resolver{
		domains: map[int][]string{0: {"github.com"}},
		sets:    map[int]SetPair{0: {V4: "mvm-allow-0", V6: "mvm-allow6-0"}},
		exchange: func(m *dns.Msg, _ string) (*dns.Msg, error) {
			resp := new(dns.Msg)
			resp.SetReply(m)
			a, _ := dns.NewRR("github.com. 300 IN A 140.82.114.6")
			aaaa, _ := dns.NewRR("github.com. 300 IN AAAA 2606:50c0:8000::153")
			resp.Answer = []dns.RR{a, aaaa}
			return resp, nil
		},
		ipsetAdd: func(set string, _ net.IP, _ uint32) error {
			gotSets = append(gotSets, set)
			return nil
		},
	}
	q := new(dns.Msg)
	q.SetQuestion("github.com.", dns.TypeA)
	r.handle(0, q)

	if len(gotSets) != 2 {
		t.Fatalf("got %d ipset adds, want 2 (one per family): %v", len(gotSets), gotSets)
	}
	if gotSets[0] != "mvm-allow-0" {
		t.Errorf("A record went to %q, want mvm-allow-0", gotSets[0])
	}
	if gotSets[1] != "mvm-allow6-0" {
		t.Errorf("AAAA record went to %q, want mvm-allow6-0", gotSets[1])
	}
}

func TestHandlerRefusesUnknownVM(t *testing.T) {
	r := &Resolver{
		domains:  map[int][]string{},
		sets:     map[int]SetPair{},
		exchange: func(*dns.Msg, string) (*dns.Msg, error) { return nil, nil },
		ipsetAdd: func(string, net.IP, uint32) error { return nil },
	}
	q := new(dns.Msg)
	q.SetQuestion("github.com.", dns.TypeA)
	if resp := r.handle(99, q); resp.Rcode != dns.RcodeRefused {
		t.Errorf("Rcode = %v for an unregistered VM, want REFUSED", dns.RcodeToString[resp.Rcode])
	}
}

func TestRunIPSetAddRejectsSuspiciousSetName(t *testing.T) {
	if err := runIPSetAdd("mvm-allow-0; rm -rf /", net.ParseIP("1.2.3.4"), 60); err == nil {
		t.Error("a set name containing shell metacharacters must be refused")
	}
}

// TestNilResolverIsSafe pins that lifecycle paths need no nil guards. Teardown
// runs on shutdown paths that must complete regardless, and a Server built
// without a resolver (as tests do) must not panic there.
func TestNilResolverIsSafe(t *testing.T) {
	var r *Resolver
	r.Stop(0) // must not panic
	if err := r.Start(state.NetAllocation{Index: 0}, SetPair{}, []string{"x.com"}); err == nil {
		t.Error("Start on a nil resolver should report an error, not succeed")
	}
}
