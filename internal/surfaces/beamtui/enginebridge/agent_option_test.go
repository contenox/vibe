package enginebridge

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/services/agentregistryservice"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	libacp "github.com/contenox/contenox/libacp"
	libdb "github.com/contenox/contenox/libdbexec"
	"github.com/stretchr/testify/require"
)

// This file asserts the wiring, not the behaviour: acpsvc's own tests already
// prove the agent select is built correctly from a registry, and every one of
// them would stay green if this package dropped the database on the way into
// acpsvc.Deps. The Bridge is the loopback beam serves its own terminal from
// AND the factory every relay attachment is minted from, so a browser reaching
// this machine discovers its agents through exactly this construction path or
// not at all.

// registerBridgeAgent declares a chain-kind agent straight into the harness's
// database — the same rows chain-agent discovery writes and `contenox agent
// list` reads.
func registerBridgeAgent(t *testing.T, ctx context.Context, db libdb.DBManager, dir, name string, enabled bool) {
	t.Helper()
	agent := &runtimetypes.Agent{Name: name, Enabled: enabled}
	require.NoError(t, agent.SetChainConfig(runtimetypes.ChainConfig{
		Path:    filepath.Join(dir, name+".json"),
		ChainID: name,
	}))
	require.NoError(t, agentregistryservice.New(db).Create(ctx, agent))
}

// TestUnit_Deps_ACPDepsCarriesTheAgentRegistry pins the one struct copy
// between a composition root that opened a database and a client that can see
// which agents this machine runs. acpsvc builds the registry from Deps.DB, so
// a forgotten field here leaves the agent picker dark on the only surface a
// browser can reach.
//
// The absent case is an explicit == nil rather than require.Nil, which passes
// on a typed nil in an interface field — precisely the value acpsvc would then
// treat as a live database and query through.
func TestUnit_Deps_ACPDepsCarriesTheAgentRegistry(t *testing.T) {
	h := newHarness(t)

	forwarded := Deps{DB: h.db}.acpDeps().DB
	require.True(t, forwarded == h.db, "the database the agent registry is read from must reach acpsvc")

	seam := Deps{}.acpDeps().DB
	require.True(t, seam == nil, "an unwired database must arrive as a nil interface, not a typed nil")
}

// TestUnit_Bridge_SessionAdvertisesTheMachinesAgents drives the real
// construction path end to end: Deps -> acpDeps -> acpsvc.New -> the live
// loopback, then a real session/new whose response reaches this package as a
// ConfigOptionUpdated event. It fails if the option is never built, never
// forwarded, or built from a registry this surface does not share.
func TestUnit_Bridge_SessionAdvertisesTheMachinesAgents(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	registerBridgeAgent(t, ctx, h.db, h.dir, "agent-reviewer", true)
	registerBridgeAgent(t, ctx, h.db, h.dir, "agent-retired", false)

	h.initSession(ctx)

	events := h.collect(5*time.Second, func(ev Event) bool {
		_, ok := ev.(ConfigOptionUpdated)
		return ok
	})
	opts, ok := firstOfType[ConfigOptionUpdated](events)
	require.True(t, ok, "session/new's config options never reached the event stream")

	var agentOption libacp.SessionConfigOption
	for _, o := range opts.Options {
		if o.ID == "agent" {
			agentOption = o
		}
	}
	require.Equal(t, "agent", agentOption.ID,
		"a machine with registered agents must advertise the agent select through the surface a browser reaches")

	values := make([]string, 0, 3)
	for _, v := range agentOption.Options.AllValues() {
		values = append(values, v.Value)
	}
	require.Contains(t, values, "agent-reviewer", "an enabled agent must be offered")
	require.NotContains(t, values, "agent-retired", "a disabled agent must never be offered")
	require.Len(t, values, 2, "the native chain plus the one enabled agent, and nothing else")
	require.Equal(t, values[0], agentOption.CurrentValue,
		"a session bound to no agent reports the native chain, which leads the select and must be pickable")
}
