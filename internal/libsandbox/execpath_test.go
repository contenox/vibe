package libsandbox

import (
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func absTestPath(p string) string {
	if runtime.GOOS != "windows" {
		return p
	}
	return `C:` + filepath.FromSlash(p)
}

// canonicalPATH joins the system exec dirs in precedence order, with no profile-derived entry.
func TestUnit_canonicalPATH_IsSystemExecDirsJoined(t *testing.T) {
	require.Equal(t, "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin", canonicalPATH())
	for _, dir := range SystemExecDirs() {
		require.True(t, strings.HasPrefix(dir, "/"), "exec dir %q must be absolute", dir)
	}
}

// The canonical PATH always validates: by construction it is entirely within the exec surface.
func TestUnit_validatePATH_CanonicalIsAlwaysValid(t *testing.T) {
	require.NoError(t, validatePATH(canonicalPATH(), "/h", nil))
}

// confinedPATH keeps only operator PATH entries within the exec surface (order preserved), dropping an uncarved profile dir.
func TestUnit_confinedPATH_FiltersToExecSurface(t *testing.T) {
	fs := []FSCarveout{{Path: "/opt/toolchain", Mode: ModeRO, Needs: "node"}}
	op := "/opt/toolchain/node/bin:/usr/bin:/home/dev/.cargo/bin:/sbin"

	got := confinedPATH(op, "/h", fs)

	require.Equal(t, "/opt/toolchain/node/bin:/usr/bin:/sbin", got)
	require.NoError(t, validatePATH(got, "/h", fs))
}

// A "~"-carve-out resolves against the scoped home; a PATH dir survives only when its home subtree is carved.
func TestUnit_confinedPATH_KeepsCarvedTildeToolchain(t *testing.T) {
	fs := []FSCarveout{{Path: "~/.nvm", Mode: ModeRO, Needs: "node runtime"}}
	op := "/home/dev/.nvm/versions/node/v24.18.0/bin:/usr/bin:/home/dev/.local/bin"

	got := confinedPATH(op, "/home/dev", fs)

	require.Equal(t, "/home/dev/.nvm/versions/node/v24.18.0/bin:/usr/bin", got)
}

// confinedPATH never yields an empty PATH: it falls back to the canonical floor.
func TestUnit_confinedPATH_FallsBackToCanonicalWhenEmpty(t *testing.T) {
	require.Equal(t, canonicalPATH(), confinedPATH("", "/h", nil))
	require.Equal(t, canonicalPATH(), confinedPATH("/opt/nowhere:relative/bin", "/h", nil))
}

// An empty PATH is inert, not an error.
func TestUnit_validatePATH_EmptyIsInert(t *testing.T) {
	require.NoError(t, validatePATH("", "/h", nil))
	require.NoError(t, validatePATH("   ", "/h", nil))
}

// A dir outside the system exec set with no matching carve-out is rejected.
func TestUnit_validatePATH_RejectsUncoveredDir(t *testing.T) {
	err := validatePATH("/usr/bin:/opt/rogue/bin", "/h", nil)
	require.ErrorIs(t, err, ErrInvalidSpec)
	require.Contains(t, err.Error(), "/opt/rogue/bin")
}

// A relative entry, including the empty implicit-cwd element, is rejected.
func TestUnit_validatePATH_RejectsRelativeAndEmptyEntry(t *testing.T) {
	require.ErrorIs(t, validatePATH("/usr/bin:rel/bin", "/h", nil), ErrInvalidSpec)
	require.ErrorIs(t, validatePATH("/usr/bin::/bin", "/h", nil), ErrInvalidSpec)
}

// A dir covered by an FS carve-out (exact or parent-dir subtree) is admitted.
func TestUnit_validatePATH_AllowsCarveoutCoveredDir(t *testing.T) {
	fs := []FSCarveout{{Path: "/opt/tools", Mode: ModeRO, Needs: "toolchain"}}
	require.NoError(t, validatePATH("/opt/tools", "/h", fs))
	require.NoError(t, validatePATH("/opt/tools/bin", "/h", fs))
	require.NoError(t, validatePATH("/usr/bin:/opt/tools/bin", "/h", fs))
}

// A "~"-relative carve-out resolves against the scoped home; without it the same dir is outside the surface.
func TestUnit_validatePATH_ResolvesTildeCarveoutAgainstHome(t *testing.T) {
	home := absTestPath("/scoped/home")
	localBin := absTestPath("/scoped/home/.local/bin")
	fs := []FSCarveout{{Path: "~/.local/bin", Mode: ModeRO, Needs: "user bin"}}
	require.NoError(t, validatePATH(localBin, home, fs))
	require.ErrorIs(t, validatePATH(localBin, home, nil), ErrInvalidSpec)
}

// pathWithin is boundary-aware: a string-prefix sibling ("/usrlocal" vs "/usr") is not within.
func TestUnit_pathWithin_RespectsPathBoundaries(t *testing.T) {
	roots := []string{"/usr"}
	require.True(t, pathWithin("/usr", roots))
	require.True(t, pathWithin("/usr/bin", roots))
	require.False(t, pathWithin("/usrlocal", roots))
	require.False(t, pathWithin("/", roots))
}

// lookupEnv: last occurrence wins, an absent name yields "".
func TestUnit_lookupEnv(t *testing.T) {
	env := []string{"HOME=/h", "PATH=/a", "PATH=/b"}
	require.Equal(t, "/b", lookupEnv(env, "PATH"))
	require.Equal(t, "/h", lookupEnv(env, "HOME"))
	require.Equal(t, "", lookupEnv(env, "MISSING"))
}
