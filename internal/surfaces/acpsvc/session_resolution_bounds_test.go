package acpsvc

import (
	"context"
	"testing"

	"github.com/contenox/beam/internal/models/llmrepo"
	"github.com/contenox/beam/internal/services/missionservice"
	"github.com/stretchr/testify/require"
)

// The unit side of the envelope's model bound: a dispatched unit's session must
// turn the allowlist it received at session/new into the bound its own resolver
// is held to. This is the last link of the chain — envelope -> dispatcher ->
// `_meta` -> session -> turn context -> llmrepo refusal — and if it breaks, the
// allowlist parses everywhere and stops nothing.
func TestUnit_SessionEntry_BindsEnvelopeAllowlistOntoTheTurnContext(t *testing.T) {
	meta, ok := missionservice.ParseMissionMetaFull(
		missionservice.MarshalMissionMetaBounded("m-1", []string{"gemini-2.5-flash"}, []string{"my-ollama"}),
	)
	require.True(t, ok)

	sess := &sessionEntry{
		MissionID:        meta.MissionID,
		ModelAllowlist:   meta.ModelAllowlist,
		BackendAllowlist: meta.BackendAllowlist,
	}

	bound := llmrepo.ResolutionBoundsFromContext(
		llmrepo.WithResolutionBounds(context.Background(), sess.resolutionBounds()),
	)
	require.Equal(t, []string{"gemini-2.5-flash"}, bound.Models)
	require.Equal(t, []string{"my-ollama"}, bound.Backends)
	require.False(t, bound.IsZero())
}

// An ordinary chat session — and a mission whose envelope declares no allowlist —
// must bind nothing at all, so those turns resolve exactly as they did before
// compute bounds existed.
func TestUnit_SessionEntry_ChatAndUnboundedMissionBindNothing(t *testing.T) {
	for _, sess := range []*sessionEntry{
		{},                 // an ordinary chat session
		{MissionID: "m-1"}, // a mission with no compute allowlist
		nil,                // defensive: no session at all
	} {
		require.True(t, sess.resolutionBounds().IsZero())
		ctx := llmrepo.WithResolutionBounds(context.Background(), sess.resolutionBounds())
		require.True(t, llmrepo.ResolutionBoundsFromContext(ctx).IsZero())
	}
}
