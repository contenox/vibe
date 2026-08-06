package libacp_test

import (
	"testing"

	"github.com/contenox/contenox/libacp"
	"github.com/stretchr/testify/assert"
)

// TestUnit_NegotiateProtocolVersion pins min-of-both negotiation, and that it
// does not require exact echo equality.
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

// TestUnit_NegotiateProtocolVersion_AcceptsSupportedNonEqual pins that a
// peer answering a supported but non-equal version is accepted, not rejected.
func TestUnit_NegotiateProtocolVersion_AcceptsSupportedNonEqual(t *testing.T) {
	const ours = 2
	got := libacp.NegotiateProtocolVersion(1, ours)
	assert.Equal(t, 1, got)
	assert.NotEqual(t, ours, got, "a supported non-equal version must be accepted, not rejected")
}
