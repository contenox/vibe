package contenoxcli

import (
	"context"
	"testing"

	"github.com/contenox/contenox/internal/services/fleetservice"
	"github.com/contenox/contenox/internal/services/gojatool"
	"github.com/contenox/contenox/internal/services/localtools"
	"github.com/contenox/contenox/internal/services/missionservice"
	"github.com/contenox/contenox/internal/services/missiontools"
	"github.com/contenox/contenox/internal/services/searchtool"
	"github.com/contenox/contenox/internal/surfaces/acpsvc"
	"github.com/contenox/contenox/libtracker"
	"github.com/stretchr/testify/require"
)

// TestUnit_ACPToolset_CarriesTheSharedToolsets pins the divergence part 1
// closes: an ACP session (contenox acp/acpx — Zed, JetBrains, OpenClaw) must
// carry the same toolsets `contenox chat`/`run` gets via engine.go's
// localToolset, not just local_fs/webtools/local_shell.
func TestUnit_ACPToolset_CarriesTheSharedToolsets(t *testing.T) {
	gt, err := gojatool.New(gojatool.Config{})
	require.NoError(t, err)
	t.Cleanup(gt.Shutdown)

	noTransport := func() *acpsvc.Transport { return nil }
	noFleet := func() fleetservice.Service { return nil }
	tools := acpToolset(nil, libtracker.NoopTracker{}, gt, "test-workspace",
		noTransport, nil, missionservice.New(nil), nil, nil, true, noFleet)

	// Every provider chat/run already had must still be present.
	for _, name := range []string{"webtools", "local_fs", "local_shell", missiontools.ToolsProviderName} {
		require.Containsf(t, tools, name, "ACP toolset must keep registering %q", name)
	}

	// The toolsets that were missing, asserted by Supports(), the same way
	// engine_test.go pins localToolset's composition.
	cases := []struct {
		provider string
		tool     string
	}{
		{searchtool.ToolsProviderName, "workspace_search"},
		{localtools.GitToolsName, "git_status"},
		{gojatool.ToolsProviderName, "goja_eval"},
	}
	for _, tc := range cases {
		repo, ok := tools[tc.provider]
		require.Truef(t, ok, "ACP toolset must register provider %q", tc.provider)
		supported, err := repo.Supports(context.Background())
		require.NoError(t, err)
		require.Containsf(t, supported, tc.tool, "%s must support %s", tc.provider, tc.tool)
	}

	// Without opt-in-beta the goja provider is absent, exactly as localToolset
	// gates it; everything stable stays.
	stable := acpToolset(nil, libtracker.NoopTracker{}, gt, "test-workspace",
		noTransport, nil, missionservice.New(nil), nil, nil, false, noFleet)
	require.NotContains(t, stable, gojatool.ToolsProviderName, "goja is a beta surface, absent without opt-in-beta")
	for _, name := range []string{"webtools", "local_fs", "local_shell",
		searchtool.ToolsProviderName, localtools.GitToolsName, missiontools.ToolsProviderName} {
		require.Containsf(t, stable, name, "stable toolset %q must not be beta-gated", name)
	}
}
