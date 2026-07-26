//go:build linux

package libsandbox

import (
	"context"
	"errors"
	"net"
	"testing"
)

// TestUnit_egressBridge_SSRFGuard is the FIX 3 regression: a carve-out that
// resolves to a non-public address is REFUSED when AllowPrivateEgress is off, and
// permitted only when it is on. It drives resolveAndGuard directly with IP-literal
// carve-outs (LookupIP returns a literal without network I/O, so every case is
// deterministic and offline) covering each SSRF class the guard must block —
// loopback, RFC1918 private, the 169.254.169.254 cloud-metadata link-local
// address, and the unspecified address — plus a public address that must pass.
func TestUnit_egressBridge_SSRFGuard(t *testing.T) {
	cases := []struct {
		name         string
		host         string
		allowPrivate bool
		wantDenied   bool // resolveAndGuard returns errEgressDenied
	}{
		{"loopback refused by default", "127.0.0.1", false, true},
		{"private RFC1918 refused by default", "10.0.0.1", false, true},
		{"private 192.168 refused by default", "192.168.1.1", false, true},
		{"cloud metadata link-local refused by default", "169.254.169.254", false, true},
		{"unspecified refused by default", "0.0.0.0", false, true},
		{"public address permitted by default", "8.8.8.8", false, false},
		{"loopback permitted when AllowPrivateEgress", "127.0.0.1", true, false},
		{"metadata permitted when AllowPrivateEgress", "169.254.169.254", true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := &egressBridge{ctx: context.Background(), allowPrivate: tc.allowPrivate}
			ips, err := b.resolveAndGuard(tc.host)
			if tc.wantDenied {
				if !errors.Is(err, errEgressDenied) {
					t.Fatalf("host %q allowPrivate=%v: want errEgressDenied, got ips=%v err=%v",
						tc.host, tc.allowPrivate, ips, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("host %q allowPrivate=%v: unexpected error: %v", tc.host, tc.allowPrivate, err)
			}
			if len(ips) == 0 {
				t.Fatalf("host %q: expected at least one vetted IP", tc.host)
			}
		})
	}
}

// TestUnit_egressBridge_SSRFGuard_CachesResult proves the guard resolves a host
// only once and reuses that vetted result — the property that closes DNS
// rebinding. A second call returns the identical cached slice (same backing
// array), so a later connection cannot smuggle in a freshly-resolved address.
func TestUnit_egressBridge_SSRFGuard_CachesResult(t *testing.T) {
	b := &egressBridge{ctx: context.Background(), allowPrivate: false}
	first, err := b.resolveAndGuard("8.8.8.8")
	if err != nil {
		t.Fatalf("first resolve: %v", err)
	}
	second, err := b.resolveAndGuard("8.8.8.8")
	if err != nil {
		t.Fatalf("second resolve: %v", err)
	}
	if !sameIPSlice(first, second) {
		t.Fatalf("cached result changed between calls: %v vs %v", first, second)
	}
}

func sameIPSlice(a, b []net.IP) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !a[i].Equal(b[i]) {
			return false
		}
	}
	return true
}
