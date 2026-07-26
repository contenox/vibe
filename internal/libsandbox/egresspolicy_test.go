package libsandbox

import (
	"errors"
	"net"
	"testing"

	"golang.org/x/net/dns/dnsmessage"
)

// These are package-internal, build-tag-free tests: the egress allow-list/DNS
// core is pure Go and compiles on every platform, so its policy is verified
// without a Linux namespace, a TUN, or a netstack — the same portability the
// Linux bridge relies on.

func mkAQuery(t *testing.T, host string) []byte {
	t.Helper()
	name, err := dnsmessage.NewName(host + ".")
	if err != nil {
		t.Fatalf("name %q: %v", host, err)
	}
	msg := dnsmessage.Message{
		Header:    dnsmessage.Header{ID: 1, RecursionDesired: true},
		Questions: []dnsmessage.Question{{Name: name, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET}},
	}
	packed, err := msg.Pack()
	if err != nil {
		t.Fatalf("pack: %v", err)
	}
	return packed
}

func TestUnit_EgressPolicy_AllowResolvesToSynthetic(t *testing.T) {
	p := newEgressPolicy([]NetCarveout{{Host: "Registry.NPMjs.org", Needs: "npm"}})

	resp, d, ok := p.answerDNS(mkAQuery(t, "registry.npmjs.org"))
	if !ok {
		t.Fatal("answerDNS not ok")
	}
	if !d.Allowed {
		t.Fatalf("host should be allowed (case-insensitive match)")
	}
	if d.Host != "registry.npmjs.org" {
		t.Fatalf("decision host = %q", d.Host)
	}
	if !d.HasIP {
		t.Fatal("expected a synthetic A answer")
	}
	// The synthetic address round-trips back to the host it stands for.
	if host, mapped := p.hostForSynth(d.IP); !mapped || host != "registry.npmjs.org" {
		t.Fatalf("hostForSynth(%v) = %q,%v", d.IP, host, mapped)
	}
	// And the answer on the wire carries that same address.
	var m dnsmessage.Message
	if err := m.Unpack(resp); err != nil {
		t.Fatalf("unpack resp: %v", err)
	}
	if m.Header.RCode != dnsmessage.RCodeSuccess {
		t.Fatalf("rcode = %v, want success", m.Header.RCode)
	}
	if len(m.Answers) != 1 {
		t.Fatalf("want 1 answer, got %d", len(m.Answers))
	}
	a, isA := m.Answers[0].Body.(*dnsmessage.AResource)
	if !isA {
		t.Fatalf("answer body type = %T, want *AResource", m.Answers[0].Body)
	}
	if net.IP(a.A[:]).String() != net.IP(d.IP[:]).String() {
		t.Fatalf("wire A %v != decision IP %v", a.A, d.IP)
	}

	// Stable: a second resolve of the same host yields the same address.
	if again, err := p.synthFor("registry.npmjs.org"); err != nil || again != d.IP {
		t.Fatalf("synthFor not stable: %v (err %v) vs %v", again, err, d.IP)
	}
}

// The synthetic-address allocator is bounded to its /16: once the range is full a
// further NEW host is refused with errSynthExhausted (rather than overflowing past
// the range), while an already-minted host keeps resolving.
func TestUnit_EgressPolicy_SyntheticRangeExhausted(t *testing.T) {
	// Two allow-listed hosts: one gets minted, the other is left unminted so it can
	// exercise the exhaustion path once the range is jammed full.
	p := newEgressPolicy([]NetCarveout{
		{Host: "known.example", Needs: "x"},
		{Host: "servfail.example", Needs: "x"},
	})

	// Mint one real address, then jam the counter to the top of the range.
	first, err := p.synthFor("known.example")
	if err != nil {
		t.Fatalf("first mint: %v", err)
	}
	p.counter = egressSynthMax

	// A brand-new host can no longer be allocated.
	if _, err := p.synthFor("overflow.example"); err == nil {
		t.Fatal("expected errSynthExhausted for a new host once the range is full")
	} else if !errors.Is(err, errSynthExhausted) {
		t.Fatalf("want errSynthExhausted, got %v", err)
	}

	// An already-minted host is unaffected by exhaustion.
	if again, err := p.synthFor("known.example"); err != nil || again != first {
		t.Fatalf("already-minted host must keep resolving: %v (err %v)", again, err)
	}

	// answerDNS surfaces exhaustion of an allow-listed-but-unminted host as a
	// SERVFAIL carrying the error, distinct from the NXDOMAIN of a non-carve-out.
	resp, d, ok := p.answerDNS(mkAQuery(t, "servfail.example"))
	if !ok {
		t.Fatal("answerDNS not ok")
	}
	if d.Err == nil {
		t.Fatal("expected decision.Err set on exhaustion")
	}
	var m dnsmessage.Message
	if uerr := m.Unpack(resp); uerr != nil {
		t.Fatalf("unpack: %v", uerr)
	}
	if m.Header.RCode != dnsmessage.RCodeServerFailure {
		t.Fatalf("rcode = %v, want SERVFAIL", m.Header.RCode)
	}
}

func TestUnit_EgressPolicy_DenyIsNXDOMAIN(t *testing.T) {
	p := newEgressPolicy([]NetCarveout{{Host: "registry.npmjs.org", Needs: "npm"}})

	resp, d, ok := p.answerDNS(mkAQuery(t, "evil.example"))
	if !ok {
		t.Fatal("answerDNS not ok")
	}
	if d.Allowed {
		t.Fatal("evil.example must not be allowed")
	}
	var m dnsmessage.Message
	if err := m.Unpack(resp); err != nil {
		t.Fatalf("unpack: %v", err)
	}
	if m.Header.RCode != dnsmessage.RCodeNameError {
		t.Fatalf("rcode = %v, want NXDOMAIN", m.Header.RCode)
	}
	if len(m.Answers) != 0 {
		t.Fatalf("deny must carry no answers, got %d", len(m.Answers))
	}
}

func TestUnit_EgressPolicy_UnresolvedConnectDenied(t *testing.T) {
	p := newEgressPolicy([]NetCarveout{{Host: "registry.npmjs.org", Needs: "npm"}})
	// A literal address the DNS step never handed out maps to no host.
	if host, ok := p.hostForSynth([4]byte{203, 0, 113, 5}); ok {
		t.Fatalf("literal address must not authorize, got host %q", host)
	}
}

// A host-only carve-out (no Ports) is reachable on every port; a carve-out that
// names ports is reachable only on those.
func TestUnit_EgressPolicy_PortRestriction(t *testing.T) {
	p := newEgressPolicy([]NetCarveout{
		{Host: "any.example", Needs: "all ports"},
		{Host: "web.example", Ports: []int{443, 8443}, Needs: "https only"},
	})

	// Host-only: every port allowed.
	if !p.portAllowed("any.example", 22) || !p.portAllowed("any.example", 443) {
		t.Fatal("host-only carve-out must allow every port")
	}
	// Port-scoped: only the named ports.
	if !p.portAllowed("web.example", 443) || !p.portAllowed("web.example", 8443) {
		t.Fatal("declared ports must be allowed")
	}
	if p.portAllowed("web.example", 80) || p.portAllowed("web.example", 22) {
		t.Fatal("a port not in the carve-out's list must be refused")
	}
}

// isPublicEgressIP refuses every SSRF-relevant address class and passes public
// ones — the pure predicate the Linux egress guard is built on.
func TestUnit_isPublicEgressIP(t *testing.T) {
	refused := []string{
		"127.0.0.1", "::1", // loopback
		"169.254.169.254", "fe80::1", // link-local (incl. cloud metadata)
		"10.0.0.1", "172.16.0.1", "192.168.1.1", "fc00::1", // private / ULA
		"0.0.0.0", "::", // unspecified
		"224.0.0.1", "ff02::1", // multicast
	}
	for _, s := range refused {
		if isPublicEgressIP(net.ParseIP(s)) {
			t.Errorf("isPublicEgressIP(%q) = true, want false (SSRF-relevant)", s)
		}
	}
	allowed := []string{"8.8.8.8", "1.1.1.1", "93.184.216.34", "2606:2800:220:1::1"}
	for _, s := range allowed {
		if !isPublicEgressIP(net.ParseIP(s)) {
			t.Errorf("isPublicEgressIP(%q) = false, want true (public)", s)
		}
	}
	if isPublicEgressIP(nil) {
		t.Error("isPublicEgressIP(nil) = true, want false")
	}
}

func TestUnit_MustIP4(t *testing.T) {
	cases := map[string][]byte{
		"10.191.0.1":      {10, 191, 0, 1},
		"10.192.0.0":      {10, 192, 0, 0},
		"255.255.255.255": {255, 255, 255, 255},
		"0.0.0.0":         {0, 0, 0, 0},
		"1.2.3":           {0, 0, 0, 0}, // malformed -> zeros
		"1.2.3.999":       {0, 0, 0, 0}, // out of range -> zeros
		"a.b.c.d":         {0, 0, 0, 0}, // non-numeric -> zeros
	}
	for in, want := range cases {
		got := mustIP4(in)
		if len(got) != 4 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] || got[3] != want[3] {
			t.Errorf("mustIP4(%q) = %v, want %v", in, got, want)
		}
	}
}
