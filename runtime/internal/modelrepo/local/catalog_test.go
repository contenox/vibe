package local

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUnit_LocalCatalog_ListModels_ReturnsFullCapabilities(t *testing.T) {
	dir := t.TempDir()
	modelDir := filepath.Join(dir, "mymodel")
	require.NoError(t, os.MkdirAll(modelDir, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(modelDir, "model.gguf"), []byte("fake"), 0644))

	c := &catalogProvider{dir: dir}
	models, err := c.ListModels(context.Background())
	require.NoError(t, err)
	require.Len(t, models, 1)
	assert.Equal(t, "mymodel", models[0].Name)
	assert.True(t, models[0].CanChat)
	assert.True(t, models[0].CanPrompt)
	assert.True(t, models[0].CanStream)
	assert.True(t, models[0].CanEmbed)
	assert.False(t, models[0].CanThink)
}

func TestUnit_LocalCatalog_ListModels_SkipsEntriesWithoutGGUF(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "nomodel"), 0755))
	modelDir := filepath.Join(dir, "hasmodel")
	require.NoError(t, os.MkdirAll(modelDir, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(modelDir, "model.gguf"), []byte("fake"), 0644))

	c := &catalogProvider{dir: dir}
	models, err := c.ListModels(context.Background())
	require.NoError(t, err)
	require.Len(t, models, 1)
	assert.Equal(t, "hasmodel", models[0].Name)
}

func TestUnit_LocalCatalog_ListModels_EmptyDir(t *testing.T) {
	c := &catalogProvider{dir: t.TempDir()}
	models, err := c.ListModels(context.Background())
	require.NoError(t, err)
	assert.Empty(t, models)
}

func TestUnit_LocalCatalog_ListModels_MissingDir(t *testing.T) {
	c := &catalogProvider{dir: "/nonexistent/path/xyz"}
	_, err := c.ListModels(context.Background())
	assert.Error(t, err)
}

func TestUnit_LocalCatalog_Type(t *testing.T) {
	c := &catalogProvider{dir: "/any"}
	assert.Equal(t, "local", c.Type())
}
