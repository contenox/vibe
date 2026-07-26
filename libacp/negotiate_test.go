package libacp_test

import (
	"testing"

	"github.com/contenox/beam/libacp"
	"github.com/stretchr/testify/assert"
)

// TestUnit_NegotiateProtocolVersion pins libacp's min-of-both negotiation and,
// crucially, that it does NOT require exact echo equality. A future refactor
// toward "reject anything != what we sent" must fail this test.
func TestUnit_NegotiateProtocolVersion(t *testing.T) {
	cases := []struct {
		name         string
		theirs, ours int
		want         int
	}{
		{"equal versions", 1, 1, 1},
		{"theirs below ours accepted", 1, 2, 1},
		{"theirs above ours falls back to ours", 3, 1, 1},
		{"theirs above ours falls back to ours (higher ours)", 5, 2, 2},
		{"theirs zero falls back to ours", 0, 1, 1},
		{"theirs negative falls back to ours", -1, 2, 2},
		{"current protocol version round-trips", libacp.ProtocolVersion, libacp.ProtocolVersion, libacp.ProtocolVersion},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, libacp.NegotiateProtocolVersion(tc.theirs, tc.ours))
		})
	}
}

// TestUnit_NegotiateProtocolVersion_AcceptsSupportedNonEqual is the explicit
// anti-regression: a peer that answers a version we can speak but that differs
// from a naive "what we sent" is still accepted, unlike an exact-equality
// check.
func TestUnit_NegotiateProtocolVersion_AcceptsSupportedNonEqual(t *testing.T) {
	// We implement up to version 2; a peer offers version 1. Exact-equality
	// against a requested 2 would reject 1; min-of-both accepts it.
	const ours = 2
	got := libacp.NegotiateProtocolVersion(1, ours)
	assert.Equal(t, 1, got)
	assert.NotEqual(t, ours, got, "a supported non-equal version must be accepted, not rejected")
}
