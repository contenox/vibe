package acpsvc

import (
	"context"
	"testing"

	libacp "github.com/contenox/beam/libacp"
	"github.com/stretchr/testify/require"
)

// TestUnit_Initialize_AdvertisesTerminalAuth_WithNilEngine pins: Initialize never depends on a built engine.
func TestUnit_Initialize_AdvertisesTerminalAuth_WithNilEngine(t *testing.T) {
	tr := transportWithMeta(`{"terminal-auth":true}`)
	require.Nil(t, tr.deps.Engine, "guards the precondition: this exercises the setup-only (no model) path")

	resp, err := tr.Initialize(context.Background(), libacp.InitializeRequest{
		ProtocolVersion:    libacp.ProtocolVersion,
		ClientCapabilities: libacp.ClientCapabilities{Meta: tr.clientCaps.Meta},
	})
	require.NoError(t, err)

	var hasTerminal bool
	for _, m := range resp.AuthMethods {
		if m.Type == "terminal" {
			hasTerminal = true
		}
	}
	require.True(t, hasTerminal,
		"setup-only mode must still advertise the terminal auth method, or the registry validator fails and a fresh install can never reach setup")
}

// TestUnit_NewSession_SetupOnly_ReturnsActionableError pins: no engine yields an actionable error, not a nil-panic.
func TestUnit_NewSession_SetupOnly_ReturnsActionableError(t *testing.T) {
	tr := transportWithMeta("")
	require.Nil(t, tr.deps.Engine)

	_, err := tr.NewSession(context.Background(), libacp.NewSessionRequest{Cwd: "/tmp"})
	require.Error(t, err)
	var e *libacp.Error
	require.ErrorAs(t, err, &e)
	require.Equal(t, libacp.ErrAuthRequired, e.Code,
		"setup-only must signal auth_required (-32000) so conformant clients offer the advertised auth methods — that IS the setup flow")
	require.Contains(t, err.Error(), "not configured")
}
