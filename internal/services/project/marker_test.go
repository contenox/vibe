package project_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/contenox/beam/internal/services/project"
	"github.com/stretchr/testify/require"
)

func writeMarkerFile(t *testing.T, projectRoot, content string) {
	t.Helper()
	dir := filepath.Join(projectRoot, project.ContenoxDirName)
	require.NoError(t, os.MkdirAll(dir, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(dir, project.MarkerFileName), []byte(content), 0o644))
}

func TestUnit_Marker_ReadsJSON(t *testing.T) {
	root := t.TempDir()
	writeMarkerFile(t, root, `{"id":"abc-123","name":"scratch"}`)
	m, ok := project.ReadFromProjectRoot(root)
	require.True(t, ok)
	require.Equal(t, "abc-123", m.ID)
	require.Equal(t, "scratch", m.Name)
}

func TestUnit_Marker_ReadsLegacyBareUUID(t *testing.T) {
	root := t.TempDir()
	writeMarkerFile(t, root, "0000-legacy-uuid\n") // pre-JSON format
	m, ok := project.ReadFromProjectRoot(root)
	require.True(t, ok)
	require.Equal(t, "0000-legacy-uuid", m.ID, "a bare-UUID marker keeps working — the DB token is stable")
	require.Equal(t, "", m.Name)
}

func TestUnit_Marker_AbsentIsNotOk(t *testing.T) {
	_, ok := project.ReadFromProjectRoot(t.TempDir())
	require.False(t, ok)
}

func TestUnit_DisplayName(t *testing.T) {
	root := t.TempDir()
	require.Equal(t, filepath.Base(root), project.DisplayName(root), "no marker → basename")

	writeMarkerFile(t, root, `{"id":"x","name":"acme-api"}`)
	require.Equal(t, "acme-api", project.DisplayName(root), "named marker → name")

	root2 := t.TempDir()
	writeMarkerFile(t, root2, `{"id":"y"}`) // marker, no name
	require.Equal(t, filepath.Base(root2), project.DisplayName(root2), "unnamed marker → basename")
}

func TestUnit_EnsureInProjectRoot_CreatesWithNameAndStableID(t *testing.T) {
	root := t.TempDir()
	m, err := project.EnsureInProjectRoot(root, "scratch")
	require.NoError(t, err)
	require.NotEmpty(t, m.ID)
	require.Equal(t, "scratch", m.Name)

	// An explicit new name RENAMES (the rename affordance) — but the ID is stable.
	renamed, err := project.EnsureInProjectRoot(root, "renamed")
	require.NoError(t, err)
	require.Equal(t, m.ID, renamed.ID, "ID is stable across ensures")
	require.Equal(t, "renamed", renamed.Name, "an explicit name renames the project")
	require.Equal(t, "renamed", project.DisplayName(root), "the rename persists")

	// An empty name never clears an existing one.
	kept, err := project.EnsureInProjectRoot(root, "")
	require.NoError(t, err)
	require.Equal(t, m.ID, kept.ID)
	require.Equal(t, "renamed", kept.Name, "an empty name keeps the stored name")
}

func TestUnit_MarkerName_NeverInventsAFallback(t *testing.T) {
	require.Equal(t, "", project.MarkerName(t.TempDir()), "no marker → no name")

	unnamed := t.TempDir()
	writeMarkerFile(t, unnamed, `{"id":"x"}`)
	require.Equal(t, "", project.MarkerName(unnamed), "unnamed marker → no name")

	named := t.TempDir()
	writeMarkerFile(t, named, `{"id":"x","name":"acme"}`)
	require.Equal(t, "acme", project.MarkerName(named))
}

func TestUnit_Register_NamesFreshButNeverClobbers(t *testing.T) {
	// Registering an unmarked dir without a name defaults to the basename —
	// registering IS naming.
	fresh := t.TempDir()
	m, err := project.Register(fresh, "")
	require.NoError(t, err)
	require.Equal(t, filepath.Base(fresh), m.Name)

	// Re-registering without a name keeps the chosen name intact.
	named := t.TempDir()
	first, err := project.Register(named, "Chosen Name")
	require.NoError(t, err)
	again, err := project.Register(named, "")
	require.NoError(t, err)
	require.Equal(t, first.ID, again.ID)
	require.Equal(t, "Chosen Name", again.Name, "a nameless re-registration never clobbers a chosen name")

	// An explicit name still renames.
	renamed, err := project.Register(named, "New Name")
	require.NoError(t, err)
	require.Equal(t, first.ID, renamed.ID)
	require.Equal(t, "New Name", renamed.Name)
}

func TestUnit_NormalizeName(t *testing.T) {
	got, err := project.NormalizeName("  My Project  ")
	require.NoError(t, err)
	require.Equal(t, "My Project", got, "names are trimmed")

	got, err = project.NormalizeName("   ")
	require.NoError(t, err)
	require.Equal(t, "", got, "whitespace-only means no name given")

	_, err = project.NormalizeName("two\nlines")
	require.Error(t, err, "control characters are refused — the name renders verbatim in pickers")

	_, err = project.NormalizeName(strings.Repeat("x", project.MaxNameLen+1))
	require.Error(t, err, "over-long names are refused")

	got, err = project.NormalizeName(strings.Repeat("x", project.MaxNameLen))
	require.NoError(t, err)
	require.Len(t, got, project.MaxNameLen)
}

func TestUnit_EnsureInProjectRoot_BackfillsNameOnBareMarker(t *testing.T) {
	root := t.TempDir()
	writeMarkerFile(t, root, "legacy-id") // a bare `contenox init` marker, no name
	m, err := project.EnsureInProjectRoot(root, "scratch")
	require.NoError(t, err)
	require.Equal(t, "legacy-id", m.ID, "the existing token is preserved")
	require.Equal(t, "scratch", m.Name, "registering under a name backfills it")

	// And it now persists as named.
	require.Equal(t, "scratch", project.DisplayName(root))
}
