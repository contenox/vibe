package transport

import "testing"

func TestUnit_ProtocolSupported(t *testing.T) {
	cases := []struct {
		name string
		p    int
		want bool
	}{
		{"below min", MinProtocol - 1, false},
		{"at min", MinProtocol, true},
		{"current", ProtocolVersion, true},
		{"above current", ProtocolVersion + 1, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Supported(c.p); got != c.want {
				t.Fatalf("Supported(%d) = %v, want %v", c.p, got, c.want)
			}
		})
	}
}
