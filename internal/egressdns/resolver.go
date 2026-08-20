// Package egressdns implements the DNS proxy backing mvm's allow:<domains>
// network policy.
//
// The policy's filter matches on an ipset that only this proxy writes to: a
// name resolves, and its addresses become reachable, only if the name is on the
// VM's allowlist. That inverts the old design, which resolved each domain once
// with getent at boot and pinned the resulting IPs — a scheme that broke open
// or closed unpredictably as CDNs rotated addresses.
package egressdns

import (
	"fmt"
	"net"
	"os/exec"
	"strconv"
	"strings"
	"sync"

	"github.com/agentstep/mvm/internal/state"
	"github.com/miekg/dns"
)

// minIPSetTTL floors how long a resolved address stays reachable. Some CDNs
// answer with a TTL of a few seconds; expiring the ipset entry that fast would
// break the connection the client is opening with the address it was just
// given.
const minIPSetTTL = 60

// Allowed reports whether qname is covered by the allowlist. A domain covers
// itself and its subdomains, matched on label boundaries: "github.com" covers
// "api.github.com" but not "evilgithub.com".
func Allowed(qname string, domains []string) bool {
	q := strings.ToLower(strings.TrimSuffix(qname, "."))
	if q == "" {
		return false
	}
	for _, d := range domains {
		d = strings.ToLower(strings.TrimSuffix(d, "."))
		if d == "" {
			continue
		}
		if q == d || strings.HasSuffix(q, "."+d) {
			return true
		}
	}
	return false
}

// SetPair is one VM's two allowlist ipsets. They are separate because an
// ipset's address family is fixed at creation: adding an IPv6 address to an
// inet set fails rather than storing it.
type SetPair struct {
	V4 string
	V6 string
}

// Resolver runs one DNS listener per allow-policy VM, bound to that VM's own
// gateway address. The listener a query arrives on identifies the VM, so no
// client-supplied field is ever trusted for authorization.
type Resolver struct {
	upstream string

	mu      sync.RWMutex
	domains map[int][]string      // net index -> allowlist
	sets    map[int]SetPair       // net index -> its family-locked ipsets
	servers map[int][]*dns.Server // net index -> its udp+tcp listeners

	// Injectable for tests.
	exchange func(*dns.Msg, string) (*dns.Msg, error)
	ipsetAdd func(set string, ip net.IP, ttl uint32) error
}

// NewResolver returns a Resolver forwarding to upstream (host:port).
func NewResolver(upstream string) *Resolver {
	c := new(dns.Client)
	return &Resolver{
		upstream: upstream,
		domains:  map[int][]string{},
		sets:     map[int]SetPair{},
		servers:  map[int][]*dns.Server{},
		exchange: func(m *dns.Msg, addr string) (*dns.Msg, error) {
			resp, _, err := c.Exchange(m, addr)
			return resp, err
		},
		ipsetAdd: runIPSetAdd,
	}
}

// Start binds UDP and TCP listeners on the VM's gateway address and registers
// its allowlist. Safe to call again for the same index: the previous listeners
// are stopped first, so a policy change converges.
func (r *Resolver) Start(alloc state.NetAllocation, sets SetPair, domains []string) error {
	// Nil-safe: a Server constructed without a resolver (tests, or a build that
	// never serves allow: policies) must not panic on the lifecycle paths.
	if r == nil {
		return fmt.Errorf("no egress DNS resolver configured")
	}
	r.Stop(alloc.Index)

	r.mu.Lock()
	r.domains[alloc.Index] = domains
	r.sets[alloc.Index] = sets
	r.mu.Unlock()

	addr := net.JoinHostPort(alloc.TAPIP, "53")
	index := alloc.Index
	mux := dns.NewServeMux()
	mux.HandleFunc(".", func(w dns.ResponseWriter, req *dns.Msg) {
		w.WriteMsg(r.handle(index, req))
	})

	var started []*dns.Server
	for _, netProto := range []string{"udp", "tcp"} {
		srv := &dns.Server{Addr: addr, Net: netProto, Handler: mux}
		ready := make(chan error, 1)
		srv.NotifyStartedFunc = func() { ready <- nil }
		go func() {
			if err := srv.ListenAndServe(); err != nil {
				select {
				case ready <- err:
				default:
				}
			}
		}()
		if err := <-ready; err != nil {
			for _, s := range started {
				s.Shutdown()
			}
			r.Stop(index)
			return fmt.Errorf("egress DNS listen on %s/%s: %w", addr, netProto, err)
		}
		started = append(started, srv)
	}

	r.mu.Lock()
	r.servers[index] = started
	r.mu.Unlock()
	return nil
}

// Stop shuts down a VM's listeners and forgets its allowlist. Idempotent, and
// safe on a nil Resolver so teardown paths never need to guard.
func (r *Resolver) Stop(index int) {
	if r == nil {
		return
	}
	r.mu.Lock()
	servers := r.servers[index]
	delete(r.servers, index)
	delete(r.domains, index)
	delete(r.sets, index)
	r.mu.Unlock()

	for _, s := range servers {
		s.Shutdown()
	}
}

// handle answers one query on behalf of the VM at the given net index.
//
// A name off the allowlist is REFUSED without being forwarded: sending it
// upstream would leak the lookup itself, which is exactly the exfiltration
// channel the policy exists to close.
func (r *Resolver) handle(index int, req *dns.Msg) *dns.Msg {
	resp := new(dns.Msg)
	resp.SetReply(req)

	r.mu.RLock()
	domains, known := r.domains[index]
	sets := r.sets[index]
	r.mu.RUnlock()

	if !known || len(req.Question) == 0 {
		resp.Rcode = dns.RcodeRefused
		return resp
	}
	for _, q := range req.Question {
		if !Allowed(q.Name, domains) {
			resp.Rcode = dns.RcodeRefused
			return resp
		}
	}

	upstream, err := r.exchange(req, r.upstream)
	if err != nil || upstream == nil {
		resp.Rcode = dns.RcodeServerFailure
		return resp
	}

	for _, rr := range upstream.Answer {
		var ip net.IP
		// A and AAAA go to different sets: an ipset's family is fixed at
		// creation, and adding an IPv6 address to the inet set fails rather
		// than storing it — so a shared set would leave every AAAA the proxy
		// resolved unreachable, silently.
		target := sets.V4
		switch v := rr.(type) {
		case *dns.A:
			ip = v.A
		case *dns.AAAA:
			ip = v.AAAA
			target = sets.V6
		default:
			continue
		}
		ttl := rr.Header().Ttl
		if ttl < minIPSetTTL {
			ttl = minIPSetTTL
		}
		r.ipsetAdd(target, ip, ttl)
	}

	upstream.Id = req.Id
	return upstream
}

// runIPSetAdd authorizes one address for one VM. The set name comes from
// firecracker.EgressIPSetName and the IP from a parsed DNS record, so neither
// is user-controlled text; exec.Command is used without a shell regardless.
func runIPSetAdd(set string, ip net.IP, ttl uint32) error {
	if ip == nil || set == "" {
		return nil
	}
	if strings.ContainsAny(set, " \t\n;|&$`") {
		return fmt.Errorf("refusing suspicious ipset name %q", set)
	}
	return exec.Command("sudo", "ipset", "add", set, ip.String(),
		"timeout", strconv.FormatUint(uint64(ttl), 10), "-exist").Run()
}
