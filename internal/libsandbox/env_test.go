package libsandbox

import (
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Only names in allow survive; HOME is forced to the scoped dir; PATH is forced to canonical even though it was allow-copied.
func TestUnit_scrubEnv_OnlyAllowedNamesPass(t *testing.T) {
	parent := []string{
		"PATH=/usr/bin",
		"TERM=xterm",
		"AWS_SECRET_ACCESS_KEY=shhh",
		"HOME=/home/real",
		"CONTENOX_TOKEN=leak",
	}

	got := scrubEnv(parent, []string{"PATH", "TERM"}, nil, "/scoped/home")

	require.Equal(t, []string{
		"HOME=/scoped/home",
		"PATH=" + canonicalPATH(),
		"TERM=xterm",
	}, got)
}

// A variable not in allow appears nowhere in the output, neither name nor value.
func TestUnit_scrubEnv_CredentialVarNotInAllowIsDropped(t *testing.T) {
	parent := []string{"AWS_SECRET_ACCESS_KEY=shhh", "PATH=/usr/bin"}

	got := scrubEnv(parent, []string{"PATH"}, nil, "/h")

	for _, kv := range got {
		require.NotContains(t, kv, "AWS_SECRET_ACCESS_KEY")
		require.NotContains(t, kv, "shhh")
	}
}

// Neither an inherited HOME (even allow-listed) nor an explicit EnvSet HOME can override the scoped home.
func TestUnit_scrubEnv_HomeForcedOverInheritedAndSet(t *testing.T) {
	parent := []string{"HOME=/home/real"}

	got := scrubEnv(parent, []string{"HOME"}, map[string]string{"HOME": "/set/home"}, "/scoped/home")

	require.Contains(t, got, "HOME=/scoped/home")
	require.NotContains(t, got, "HOME=/home/real")
	require.NotContains(t, got, "HOME=/set/home")
}

// set overrides an allow-copied value and can add variables absent from the parent.
func TestUnit_scrubEnv_SetOverridesAllowCopiedAndAddsExtras(t *testing.T) {
	parent := []string{"FOO=parent", "PATH=/usr/bin"}

	got := scrubEnv(parent, []string{"FOO", "PATH"}, map[string]string{"FOO": "explicit", "EXTRA": "x"}, "/h")

	require.Contains(t, got, "FOO=explicit")
	require.NotContains(t, got, "FOO=parent")
	require.Contains(t, got, "EXTRA=x")
	require.Contains(t, got, "PATH="+canonicalPATH())
	require.NotContains(t, got, "PATH=/usr/bin")
}

// An explicit EnvSet PATH is the one sanctioned override of the emulated canonical PATH.
func TestUnit_scrubEnv_EnvSetPathOverridesCanonical(t *testing.T) {
	parent := []string{"PATH=/usr/bin"}

	got := scrubEnv(parent, []string{"PATH"}, map[string]string{"PATH": "/opt/tools/bin"}, "/h")

	require.Contains(t, got, "PATH=/opt/tools/bin")
	require.NotContains(t, got, "PATH="+canonicalPATH())
}

// With nothing allowed or set, the confined process still gets exactly HOME and canonical PATH.
func TestUnit_scrubEnv_EmptyAllowYieldsHomeAndCanonicalPath(t *testing.T) {
	parent := []string{"PATH=/usr/bin", "SECRET=x"}

	got := scrubEnv(parent, nil, nil, "/scoped")

	require.Equal(t, []string{"HOME=/scoped", "PATH=" + canonicalPATH()}, got)
}

// Output is sorted by name and stable across identical calls (pure, deterministic).
func TestUnit_scrubEnv_DeterministicSorted(t *testing.T) {
	parent := []string{"BETA=2", "ALPHA=1", "GAMMA=3"}
	allow := []string{"BETA", "ALPHA", "GAMMA"}

	got := scrubEnv(parent, allow, nil, "/h")

	names := make([]string, len(got))
	for i, kv := range got {
		names[i] = kv[:strings.IndexByte(kv, '=')]
	}
	require.True(t, sort.StringsAreSorted(names), "env names must be sorted: %v", names)
	require.Equal(t, got, scrubEnv(parent, allow, nil, "/h"))
}

// A malformed parent entry (no "=") is dropped.
func TestUnit_scrubEnv_SkipsMalformedParentEntry(t *testing.T) {
	parent := []string{"NOTAKEYVALUE", "PATH=/usr/bin"}

	got := scrubEnv(parent, []string{"PATH"}, nil, "/h")

	require.Equal(t, []string{"HOME=/h", "PATH=" + canonicalPATH()}, got)
}
