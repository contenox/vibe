package modelregistry_test

import (
	"context"
	"testing"

	"github.com/contenox/contenox/runtime/modelregistry"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newCuratedOnly returns a Registry backed by curated entries only (no DB).
func newCuratedOnly() modelregistry.Registry {
	return modelregistry.New(nil)
}

func TestUnit_Registry_ResolveCuratedByExactName(t *testing.T) {
	reg := newCuratedOnly()
	d, err := reg.Resolve(context.Background(), "qwen2.5-1.5b")
	require.NoError(t, err)
	assert.Equal(t, "qwen2.5-1.5b", d.Name)
	assert.True(t, d.Curated)
	assert.NotEmpty(t, d.SourceURL)
}

func TestUnit_Registry_ListIncludesCurated(t *testing.T) {
	reg := newCuratedOnly()
	entries, err := reg.List(context.Background())
	require.NoError(t, err)

	names := make(map[string]bool)
	for _, e := range entries {
		names[e.Name] = true
	}
	assert.True(t, names["qwen2.5-1.5b"])
	assert.True(t, names["qwen2.5-7b"])
	assert.True(t, names["llama3.2-1b"])
	assert.True(t, names["tiny"])
}

func TestUnit_Registry_OptimalForExactCuratedMatch(t *testing.T) {
	reg := newCuratedOnly()
	name, err := reg.OptimalFor(context.Background(), "qwen2.5-1.5b")
	require.NoError(t, err)
	assert.Equal(t, "qwen2.5-1.5b", name)
}

func TestUnit_Registry_OptimalForFamilyMapping(t *testing.T) {
	reg := newCuratedOnly()
	name, err := reg.OptimalFor(context.Background(), "Qwen2.5-1.5B-Instruct")
	require.NoError(t, err)
	assert.Equal(t, "qwen2.5-1.5b", name)
}

func TestUnit_Registry_OptimalForFallbackOnUnknown(t *testing.T) {
	reg := newCuratedOnly()
	name, err := reg.OptimalFor(context.Background(), "totally-unknown-model-xyz")
	require.NoError(t, err)
	assert.Equal(t, "tiny", name) // defaultFallback
}

func TestUnit_Registry_ResolveNotFoundReturnsError(t *testing.T) {
	reg := newCuratedOnly()
	_, err := reg.Resolve(context.Background(), "does-not-exist")
	require.Error(t, err)
	require.ErrorIs(t, err, modelregistry.ErrNotFound)
}
