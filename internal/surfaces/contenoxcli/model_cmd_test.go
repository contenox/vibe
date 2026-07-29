package contenoxcli

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	libdbexec "github.com/contenox/contenox/libdbexec"
	"github.com/contenox/contenox/internal/models/runtimestate"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	"github.com/stretchr/testify/require"
)

// TestUnit_PrintLiveModels_ShowsVisionAndThinkCapabilities asserts the model-list table carries THINK/VISION columns and marks only vision-capable models.
func TestUnit_PrintLiveModels_ShowsVisionAndThinkCapabilities(t *testing.T) {
	ctx := context.Background()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"gpt-4o"},{"id":"gpt-3.5-turbo"}]}`))
	}))
	defer server.Close()

	path := filepath.Join(t.TempDir(), "model-list.db")
	db, err := libdbexec.NewSQLiteDBManager(ctx, path, runtimetypes.SchemaSQLite)
	require.NoError(t, err)
	defer db.Close()

	store := runtimetypes.New(db.WithoutTransaction())
	require.NoError(t, store.CreateBackend(ctx, &runtimetypes.Backend{
		ID: "openai-backend", Name: "openai", Type: "openai", BaseURL: server.URL,
	}))
	cfg, err := json.Marshal(runtimestate.ProviderConfig{APIKey: "test-key", Type: "openai"})
	require.NoError(t, err)
	require.NoError(t, store.SetKV(ctx, runtimestate.OpenaiKey, cfg))

	var out, errW strings.Builder
	require.NoError(t, printLiveModels(ctx, db, &out, &errW))

	lines := strings.Split(out.String(), "\n")
	require.NotEmpty(t, lines)
	header := strings.Fields(lines[0])
	visionCol := -1
	thinkCol := -1
	for i, h := range header {
		switch h {
		case "VISION":
			visionCol = i
		case "THINK":
			thinkCol = i
		}
	}
	require.GreaterOrEqual(t, visionCol, 0, "model list must carry a VISION column:\n%s", out.String())
	require.GreaterOrEqual(t, thinkCol, 0, "model list must carry a THINK column:\n%s", out.String())

	rowFields := func(model string) []string {
		for _, line := range lines[1:] {
			f := strings.Fields(line)
			if len(f) >= 2 && f[1] == model {
				return f
			}
		}
		return nil
	}
	gpt4o := rowFields("gpt-4o")
	require.NotNil(t, gpt4o, "gpt-4o row missing:\n%s", out.String())
	require.Equal(t, "✓", gpt4o[visionCol], "gpt-4o must show the vision marker")

	textOnly := rowFields("gpt-3.5-turbo")
	require.NotNil(t, textOnly, "gpt-3.5-turbo row missing:\n%s", out.String())
	require.Equal(t, "-", textOnly[visionCol], "a text-only model must not claim vision")
}

func TestUnit_DisplayModelNameStripsGeminiResourcePrefix(t *testing.T) {
	if got := displayModelName("models/gemini-3.1-pro-preview"); got != "gemini-3.1-pro-preview" {
		t.Fatalf("displayModelName stripped = %q", got)
	}
	if got := displayModelName("openai/gpt-5"); got != "openai/gpt-5" {
		t.Fatalf("displayModelName must not strip non-Gemini-looking names: %q", got)
	}
}
