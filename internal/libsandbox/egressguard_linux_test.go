//go:build linux

package libsandbox

import (
	"context"
	"errors"
	"net"
	"testing"
)

// TestUnit_egressBridge_SSRFGuard pins that a carve-out resolving to a
// non-public address (loopback, RFC1918 private, cloud-metadata link-local,
// unspecified) is refused unless AllowPrivateEgress is on; a public address
// always passes.
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

// TestUnit_egressBridge_SSRFGuard_CachesResult pins that the guard resolves a
// host only once and reuses the cached result, closing DNS rebinding.
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
