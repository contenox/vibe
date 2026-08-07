package vertex

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestUnit_SanitizeVertexSchema_UnionWithItemsCollapsesToArray pins the
// declaration Vertex refuses outright: a property whose type is the union
// ("string" or "array") carrying its own items. The collapse used to take the
// FIRST non-null member — "string" — while still forwarding items, and Vertex
// answers 400 INVALID_ARGUMENT ("for schema with items, schema type should be
// ARRAY") for the whole request, so one such property disables every tool in
// the turn rather than just itself.
//
// This is local_fs/git's `paths` argument (localtools.gitPathsProp), which is
// why the failure surfaced as every ACP turn on vertex-google burning its
// entire request budget on recovery attempts.
func TestUnit_SanitizeVertexSchema_UnionWithItemsCollapsesToArray(t *testing.T) {
	t.Parallel()

	schema, err := sanitizeVertexSchema(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"paths": map[string]any{
				"type":        []any{"string", "array"},
				"items":       map[string]any{"type": "string"},
				"description": "A path relative to the repository root, or an array of them.",
			},
		},
		"required": []string{"paths"},
	})
	require.NoError(t, err)
	require.NotNil(t, schema)

	paths, ok := schema.Properties["paths"].(map[string]any)
	require.True(t, ok, "paths property survives sanitization")
	require.Equal(t, "array", paths["type"], "a union carrying items must collapse to the array branch")
	require.NotNil(t, paths["items"], "the array branch keeps its items")
}

// TestUnit_SanitizeVertexSchema_UnionWithoutItemsKeepsFirstBranch guards the
// other half: webtools' `body` is a seven-way union with NO items, and must
// keep collapsing to its first member rather than being widened to array by
// the fix above.
func TestUnit_SanitizeVertexSchema_UnionWithoutItemsKeepsFirstBranch(t *testing.T) {
	t.Parallel()

	schema, err := sanitizeVertexSchema(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"body": map[string]any{
				"type":        []any{"string", "number", "integer", "boolean", "object", "array", "null"},
				"description": "Request body.",
			},
		},
	})
	require.NoError(t, err)

	body, ok := schema.Properties["body"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "string", body["type"], "a union without items keeps its first non-null branch")
	require.Equal(t, true, body["nullable"], "a union containing null stays nullable")
	require.Nil(t, body["items"])
}

// TestUnit_SanitizeVertexSchema_StrayItemsOnNonArrayIsDropped covers a plain
// (non-union) property that carries items under a scalar type. items is
// meaningless there and Vertex refuses the declaration for it, so it is
// dropped rather than forwarded.
func TestUnit_SanitizeVertexSchema_StrayItemsOnNonArrayIsDropped(t *testing.T) {
	t.Parallel()

	schema, err := sanitizeVertexSchema(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"name": map[string]any{
				"type":  "string",
				"items": map[string]any{"type": "string"},
			},
		},
	})
	require.NoError(t, err)

	name, ok := schema.Properties["name"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "string", name["type"])
	require.Nil(t, name["items"], "items on a scalar is dropped, not forwarded")
}

// TestUnit_SanitizeVertexSchema_PlainArrayKeepsItems is the control: an
// ordinary array declaration is untouched by either guard.
func TestUnit_SanitizeVertexSchema_PlainArrayKeepsItems(t *testing.T) {
	t.Parallel()

	schema, err := sanitizeVertexSchema(map[string]any{
		"type":  "array",
		"items": map[string]any{"type": "string"},
	})
	require.NoError(t, err)
	require.Equal(t, "array", schema.Type)
	require.NotNil(t, schema.Items)
	require.Equal(t, "string", schema.Items.Type)
}
