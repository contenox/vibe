package contenoxcli

import (
	"context"
	"testing"

	"github.com/contenox/contenox/libtracker"
	"github.com/contenox/contenox/internal/services/gointel"
	"github.com/contenox/contenox/internal/services/gojatool"
	"github.com/contenox/contenox/internal/services/jqtool"
	"github.com/contenox/contenox/internal/services/localtools"
	"github.com/contenox/contenox/internal/services/missionservice"
	"github.com/contenox/contenox/internal/services/missiontools"
	"github.com/contenox/contenox/internal/services/searchtool"
	"github.com/contenox/contenox/internal/surfaces/acpsvc"
	"github.com/stretchr/testify/require"
)

// TestUnit_ACPToolset_CarriesTheCodeIntelligenceToolsets pins the divergence
// part 1 closes: an ACP session (contenox acp/acpx — Zed, JetBrains, OpenClaw)
// must carry the same code-intelligence toolsets `contenox chat`/`run` gets
// via engine.go's localToolset, not just local_fs/webtools/local_shell/print/echo.
func TestUnit_ACPToolset_CarriesTheCodeIntelligenceToolsets(t *testing.T) {
	goIndex := gointel.NewIndex(gointel.Config{})
	t.Cleanup(goIndex.Shutdown)
	gt, err := gojatool.New(gojatool.Config{})
	require.NoError(t, err)
	t.Cleanup(gt.Shutdown)

	noTransport := func() *acpsvc.Transport { return nil }
	tools := acpToolset(nil, libtracker.NoopTracker{}, goIndex, gt, "test-workspace",
		noTransport, nil, missionservice.New(nil), nil, nil)

	// Every provider chat/run already had must still be present.
	for _, name := range []string{"echo", "print", "webtools", "local_fs", "local_shell", missiontools.ToolsProviderName} {
		require.Containsf(t, tools, name, "ACP toolset must keep registering %q", name)
	}

	// The five toolsets that were missing, asserted by Supports(), the same
	// way engine_test.go pins localToolset's composition.
	cases := []struct {
		provider string
		tool     string
	}{
		{gointel.ToolsProviderName, "go_symbols"},
		{searchtool.ToolsProviderName, "workspace_search"},
		{localtools.GitToolsName, "git_status"},
		{jqtool.ToolsProviderName, "jq_query"},
		{gojatool.ToolsProviderName, "goja_eval"},
	}
	for _, tc := range cases {
		repo, ok := tools[tc.provider]
		require.Truef(t, ok, "ACP toolset must register provider %q", tc.provider)
		supported, err := repo.Supports(context.Background())
		require.NoError(t, err)
		require.Containsf(t, supported, tc.tool, "%s must support %s", tc.provider, tc.tool)
	}
}
