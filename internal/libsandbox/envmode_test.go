package libsandbox

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// deny-secrets keeps an inherited toolchain variable while stripping a known
// credential and the control plane — the low-breakage default posture.
func TestUnit_EnvPolicyForMode_DenySecrets(t *testing.T) {
	scrub := EnvScrub(ScrubDenySecrets, nil, nil)
	require.NotNil(t, scrub)

	got := scrub([]string{
		"GOCACHE=/c",
		"AWS_SECRET_ACCESS_KEY=shhh",
		"CONTENOX_DB=/x",
		"PATH=/usr/bin",
	})

	require.Equal(t, []string{"GOCACHE=/c", "PATH=/usr/bin"}, got)
}

// strict passes only the safe base set plus an explicit extra allow; an
// un-named toolchain variable that deny-secrets would have kept is absent here.
func TestUnit_EnvPolicyForMode_Strict(t *testing.T) {
	scrub := EnvScrub(ScrubStrict, []string{"GOCACHE"}, nil)
	require.NotNil(t, scrub)

	got := scrub([]string{
		"PATH=/usr/bin",
		"GOCACHE=/c",
		"CARGO_HOME=/h", // not allowed in strict — dropped
		"AWS_SECRET_ACCESS_KEY=shhh",
	})

	require.Equal(t, []string{"GOCACHE=/c", "PATH=/usr/bin"}, got)
}

// off (and any unrecognized mode) is inactive: EnvScrub returns nil so the caller
// leaves the environment untouched, and EnvPolicyForMode reports active=false.
func TestUnit_EnvPolicyForMode_OffAndUnknownAreInactive(t *testing.T) {
	require.Nil(t, EnvScrub(ScrubOff, nil, nil))
	require.Nil(t, EnvScrub("typo-mode", nil, nil))

	_, active := EnvPolicyForMode(ScrubOff, nil, nil)
	require.False(t, active)
	_, active = EnvPolicyForMode("typo-mode", nil, nil)
	require.False(t, active)
}

// The operator's extra deny is layered onto both postures, and the operator's
// extra allow onto strict, without mutating the shared default literals.
func TestUnit_EnvPolicyForMode_ExtraListsLayerIn(t *testing.T) {
	strict, _ := EnvPolicyForMode(ScrubStrict, []string{"FOO_*"}, []string{"BAR"})
	require.Contains(t, strict.Allow, "FOO_*")
	require.Contains(t, strict.Deny, "BAR")
	require.Contains(t, strict.Deny, "CONTENOX_*") // control plane always present

	// A fresh policy is not tainted by the previous call's extras.
	fresh, _ := EnvPolicyForMode(ScrubStrict, nil, nil)
	require.NotContains(t, fresh.Allow, "FOO_*")
}

func TestUnit_ScrubModeValid(t *testing.T) {
	require.True(t, ScrubModeValid(ScrubOff))
	require.True(t, ScrubModeValid(ScrubDenySecrets))
	require.True(t, ScrubModeValid(ScrubStrict))
	require.False(t, ScrubModeValid(""))
	require.False(t, ScrubModeValid("Strict"))
}
