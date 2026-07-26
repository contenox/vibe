package modelrepo

import "testing"

func TestUnit_ClampMaxOutputTokens(t *testing.T) {
	tests := []struct {
		name      string
		requested int
		ceiling   int
		want      int
		clamped   bool
	}{
		{name: "unknown ceiling", requested: 9000, ceiling: 0, want: 9000},
		{name: "below ceiling", requested: 1024, ceiling: 4096, want: 1024},
		{name: "above ceiling", requested: 9000, ceiling: 4096, want: 4096, clamped: true},
		{name: "zero request", requested: 0, ceiling: 4096, want: 0},
		{name: "negative sentinel", requested: -1, ceiling: 4096, want: -1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, clamped := ClampMaxOutputTokens(tc.requested, tc.ceiling)
			if got != tc.want || clamped != tc.clamped {
				t.Fatalf("ClampMaxOutputTokens(%d, %d) = (%d, %v), want (%d, %v)",
					tc.requested, tc.ceiling, got, clamped, tc.want, tc.clamped)
			}
		})
	}
}
