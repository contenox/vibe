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

var errSynthExhausted = errors.New("libsandbox: egress synthetic-address range exhausted")

const egressSynthBase = "10.192.0.0"

const (
	egressSynthPrefix = 16
	egressSynthMax    = (1 << (32 - egressSynthPrefix)) - 1 // 65535
)

type egressPolicy struct {
	allow map[string]bool
	ports map[string][]int

	mu      sync.Mutex
	counter uint32
	host2ip map[string][4]byte
	ip2host map[[4]byte]string
}

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

func (p *egressPolicy) hostForSynth(a [4]byte) (string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	h, ok := p.ip2host[a]
	return h, ok
}

type dnsDecision struct {
	Host    string  // queried name, normalized (lower-case, no trailing dot)
	Type    string  // query type, for telemetry
	Allowed bool    // whether the name is an egress carve-out
	IP      [4]byte // synthetic address handed out (valid when Allowed && A query)
	HasIP   bool
	// Err is non-nil when Host is allow-listed but no synthetic address could
	// be minted; answered SERVFAIL, distinct from NXDOMAIN.
	Err error
}

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
