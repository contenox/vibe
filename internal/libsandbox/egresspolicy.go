package libsandbox

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"

	"golang.org/x/net/dns/dnsmessage"
)

// errSynthExhausted marks that the synthetic-address range (egressSynthBase /16)
// is full: the spec declared more distinct egress hosts than the range holds. It
// is a bounded, clear failure — logged and answered SERVFAIL — rather than a
// silent overflow past the /16.
var errSynthExhausted = errors.New("libsandbox: egress synthetic-address range exhausted")

// egresspolicy.go is the OS-portable core of the egress wall: the allow-list
// decision, the synthetic-address bookkeeping, and the DNS wire logic. It holds
// no Linux syscalls, no userspace-stack types, and no I/O — it maps a hostname or
// a destination address to an allow/deny decision and produces a DNS answer.
// The Linux bridge (netbridge_linux.go) wires it to a real TUN and netstack; the
// same core could later drive an advisory HTTPS_PROXY path off Linux without
// change. Keeping it pure is also what makes it unit-testable on any platform.
//
// The enforcement model is allow-by-hostname, connect-by-resolved-address:
//   - DNS resolves ONLY carve-out hostnames, and only to a private synthetic
//     address minted here (never a real one). A name not on the list fails to
//     resolve — the churn of CDN IPs is irrelevant because the agent never learns
//     a real address, only a synthetic token that maps back to the allowed name.
//   - A TCP connection is authorized only if its destination is a synthetic
//     address this policy handed out; the Linux layer then dials the real host the
//     token stands for. A literal-IP connection the agent invents was never handed
//     out, so it maps to nothing and is refused.

// egressSynthBase is the /16 the DNS server draws synthetic per-host addresses
// from. It is disjoint from the device subnet (egressAgentIP/egressGatewayIP) so a
// synthetic token can never alias the agent or the gateway, and it exists only
// inside the agent's namespace, so the range is free to choose.
const egressSynthBase = "10.192.0.0"

// egressSynthPrefix is the prefix length of egressSynthBase, and egressSynthMax is
// the number of distinct hosts the range can mint a synthetic address for:
// base+1 .. base+65535 (the network address base+0 is skipped). synthFor refuses
// past this rather than letting base+counter overflow the /16 into an aliasing
// address in a neighbouring block.
const (
	egressSynthPrefix = 16
	egressSynthMax    = (1 << (32 - egressSynthPrefix)) - 1 // 65535
)

// egressPolicy is the allow-list and its synthetic-address ledger. It is safe for
// concurrent use: the DNS server and the TCP forwarder consult it from different
// goroutines.
type egressPolicy struct {
	allow map[string]bool  // lower-cased carve-out hostnames
	ports map[string][]int // host -> allowed dest ports; absent/empty == all ports

	mu      sync.Mutex
	counter uint32             // next synthetic host offset within egressSynthBase
	host2ip map[string][4]byte // allow-listed host -> its synthetic address
	ip2host map[[4]byte]string // synthetic address -> the host it stands for
}

// newEgressPolicy builds the allow-list from the spec's network carve-outs. An
// empty carve-out host is skipped (validation rejects it upstream, so this is
// belt-and-braces); hosts are lower-cased so the match is case-insensitive, as
// DNS is. A carve-out that names Ports narrows the host to those ports; one that
// names none leaves it reachable on every port (the default).
func newEgressPolicy(carveouts []NetCarveout) *egressPolicy {
	allow := make(map[string]bool, len(carveouts))
	ports := make(map[string][]int, len(carveouts))
	for _, c := range carveouts {
		h := strings.ToLower(strings.TrimSpace(c.Host))
		if h == "" {
			continue
		}
		allow[h] = true
		if len(c.Ports) > 0 {
			ports[h] = append(ports[h], c.Ports...)
		}
	}
	return &egressPolicy{
		allow:   allow,
		ports:   ports,
		host2ip: make(map[string][4]byte),
		ip2host: make(map[[4]byte]string),
	}
}

// portAllowed reports whether a connection to host on port is authorized by the
// carve-out's port list. A host with no declared ports is reachable on every port
// (host-only, the default and the pre-port behaviour); a host with declared ports
// is reachable only on those. host is expected already normalized (lower-cased),
// as hostForSynth returns it.
func (p *egressPolicy) portAllowed(host string, port int) bool {
	ps, ok := p.ports[host]
	if !ok || len(ps) == 0 {
		return true
	}
	for _, allowed := range ps {
		if allowed == port {
			return true
		}
	}
	return false
}

// isPublicEgressIP reports whether ip is a safe *public* egress target — i.e. NOT
// one of the address classes an SSRF pivot aims at. It refuses loopback (127/8,
// ::1), link-local (169.254/16 — which includes the 169.254.169.254 cloud
// metadata endpoint — and fe80::/10), RFC1918 / ULA private (10/8, 172.16/12,
// 192.168/16, fc00::/7), the unspecified address (0.0.0.0, ::), and multicast.
// It is a pure predicate (no I/O), so the Linux egress bridge can gate a resolved
// carve-out on it and it stays unit-testable on any platform.
func isPublicEgressIP(ip net.IP) bool {
	if ip == nil {
		return false
	}
	if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsPrivate() ||
		ip.IsUnspecified() || ip.IsMulticast() {
		return false
	}
	return true
}

// synthFor returns the stable synthetic address for an allow-listed host,
// minting one on first use. The mapping is bidirectional so a later connection to
// the address can be traced back to the host it stands for. It refuses with
// errSynthExhausted once the /16 is full (more distinct hosts than egressSynthMax)
// rather than letting base+counter overflow past the range into an aliasing
// address; an already-minted host keeps working regardless.
func (p *egressPolicy) synthFor(host string) ([4]byte, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if a, ok := p.host2ip[host]; ok {
		return a, nil
	}
	if p.counter >= egressSynthMax {
		return [4]byte{}, fmt.Errorf("%w: %s/%d holds at most %d hosts",
			errSynthExhausted, egressSynthBase, egressSynthPrefix, egressSynthMax)
	}
	p.counter++
	base := binary.BigEndian.Uint32(mustIP4(egressSynthBase))
	var a [4]byte
	binary.BigEndian.PutUint32(a[:], base+p.counter)
	p.host2ip[host] = a
	p.ip2host[a] = host
	return a, nil
}

// hostForSynth reports the allow-listed host a destination address stands for,
// or ("", false) if the address was never handed out — the deny-by-default case
// for a literal-IP or otherwise unresolved connection.
func (p *egressPolicy) hostForSynth(a [4]byte) (string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	h, ok := p.ip2host[a]
	return h, ok
}

// dnsDecision is the outcome of a DNS query, surfaced to the caller so it can log
// the attempt (allow or deny) against the queried name before the answer is sent.
type dnsDecision struct {
	Host    string  // the queried name, normalized (lower-case, no trailing dot)
	Type    string  // the query type, for telemetry
	Allowed bool    // whether the name is an egress carve-out
	IP      [4]byte // the synthetic address handed out (valid when Allowed && A query)
	HasIP   bool
	// Err is non-nil when the name IS an allow-listed carve-out but no synthetic
	// address could be minted for it (the range is exhausted). The caller logs it
	// and the query is answered SERVFAIL; it is distinct from the deny-by-default
	// case (Allowed==false), which is a plain NXDOMAIN.
	Err error
}

// answerDNS parses a single DNS query, decides it against the allow-list, and
// returns the wire-format response plus the decision. Deny-by-default: a name not
// on the list is answered NXDOMAIN. An allow-listed A query is answered with the
// host's synthetic address; any other allow-listed query type is answered
// NOERROR with no records (so a resolver doing A+AAAA in parallel does not treat
// the AAAA as a hard failure and still uses the A). ok is false only when the
// query cannot be parsed, in which case there is nothing to answer.
func (p *egressPolicy) answerDNS(query []byte) (resp []byte, d dnsDecision, ok bool) {
	var parser dnsmessage.Parser
	hdr, err := parser.Start(query)
	if err != nil {
		return nil, dnsDecision{}, false
	}
	q, err := parser.Question()
	if err != nil {
		return nil, dnsDecision{}, false
	}

	name := strings.TrimSuffix(strings.ToLower(q.Name.String()), ".")
	d = dnsDecision{Host: name, Type: q.Type.String(), Allowed: p.allow[name]}

	rh := dnsmessage.Header{
		ID:                 hdr.ID,
		Response:           true,
		OpCode:             hdr.OpCode,
		Authoritative:      true,
		RecursionAvailable: true,
	}

	if !d.Allowed {
		rh.RCode = dnsmessage.RCodeNameError // NXDOMAIN
		return buildDNSResponse(rh, q, nil), d, true
	}

	var answers []dnsmessage.Resource
	if q.Type == dnsmessage.TypeA {
		a, serr := p.synthFor(name)
		if serr != nil {
			// Allow-listed but no address left to mint: answer SERVFAIL and surface
			// the error for logging, rather than overflow the synthetic range.
			d.Err = serr
			rh.RCode = dnsmessage.RCodeServerFailure
			return buildDNSResponse(rh, q, nil), d, true
		}
		d.IP, d.HasIP = a, true
		answers = append(answers, dnsmessage.Resource{
			Header: dnsmessage.ResourceHeader{
				Name:  q.Name,
				Type:  dnsmessage.TypeA,
				Class: dnsmessage.ClassINET,
				TTL:   1,
			},
			Body: &dnsmessage.AResource{A: a},
		})
	}
	return buildDNSResponse(rh, q, answers), d, true
}

// buildDNSResponse serializes a response echoing the question and carrying the
// (possibly empty) answers. A serialization slip yields nil rather than a partial
// message, so the server simply drops the reply and the resolver retries.
func buildDNSResponse(h dnsmessage.Header, q dnsmessage.Question, answers []dnsmessage.Resource) []byte {
	b := dnsmessage.NewBuilder(nil, h)
	b.EnableCompression()
	if err := b.StartQuestions(); err != nil {
		return nil
	}
	if err := b.Question(q); err != nil {
		return nil
	}
	if len(answers) > 0 {
		if err := b.StartAnswers(); err != nil {
			return nil
		}
		for _, ans := range answers {
			if a, okA := ans.Body.(*dnsmessage.AResource); okA {
				if err := b.AResource(ans.Header, *a); err != nil {
					return nil
				}
			}
		}
	}
	msg, err := b.Finish()
	if err != nil {
		return nil
	}
	return msg
}

// mustIP4 parses a dotted-quad known at build time (a package constant) into its
// four bytes. A miss is a programmer error in a constant, not a runtime input, so
// it yields zeros; the constants are covered by test.
func mustIP4(s string) []byte {
	parts := strings.Split(s, ".")
	out := make([]byte, 4)
	if len(parts) != 4 {
		return out
	}
	for i, p := range parts {
		n := 0
		for _, r := range p {
			if r < '0' || r > '9' {
				return []byte{0, 0, 0, 0}
			}
			n = n*10 + int(r-'0')
		}
		if n > 255 {
			return []byte{0, 0, 0, 0}
		}
		out[i] = byte(n)
	}
	return out
}
