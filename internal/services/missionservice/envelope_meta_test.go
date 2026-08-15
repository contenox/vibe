package missionservice_test

import (
	"testing"

	"github.com/contenox/contenox/internal/services/missionservice"
	"github.com/stretchr/testify/require"
)

// TestUnit_MissionMeta_CarriesTheEnvelope pins the field a unit is gated by.
// Without it a spawned unit falls back to its host's policy, so the envelope
// the operator accepted is not the envelope enforced.
func TestUnit_MissionMeta_CarriesTheEnvelope(t *testing.T) {
	raw := missionservice.MarshalMissionMetaBounded("m-1", nil, nil, " hitl-policy-vault.json ")
	require.NotNil(t, raw)

	got, ok := missionservice.ParseMissionMetaFull(raw)
	require.True(t, ok)
	require.Equal(t, "hitl-policy-vault.json", got.HITLPolicyName, "the envelope survives the round trip, trimmed")

	// A mission that named no envelope stays absent rather than empty-string,
	// so a reader can tell "unbounded" from "bounded by nothing".
	bare := missionservice.MarshalMissionMetaBounded("m-2", nil, nil, "")
	require.NotContains(t, string(bare), "hitlPolicyName")
	got, ok = missionservice.ParseMissionMetaFull(bare)
	require.True(t, ok)
	require.Empty(t, got.HITLPolicyName)
}
