package libsandbox

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// canonicalPATH joins the system exec dirs in precedence order and carries no
// profile-derived entry — the deterministic value the confined process gets.
func TestUnit_canonicalPATH_IsSystemExecDirsJoined(t *testing.T) {
	require.Equal(t, "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin", canonicalPATH())
	for _, dir := range SystemExecDirs() {
		require.True(t, strings.HasPrefix(dir, "/"), "exec dir %q must be absolute", dir)
	}
}

// The default (canonical) PATH always validates: the emulated value is, by
// construction, entirely within the exec surface.
func TestUnit_validatePATH_CanonicalIsAlwaysValid(t *testing.T) {
	require.NoError(t, validatePATH(canonicalPATH(), "/h", nil))
}

// confinedPATH keeps the operator's PATH entries that lie within the exec surface
// (system dirs or a carve-out subtree), preserving order, and drops the rest —
// including an uncarved profile dir. The kept result is, by construction, valid.
func TestUnit_confinedPATH_FiltersToExecSurface(t *testing.T) {
	fs := []FSCarveout{{Path: "/opt/toolchain", Mode: ModeRO, Needs: "node"}}
	op := "/opt/toolchain/node/bin:/usr/bin:/home/dev/.cargo/bin:/sbin"

	got := confinedPATH(op, "/h", fs)

	require.Equal(t, "/opt/toolchain/node/bin:/usr/bin:/sbin", got)
	require.NoError(t, validatePATH(got, "/h", fs))
}

// A "~"-carve-out resolves against the scoped home, so an operator PATH dir under
// the scoped home survives only when that home subtree is carved — the live nvm case.
func TestUnit_confinedPATH_KeepsCarvedTildeToolchain(t *testing.T) {
	fs := []FSCarveout{{Path: "~/.nvm", Mode: ModeRO, Needs: "node runtime"}}
	op := "/home/naro/.nvm/versions/node/v24.18.0/bin:/usr/bin:/home/naro/.local/bin"

	got := confinedPATH(op, "/home/naro", fs)

	require.Equal(t, "/home/naro/.nvm/versions/node/v24.18.0/bin:/usr/bin", got)
}

// When nothing in the operator PATH is within the surface (or it is empty),
// confinedPATH falls back to the canonical floor so the process is never handed an
// empty PATH.
func TestUnit_confinedPATH_FallsBackToCanonicalWhenEmpty(t *testing.T) {
	require.Equal(t, canonicalPATH(), confinedPATH("", "/h", nil))
	require.Equal(t, canonicalPATH(), confinedPATH("/opt/nowhere:relative/bin", "/h", nil))
}

// An empty PATH is inert — nothing to run by bare name — and is not an error.
func TestUnit_validatePATH_EmptyIsInert(t *testing.T) {
	require.NoError(t, validatePATH("", "/h", nil))
	require.NoError(t, validatePATH("   ", "/h", nil))
}

// A dir outside the system exec set with no carve-out is rejected: the profile
// leak (~/.cargo/bin and friends) the emulation exists to remove.
func TestUnit_validatePATH_RejectsUncoveredDir(t *testing.T) {
	err := validatePATH("/usr/bin:/opt/rogue/bin", "/h", nil)
	require.ErrorIs(t, err, ErrInvalidSpec)
	require.Contains(t, err.Error(), "/opt/rogue/bin")
}

// A relative entry (including the empty implicit-cwd element) is rejected — a
// bare-name exec of whatever the process's cwd happens to be.
func TestUnit_validatePATH_RejectsRelativeAndEmptyEntry(t *testing.T) {
	require.ErrorIs(t, validatePATH("/usr/bin:rel/bin", "/h", nil), ErrInvalidSpec)
	require.ErrorIs(t, validatePATH("/usr/bin::/bin", "/h", nil), ErrInvalidSpec)
}

// A dir covered by an FS carve-out is admitted — the sanctioned widening: a
// toolchain dir on PATH only when it is also granted through the wall. Both an
// exact carve-out and a parent-dir carve-out (subtree) count.
func TestUnit_validatePATH_AllowsCarveoutCoveredDir(t *testing.T) {
	fs := []FSCarveout{{Path: "/opt/tools", Mode: ModeRO, Needs: "toolchain"}}
	require.NoError(t, validatePATH("/opt/tools", "/h", fs))
	require.NoError(t, validatePATH("/opt/tools/bin", "/h", fs))
	require.NoError(t, validatePATH("/usr/bin:/opt/tools/bin", "/h", fs))
}

// A "~"-relative carve-out resolves against the scoped home, so a PATH dir under
// the scoped home is admitted only when the carve-out names it.
func TestUnit_validatePATH_ResolvesTildeCarveoutAgainstHome(t *testing.T) {
	fs := []FSCarveout{{Path: "~/.local/bin", Mode: ModeRO, Needs: "user bin"}}
	require.NoError(t, validatePATH("/scoped/home/.local/bin", "/scoped/home", fs))
	// Without the carve-out the same dir is outside the surface.
	require.ErrorIs(t, validatePATH("/scoped/home/.local/bin", "/scoped/home", nil), ErrInvalidSpec)
}

// pathWithin is a boundary-aware subtree check: a sibling that merely shares a
// string prefix ("/usrlocal" vs "/usr") is NOT within.
func TestUnit_pathWithin_RespectsPathBoundaries(t *testing.T) {
	roots := []string{"/usr"}
	require.True(t, pathWithin("/usr", roots))
	require.True(t, pathWithin("/usr/bin", roots))
	require.False(t, pathWithin("/usrlocal", roots))
	require.False(t, pathWithin("/", roots))
}

// lookupEnv reads a value out of a KEY=VALUE slice; last occurrence wins and an
// absent name yields "".
func TestUnit_lookupEnv(t *testing.T) {
	env := []string{"HOME=/h", "PATH=/a", "PATH=/b"}
	require.Equal(t, "/b", lookupEnv(env, "PATH"))
	require.Equal(t, "/h", lookupEnv(env, "HOME"))
	require.Equal(t, "", lookupEnv(env, "MISSING"))
}
