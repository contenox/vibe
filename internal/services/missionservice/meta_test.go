package missionservice_test

import (
	"encoding/json"
	"testing"

	"github.com/contenox/beam/internal/services/missionservice"
	"github.com/stretchr/testify/require"
)

// The compute allowlists must survive the session/new `_meta` hop intact — that
// hop IS the enforcement path for them (the dispatching process cannot see which
// model a unit picks, so it hands the bound down instead).
func TestUnit_MissionMeta_CarriesComputeAllowlistsAcrossTheWire(t *testing.T) {
	raw := missionservice.MarshalMissionMetaBounded(
		"mission-123",
		[]string{"gemini-2.5-flash", " gpt-5 "},
		[]string{"my-ollama"},
	)
	require.NotNil(t, raw)

	got, ok := missionservice.ParseMissionMetaFull(raw)
	require.True(t, ok)
	require.Equal(t, "mission-123", got.MissionID)
	require.Equal(t, []string{"gemini-2.5-flash", "gpt-5"}, got.ModelAllowlist, "entries arrive trimmed")
	require.Equal(t, []string{"my-ollama"}, got.BackendAllowlist)
}

// An unbounded mission must produce byte-for-byte the `_meta` it produced before
// compute allowlists existed, so nothing changes for every envelope that declares
// none (which is every shipped preset).
func TestUnit_MissionMeta_UnboundedMissionIsWireIdentical(t *testing.T) {
	require.JSONEq(t,
		`{"contenox.mission":{"missionId":"m-1"}}`,
		string(missionservice.MarshalMissionMeta("m-1")),
	)
	require.JSONEq(t,
		string(missionservice.MarshalMissionMeta("m-1")),
		string(missionservice.MarshalMissionMetaBounded("m-1", nil, nil)),
	)
	// Lists that are present but blank are not a bound either.
	require.JSONEq(t,
		string(missionservice.MarshalMissionMeta("m-1")),
		string(missionservice.MarshalMissionMetaBounded("m-1", []string{"", "  "}, []string{})),
	)
}

// Fail-soft, exactly as ParseMissionMeta already promised: unrelated or malformed
// `_meta` reads as "not on a mission" rather than an error, and a mission id with
// no allowlist reads as unbounded.
func TestUnit_MissionMeta_FailsSoftAndDefaultsToUnbounded(t *testing.T) {
	for _, raw := range []json.RawMessage{
		nil,
		json.RawMessage(`{}`),
		json.RawMessage(`{"contenox.agent":"runner"}`),
		json.RawMessage(`not json`),
		json.RawMessage(`{"contenox.mission":{"missionId":"   "}}`),
	} {
		got, ok := missionservice.ParseMissionMetaFull(raw)
		require.False(t, ok)
		require.Empty(t, got.MissionID)
		require.Nil(t, got.ModelAllowlist)
	}

	got, ok := missionservice.ParseMissionMetaFull(json.RawMessage(`{"contenox.mission":{"missionId":"m-9"}}`))
	require.True(t, ok)
	require.Equal(t, "m-9", got.MissionID)
	require.Nil(t, got.ModelAllowlist, "no allowlist on the wire means unbounded, never empty-and-therefore-deny")
	require.Nil(t, got.BackendAllowlist)
}

// ParseMissionMeta keeps its existing contract now that it delegates.
func TestUnit_MissionMeta_LegacyParserStillReportsTheID(t *testing.T) {
	id, ok := missionservice.ParseMissionMeta(
		missionservice.MarshalMissionMetaBounded("m-7", []string{"only-model"}, nil),
	)
	require.True(t, ok)
	require.Equal(t, "m-7", id)
}
