package acpsvc

import (
	"context"
	"testing"

	"github.com/contenox/beam/internal/models/llmrepo"
	"github.com/contenox/beam/internal/services/missionservice"
	"github.com/stretchr/testify/require"
)

// TestUnit_SessionEntry_BindsEnvelopeAllowlistOntoTheTurnContext pins that a
// session's model/backend allowlist reaches the turn's resolution bounds.
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

// TestUnit_SessionEntry_ChatAndUnboundedMissionBindNothing pins that an
// ordinary chat session and an unbounded mission resolve no allowlist at all.
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
