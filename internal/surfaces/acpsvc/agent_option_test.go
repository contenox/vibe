package acpsvc

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/contenox/contenox/internal/services/agentregistryservice"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	libacp "github.com/contenox/contenox/libacp"
	libdb "github.com/contenox/contenox/libdbexec"
	"github.com/stretchr/testify/require"
)

// This file pins the agent config option: the only way a client that speaks
// nothing but ACP learns which agents this machine can run. The desktop shell
// listed them over an Electron IPC bus; a browser reaching the relay has no
// such bus, so the catalogue rides the same SessionConfigOption channel the
// workspace-root picker does, under the same contract.

// agentOptionTransport builds a transport over a real registry database, plus
// a natively-driven session entry, so the option is reached through the same
// driver dispatch a live session uses.
func agentOptionTransport(t *testing.T) (context.Context, *Transport, agentregistryservice.Service, *sessionEntry) {
	t.Helper()
	ctx := context.Background()
	db, err := libdb.NewSQLiteDBManager(ctx, filepath.Join(t.TempDir(), "acp-agent-option.db"), runtimetypes.SchemaSQLite)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })

	tr := &Transport{deps: Deps{DB: db}, defaultProvider: "openai", defaultModel: "gpt-5-mini"}
	return ctx, tr, agentregistryservice.New(db), &sessionEntry{driver: &nativeDriver{t: tr}}
}

// registerChainAgent declares a chain-kind agent, the kind chain-agent
// discovery registers and `contenox agent list` shows.
func registerChainAgent(t *testing.T, ctx context.Context, reg agentregistryservice.Service, name string, enabled bool) {
	t.Helper()
	agent := &runtimetypes.Agent{Name: name, Enabled: enabled}
	require.NoError(t, agent.SetChainConfig(runtimetypes.ChainConfig{
		Path:    absTestPath("/chains/" + name + ".json"),
		ChainID: name,
	}))
	require.NoError(t, reg.Create(ctx, agent))
}

// optionValues returns an option's value strings in wire order.
func optionValues(option libacp.SessionConfigOption) []string {
	values := option.Options.AllValues()
	out := make([]string, 0, len(values))
	for _, v := range values {
		out = append(out, v.Value)
	}
	return out
}

// TestUnit_AgentOptionAbsentWithoutRegistry pins the registry-less surface: a
// process with no database (bare stdio, a setup-only editor) has no catalogue
// at all, so no option is advertised and a client hides its agent picker
// rather than rendering an empty one.
func TestUnit_AgentOptionAbsentWithoutRegistry(t *testing.T) {
	ctx := context.Background()
	tr := &Transport{deps: Deps{}}
	sess := &sessionEntry{driver: &nativeDriver{t: tr}}

	require.Nil(t, tr.agentRegistry(), "no database means no registry to read")

	_, ok := tr.agentConfigOption(ctx, sess)
	require.False(t, ok, "a process with no registry must advertise no agent option")
	require.False(t, hasOption(tr.sessionConfigOptions(ctx, sess), configIDAgent))
	require.False(t, hasOption(tr.workspaceConfigOptions(ctx), configIDAgent))
}

// TestUnit_AgentOptionAbsentWithNoAgentsRegistered pins the "absent, not
// empty" half against a live registry: a machine that has a database but has
// registered nothing offers no picker, since its only entry would be the
// native chain the session already runs.
func TestUnit_AgentOptionAbsentWithNoAgentsRegistered(t *testing.T) {
	ctx, tr, _, sess := agentOptionTransport(t)

	_, ok := tr.agentConfigOption(ctx, sess)
	require.False(t, ok, "an empty registry must yield no agent option")
	require.False(t, hasOption(tr.sessionConfigOptions(ctx, sess), configIDAgent))
	require.False(t, hasOption(tr.workspaceConfigOptions(ctx), configIDAgent))
}

// TestUnit_AgentOptionAbsentWhenEveryAgentIsDisabled pins that disabled is not
// a lesser kind of registered: a registry holding only agents the operator
// took out of service is the same state as an empty one.
func TestUnit_AgentOptionAbsentWhenEveryAgentIsDisabled(t *testing.T) {
	ctx, tr, reg, sess := agentOptionTransport(t)
	registerChainAgent(t, ctx, reg, "retired-reviewer", false)

	_, ok := tr.agentConfigOption(ctx, sess)
	require.False(t, ok, "a registry of only disabled agents must advertise nothing")
}

// TestUnit_AgentOptionListsOnlyEnabledAgents is the discovery contract: the
// machine's enabled agents are offered by name, the native chain is a value a
// person can pick their way back to, and a disabled agent is never offered —
// ResolveForSpawn would refuse it, so a menu entry would be a refusal behind a
// button.
func TestUnit_AgentOptionListsOnlyEnabledAgents(t *testing.T) {
	ctx, tr, reg, sess := agentOptionTransport(t)
	registerChainAgent(t, ctx, reg, "agent-reviewer", true)
	registerChainAgent(t, ctx, reg, "agent-planner", true)
	registerChainAgent(t, ctx, reg, "agent-retired", false)

	option, ok := tr.agentConfigOption(ctx, sess)
	require.True(t, ok, "registered enabled agents must advertise the option")
	require.Equal(t, configIDAgent, option.ID)
	require.Equal(t, configCategoryAgent, option.Category)
	require.Equal(t, configTypeSelect, option.Type)
	require.Equal(t, agentNativeValue, option.CurrentValue,
		"a session bound to no agent reports the native chain")

	require.Equal(t, []string{agentNativeValue, "agent-planner", "agent-reviewer"}, optionValues(option),
		"the native chain leads, then the enabled agents by name; the disabled one is absent")
	require.True(t, configOptionHasValue(option, agentNativeValue),
		"switching back to the native chain must be something a person can pick")
	require.Equal(t, "", agentNativeValue,
		"the native chain's wire value is the empty string, the spelling contenox.agent already uses and the client sends")

	require.True(t, hasOption(tr.sessionConfigOptions(ctx, sess), configIDAgent))
	require.True(t, hasOption(tr.workspaceConfigOptions(ctx), configIDAgent),
		"the picker must also ride the initialize _meta snapshot, where a client chooses before any session exists")
}

// TestUnit_AgentOptionReportsTheSessionsBoundAgent pins the "see what this
// session is running as" half, on both live external paths and after the agent
// is taken out of service — a session already bound must still name its agent
// rather than silently reading as native.
func TestUnit_AgentOptionReportsTheSessionsBoundAgent(t *testing.T) {
	ctx, tr, reg, _ := agentOptionTransport(t)
	registerChainAgent(t, ctx, reg, "agent-reviewer", true)

	bound := &sessionEntry{driver: &externalDriver{t: tr, agentName: "agent-reviewer"}}
	option, ok := tr.agentConfigOption(ctx, bound)
	require.True(t, ok)
	require.Equal(t, "agent-reviewer", option.CurrentValue)
	require.True(t, hasOption(tr.sessionConfigOptions(ctx, bound), configIDAgent),
		"an external session's option set must name the agent it is running as")

	orphan := &sessionEntry{driver: &externalDriver{t: tr, agentName: "agent-vanished"}}
	orphanOption, ok := tr.agentConfigOption(ctx, orphan)
	require.True(t, ok)
	require.Equal(t, "agent-vanished", orphanOption.CurrentValue)
	require.True(t, configOptionHasValue(orphanOption, "agent-vanished"),
		"the bound agent must be a value of its own option, or the select renders no label for it")
}

// TestUnit_SetAgentConfigOptionRefused pins immutability on both drivers. The
// agent is chosen at session/new and a session cannot change what it is
// mid-flight; a silent no-op would read to a client as a switch that took, and
// forwarding the id downstream would ask a foreign agent about an option
// contenox owns.
func TestUnit_SetAgentConfigOptionRefused(t *testing.T) {
	ctx, tr, reg, sess := agentOptionTransport(t)
	registerChainAgent(t, ctx, reg, "agent-reviewer", true)

	for _, value := range []string{"agent-reviewer", agentNativeValue, "nonexistent"} {
		err := tr.setSessionConfigOption(ctx, sess, configIDAgent, value)
		require.Error(t, err, "set_config_option %q on the agent must be refused", value)
		require.ErrorContains(t, err, "cannot be changed after the session starts")
	}

	external := &externalDriver{t: tr, agentName: "agent-reviewer"}
	bound := &sessionEntry{driver: external}
	err := external.SetConfigOption(ctx, bound, configIDAgent, libacp.StringConfigValue("nonexistent"))
	require.Error(t, err, "an external session must refuse the agent option rather than forward it downstream")
	require.ErrorContains(t, err, "cannot be changed after the session starts")
	require.Equal(t, "agent-reviewer", external.AgentName(), "a refused set must not rebind the session")
}

// TestUnit_AgentNativeValueRoundTripsToTheNativePath pins the value contract
// the client half depends on: whatever the picker hands out is what goes back
// on session/new's contenox.agent `_meta`. The native value must resolve to
// the native chain rather than being looked up as an agent name, or picking
// "back to Contenox" would fail with "unknown contenox.agent".
func TestUnit_AgentNativeValueRoundTripsToTheNativePath(t *testing.T) {
	require.Equal(t, "", parseAgentMeta(agentMetaJSON(agentNativeValue)),
		"the native value must select the native chain, not an agent named after it")
	require.Equal(t, "agent-reviewer", parseAgentMeta(agentMetaJSON("agent-reviewer")),
		"a registered name must still bind that agent")
	require.Equal(t, "", parseAgentMeta(nil))
}

// TestUnit_AgentOptionOfferedValuesAreBindable closes the loop between the two
// halves: every value the picker offers is one session/new resolves, and it
// resolves to exactly the agent whose name was picked. This is the assertion
// that would fail if the option ever advertised a name the bind path cannot
// see — a listing built from a second source, or an id shape the `_meta` key
// does not accept.
func TestUnit_AgentOptionOfferedValuesAreBindable(t *testing.T) {
	ctx, tr, reg, sess := agentOptionTransport(t)
	registerChainAgent(t, ctx, reg, "agent-reviewer", true)
	registerChainAgent(t, ctx, reg, "agent-retired", false)

	option, ok := tr.agentConfigOption(ctx, sess)
	require.True(t, ok)

	for _, value := range optionValues(option) {
		name := parseAgentMeta(agentMetaJSON(value))
		if value == agentNativeValue {
			require.Equal(t, "", name)
			continue
		}
		require.Equal(t, value, name)
		agent, err := agentregistryservice.ResolveForSpawn(ctx, reg, name)
		require.NoErrorf(t, err, "the picker offered %q, which session/new cannot bind", value)
		require.True(t, agent.Enabled)
	}

	_, err := agentregistryservice.ResolveForSpawn(ctx, reg, "agent-retired")
	require.ErrorIs(t, err, agentregistryservice.ErrAgentDisabled,
		"the agent left out of the picker is exactly the one the bind path refuses")
}
