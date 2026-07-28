package contenoxcli

import (
	"strings"
	"testing"

	"github.com/contenox/beam/internal/models/runtimestate"
	"github.com/stretchr/testify/require"
)

// TestUnit_VisionSummary asserts doctor's vision line lists vision-capable models, flags a text-only default, and stays silent with no reachable backend.
func TestUnit_VisionSummary(t *testing.T) {
	state := map[string]runtimestate.BackendRuntimeState{
		"b1": {
			ID: "b1", Name: "openai",
			PulledModels: []runtimestate.ModelPullStatus{
				{Model: "gpt-4o", CanChat: true, CanVision: true},
				{Model: "qwen3-4b", CanChat: true},
			},
		},
		"b2": {
			ID: "b2", Name: "broken", Error: "connection refused",
			PulledModels: []runtimestate.ModelPullStatus{
				{Model: "ghost-vlm", CanChat: true, CanVision: true},
			},
		},
	}

	t.Run("lists vision models and flags a text-only default", func(t *testing.T) {
		v := visionSummaryFromState(state, "qwen3-4b")
		require.True(t, v.reachable)
		require.Equal(t, []string{"gpt-4o"}, v.visionModels, "models on erroring backends must not count")
		require.True(t, v.defaultKnown)
		require.False(t, v.defaultHasVision)

		var out strings.Builder
		printVisionSummary(&out, v)
		require.Contains(t, out.String(), "1 model(s) accept images")
		require.Contains(t, out.String(), "gpt-4o")
		require.Contains(t, out.String(), "default model is text-only")
	})

	t.Run("vision-capable default gets no warning", func(t *testing.T) {
		v := visionSummaryFromState(state, "gpt-4o")
		var out strings.Builder
		printVisionSummary(&out, v)
		require.NotContains(t, out.String(), "text-only")
	})

	t.Run("no vision models teaches the refusal", func(t *testing.T) {
		v := visionSummaryFromState(map[string]runtimestate.BackendRuntimeState{
			"b1": {ID: "b1", PulledModels: []runtimestate.ModelPullStatus{{Model: "qwen3-4b", CanChat: true}}},
		}, "")
		var out strings.Builder
		printVisionSummary(&out, v)
		require.Contains(t, out.String(), "requests with images will be refused")
	})

	t.Run("no reachable backend prints nothing", func(t *testing.T) {
		v := visionSummaryFromState(map[string]runtimestate.BackendRuntimeState{
			"b2": {ID: "b2", Error: "down"},
		}, "")
		var out strings.Builder
		printVisionSummary(&out, v)
		require.Empty(t, out.String())
	})
}
