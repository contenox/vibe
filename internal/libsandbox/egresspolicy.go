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

// errSynthExhausted marks the synthetic-address range (egressSynthBase /16)
// as full: more distinct egress hosts were declared than the range holds.
// Answered as a bounded SERVFAIL rather than overflowing past the /16.
var errSynthExhausted = errors.New("libsandbox: egress synthetic-address range exhausted")

// egresspolicy.go is the OS-portable core of the egress wall — allow-list
// decision, synthetic-address bookkeeping, DNS wire logic — with no Linux
// syscalls, no I/O, so it is unit-testable on any platform. The Linux bridge
// (netbridge_linux.go) wires it to a real TUN and netstack.
//
// Enforcement is allow-by-hostname, connect-by-resolved-address: DNS resolves
// only carve-out hostnames, and only to a private synthetic address minted
// here, never a real one — a name off the list fails to resolve. A TCP
// connection is authorized only if its destination is a synthetic address
// this policy handed out; a literal-IP connection the agent invents was
// never handed out and maps to nothing.

// egressSynthBase is the /16 the DNS server draws synthetic per-host
// addresses from, disjoint from the device subnet so a synthetic token can
// never alias the agent or gateway.
const egressSynthBase = "10.192.0.0"

// egressSynthMax is the number of distinct hosts the range can mint an
// address for (base+1..base+65535); synthFor refuses past this rather than
// overflowing the /16 into an aliasing address.
const (
	egressSynthPrefix = 16
	egressSynthMax    = (1 << (32 - egressSynthPrefix)) - 1 // 65535
)

// egressPolicy is the allow-list and its synthetic-address ledger, safe for
// concurrent use by the DNS server and TCP forwarder goroutines.
type egressPolicy struct {
	allow map[string]bool  // lower-cased carve-out hostnames
	ports map[string][]int // host -> allowed dest ports; absent/empty == all ports

	mu      sync.Mutex
	counter uint32             // next synthetic host offset within egressSynthBase
	host2ip map[string][4]byte // allow-listed host -> its synthetic address
	ip2host map[[4]byte]string // synthetic address -> the host it stands for
}

// newEgressPolicy builds the allow-list from the spec's network carve-outs.
// Hosts are lower-cased (DNS is case-insensitive). A carve-out naming Ports
// narrows the host to those ports; naming none leaves every port reachable.
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

// portAllowed reports whether a connection to host on port is authorized. A
// host with no declared ports is reachable on every port (the default); host
// is expected already normalized (lower-cased).
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

// isPublicEgressIP reports whether ip is a safe public egress target, i.e.
// not one of the address classes an SSRF pivot aims at: loopback, link-local
// (incl. 169.254.169.254 cloud metadata), RFC1918/ULA private, unspecified,
// or multicast. Pure predicate, so the Linux egress bridge can gate a
// resolved carve-out on it.
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
// minting one on first use (bidirectionally, so a later connection traces
// back to the host). Refuses with errSynthExhausted once the /16 is full
// rather than overflowing into an aliasing address.
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

// hostForSynth reports the allow-listed host a destination address stands
// for, or ("", false) if never handed out (a literal-IP or unresolved
// connection — deny by default).
func (p *egressPolicy) hostForSynth(a [4]byte) (string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	h, ok := p.ip2host[a]
	return h, ok
}

// dnsDecision is the outcome of a DNS query, surfaced so the caller can log
// the allow/deny decision before the answer is sent.
type dnsDecision struct {
	Host    string  // queried name, normalized (lower-case, no trailing dot)
	Type    string  // query type, for telemetry
	Allowed bool    // whether the name is an egress carve-out
	IP      [4]byte // synthetic address handed out (valid when Allowed && A query)
	HasIP   bool
	// Err is non-nil when Host is allow-listed but no synthetic address could
	// be minted (range exhausted) — answered SERVFAIL, distinct from the
	// plain NXDOMAIN of Allowed==false.
	Err error
}

// answerDNS parses a single DNS query, decides it against the allow-list, and
// returns the wire-format response plus the decision. Deny-by-default: a name
// not on the list is answered NXDOMAIN. An allow-listed A query gets the
// host's synthetic address; any other allow-listed query type gets NOERROR
// with no records (so a parallel A+AAAA resolver doesn't hard-fail on the
// AAAA). ok is false only when the query cannot be parsed.
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
			// Allow-listed but no address left to mint: SERVFAIL, not overflow.
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

// buildDNSResponse serializes a response echoing the question plus answers.
// A serialization failure yields nil (rather than a partial message), so the
// server drops the reply and the resolver retries.
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

// mustIP4 parses a dotted-quad known at build time (a package constant) into
// its four bytes. A malformed input is a programmer error, not a runtime one,
// so it yields zeros.
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
