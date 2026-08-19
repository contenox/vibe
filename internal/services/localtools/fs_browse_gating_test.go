package localtools_test

import (
	"context"
	"testing"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/contenox/contenox/internal/services/localtools"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	"github.com/getkin/kin-openapi/openapi3"
	"github.com/stretchr/testify/require"
)

// toolsetRegistry mirrors what PersistentRepo.Supports hands the allowlist: the
// registered toolset names only, never the individual tool names.
type toolsetRegistry []string

func (p toolsetRegistry) Supports(context.Context) ([]string, error) { return []string(p), nil }

func (p toolsetRegistry) GetSchemasForSupportedTools(context.Context) (map[string]*openapi3.T, error) {
	return nil, nil
}

func (p toolsetRegistry) GetToolsForToolsByName(context.Context, string) ([]taskengine.Tool, error) {
	return nil, nil
}

// TestUnit_LocalFSBrowseTools_AllowlistVocabulary asserts what an operator can express about the browse toolset: "*" admits it, "!name" removes it, a bare name grants exactly it, an empty allowlist grants nothing.
func TestUnit_LocalFSBrowseTools_AllowlistVocabulary(t *testing.T) {
	ctx := context.Background()
	registry := toolsetRegistry{localtools.LocalFSToolsName, localtools.LocalFSBrowseToolsName}

	star, err := taskengine.ExportedResolveToolsNames(ctx, []string{"*"}, registry)
	require.NoError(t, err)
	require.Equal(t, []string{localtools.LocalFSToolsName, localtools.LocalFSBrowseToolsName}, star,
		"\"*\" must admit every connected toolset; the scope is a namespace, not a hidden exclusion")

	removed, err := taskengine.ExportedResolveToolsNames(ctx, []string{"*", "!" + localtools.LocalFSBrowseToolsName}, registry)
	require.NoError(t, err)
	require.Equal(t, []string{localtools.LocalFSToolsName}, removed,
		"\"!\"+the toolset name is how an operator drops exactly this toolset")

	only, err := taskengine.ExportedResolveToolsNames(ctx, []string{localtools.LocalFSBrowseToolsName}, registry)
	require.NoError(t, err)
	require.Equal(t, []string{localtools.LocalFSBrowseToolsName}, only,
		"a bare name grants exactly it")

	none, err := taskengine.ExportedResolveToolsNames(ctx, nil, registry)
	require.NoError(t, err)
	require.Empty(t, none, "an empty allowlist grants nothing")
}

// TestUnit_LocalFSBrowseTools_RegistersUnderTheAddressedName asserts the name the
// allowlist filters on is the same name the toolset reports as its registry key;
// a drift between them would leave the toolset unaddressable.
func TestUnit_LocalFSBrowseTools_RegistersUnderTheAddressedName(t *testing.T) {
	h := localtools.NewLocalFSBrowseTools(t.TempDir(), nil)
	supported, err := h.Supports(context.Background())
	require.NoError(t, err)
	require.Equal(t, localtools.LocalFSBrowseToolsName, supported[0])
	require.Equal(t, []string{"list_dir", "grep", "find_files", "count_stats", "stat_file"}, supported[1:])
}

// TestUnit_LocalFSBrowseTools_NameIsUnmintableByDeclaredMCP asserts the browse
// name cannot collide with a decl- row, which PersistentRepo.Exec would resolve
// to the local toolset first and so silently substitute for a declared server.
func TestUnit_LocalFSBrowseTools_NameIsUnmintableByDeclaredMCP(t *testing.T) {
	require.False(t, runtimetypes.IsDeclaredToolName(localtools.LocalFSBrowseToolsName))
	require.NotEqual(t, localtools.LocalFSToolsName, localtools.LocalFSBrowseToolsName)
}
