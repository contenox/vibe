package version

import (
	"runtime/debug"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestUnit_ProvenanceFromSettings pins the three vcs.* keys and that unknown
// settings are ignored rather than misread.
func TestUnit_ProvenanceFromSettings(t *testing.T) {
	p := provenanceFromSettings([]debug.BuildSetting{
		{Key: "GOOS", Value: "linux"},
		{Key: "vcs.revision", Value: "41b11dd6"},
		{Key: "vcs.modified", Value: "true"},
		{Key: "vcs.time", Value: "2026-08-18T06:57:00Z"},
	})
	require.Equal(t, Provenance{Revision: "41b11dd6", Dirty: true, Time: "2026-08-18T06:57:00Z"}, p)

	p = provenanceFromSettings([]debug.BuildSetting{
		{Key: "vcs.revision", Value: "41b11dd6"},
		{Key: "vcs.modified", Value: "false"},
	})
	require.Equal(t, Provenance{Revision: "41b11dd6"}, p)

	require.Equal(t, Provenance{}, provenanceFromSettings(nil))
}

// TestUnit_ProvenanceString pins the render: a dirty working-tree build says
// so, a clean stamp stays terse, and the zero value renders nothing so release
// output is unchanged.
func TestUnit_ProvenanceString(t *testing.T) {
	tests := []struct {
		name string
		p    Provenance
		want string
	}{
		{name: "zero value renders nothing", p: Provenance{}, want: ""},
		{name: "dirty working tree is named", p: Provenance{Revision: "41b11dd6", Dirty: true, Time: "2026-08-18T06:57:00Z"},
			want: "revision 41b11dd6 (working tree modified), built 2026-08-18T06:57:00Z"},
		{name: "clean stamp stays terse", p: Provenance{Revision: "41b11dd6", Time: "2026-08-18T06:57:00Z"},
			want: "revision 41b11dd6, built 2026-08-18T06:57:00Z"},
		{name: "revision without time", p: Provenance{Revision: "41b11dd6"}, want: "revision 41b11dd6"},
		{name: "dirty without revision", p: Provenance{Dirty: true}, want: "working tree modified"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, tc.p.String())
		})
	}
}
