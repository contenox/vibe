package runtimetypes_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/store/runtimetypes"
	libdb "github.com/contenox/contenox/libdbexec"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func newExternalACPAgent(name string) *runtimetypes.Agent {
	agent := &runtimetypes.Agent{
		ID:      uuid.New().String(),
		Name:    name,
		Enabled: true,
	}
	must(agent.SetExternalACPConfig(runtimetypes.ExternalACPConfig{
		Transport: runtimetypes.ExternalACPTransportStdio,
		Command:   "my-acp-agent",
		Args:      []string{"--flag"},
		Env:       map[string]string{"FOO": "bar"},
		Cwd:       "/workspace",
	}))
	return agent
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}

func TestUnit_Agents_CreateAndGet(t *testing.T) {
	ctx, s := runtimetypes.SetupStore(t)

	agent := newExternalACPAgent("create-and-get")
	require.NoError(t, s.CreateAgent(ctx, agent))

	got, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	require.Equal(t, agent.ID, got.ID)
	require.Equal(t, agent.Name, got.Name)
	require.Equal(t, runtimetypes.AgentKindExternalACP, got.Kind)
	require.True(t, got.Enabled)
	require.WithinDuration(t, time.Now().UTC(), got.CreatedAt, 2*time.Second)
	require.WithinDuration(t, time.Now().UTC(), got.UpdatedAt, 2*time.Second)

	byName, err := s.GetAgentByName(ctx, agent.Name)
	require.NoError(t, err)
	require.Equal(t, agent.ID, byName.ID)
}

func TestUnit_Agents_ConfigJSONRoundTrip(t *testing.T) {
	ctx, s := runtimetypes.SetupStore(t)

	agent := newExternalACPAgent("config-roundtrip")
	require.NoError(t, s.CreateAgent(ctx, agent))

	got, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)

	cfg, err := got.ExternalACPConfig()
	require.NoError(t, err)
	require.Equal(t, "stdio", cfg.Transport)
	require.Equal(t, "my-acp-agent", cfg.Command)
	require.Equal(t, []string{"--flag"}, cfg.Args)
	require.Equal(t, map[string]string{"FOO": "bar"}, cfg.Env)
	require.Equal(t, "/workspace", cfg.Cwd)
}

func TestUnit_Agents_ReservedSeams_HarnessAndWorkspace(t *testing.T) {
	ctx, s := runtimetypes.SetupStore(t)

	agent := newExternalACPAgent("reserved-seams")
	harnessID := "harness-1"
	workspaceID := "workspace-1"
	agent.HarnessID = &harnessID
	agent.WorkspaceID = &workspaceID
	require.NoError(t, s.CreateAgent(ctx, agent))

	got, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	require.NotNil(t, got.HarnessID)
	require.Equal(t, harnessID, *got.HarnessID)
	require.NotNil(t, got.WorkspaceID)
	require.Equal(t, workspaceID, *got.WorkspaceID)
}

func TestUnit_Agents_NilHarnessAndWorkspaceRoundTripAsNull(t *testing.T) {
	ctx, s := runtimetypes.SetupStore(t)

	agent := newExternalACPAgent("nil-seams")
	require.NoError(t, s.CreateAgent(ctx, agent))

	got, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	require.Nil(t, got.HarnessID)
	require.Nil(t, got.WorkspaceID)
}

func TestUnit_Agents_Provenance_RoundTrip(t *testing.T) {
	ctx, s := runtimetypes.SetupStore(t)

	agent := newExternalACPAgent("provenance-agent")
	source := "registry"
	regID := "goose"
	regVer := "1.43.0"
	agent.Source = &source
	agent.RegistryID = &regID
	agent.RegistryVersion = &regVer
	require.NoError(t, s.CreateAgent(ctx, agent))

	got, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	require.NotNil(t, got.Source)
	require.Equal(t, "registry", *got.Source)
	require.NotNil(t, got.RegistryID)
	require.Equal(t, "goose", *got.RegistryID)
	require.NotNil(t, got.RegistryVersion)
	require.Equal(t, "1.43.0", *got.RegistryVersion)

	items, err := s.ListAgents(ctx, nil, 100)
	require.NoError(t, err)
	require.Len(t, items, 1)
	require.Equal(t, "registry", *items[0].Source)

	got.Enabled = false
	require.NoError(t, s.UpdateAgent(ctx, got))
	afterUpdate, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	require.Equal(t, "goose", *afterUpdate.RegistryID)
}

func TestUnit_Agents_Provenance_NilRoundTripsAsNull(t *testing.T) {
	ctx, s := runtimetypes.SetupStore(t)

	agent := newExternalACPAgent("no-provenance")
	require.NoError(t, s.CreateAgent(ctx, agent))

	got, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	require.Nil(t, got.Source)
	require.Nil(t, got.RegistryID)
	require.Nil(t, got.RegistryVersion)
}

func TestUnit_Agents_Update(t *testing.T) {
	ctx, s := runtimetypes.SetupStore(t)

	original := newExternalACPAgent("update-me")
	require.NoError(t, s.CreateAgent(ctx, original))

	updated := *original
	require.NoError(t, updated.SetExternalACPConfig(runtimetypes.ExternalACPConfig{
		Transport: runtimetypes.ExternalACPTransportEndpoint,
		URL:       "https://agent.example.com/acp",
	}))
	updated.Enabled = false

	require.NoError(t, s.UpdateAgent(ctx, &updated))

	got, err := s.GetAgent(ctx, original.ID)
	require.NoError(t, err)
	require.False(t, got.Enabled)
	cfg, err := got.ExternalACPConfig()
	require.NoError(t, err)
	require.Equal(t, "endpoint", cfg.Transport)
	require.Equal(t, "https://agent.example.com/acp", cfg.URL)
	require.True(t, got.UpdatedAt.After(original.UpdatedAt), "UpdatedAt should advance")
}

func TestUnit_Agents_Delete(t *testing.T) {
	ctx, s := runtimetypes.SetupStore(t)

	agent := newExternalACPAgent("delete-me")
	require.NoError(t, s.CreateAgent(ctx, agent))
	require.NoError(t, s.DeleteAgent(ctx, agent.ID))

	_, err := s.GetAgent(ctx, agent.ID)
	require.Error(t, err)
	require.True(t, errors.Is(err, libdb.ErrNotFound))
}

func TestUnit_Agents_ListEmpty(t *testing.T) {
	ctx, s := runtimetypes.SetupStore(t)

	items, err := s.ListAgents(ctx, nil, 100)
	require.NoError(t, err)
	require.Empty(t, items, "fresh DB should return empty list")
}

func TestUnit_Agents_List(t *testing.T) {
	ctx, s := runtimetypes.SetupStore(t)

	agents := []*runtimetypes.Agent{
		newExternalACPAgent("list-1"),
		newExternalACPAgent("list-2"),
		newExternalACPAgent("list-3"),
	}
	for _, a := range agents {
		require.NoError(t, s.CreateAgent(ctx, a))
	}

	items, err := s.ListAgents(ctx, nil, 100)
	require.NoError(t, err)
	require.Len(t, items, 3)

	require.Equal(t, agents[2].ID, items[0].ID)
	require.Equal(t, agents[1].ID, items[1].ID)
	require.Equal(t, agents[0].ID, items[2].ID)
}

func TestUnit_Agents_ListPagination(t *testing.T) {
	ctx, s := runtimetypes.SetupStore(t)

	var created []*runtimetypes.Agent
	for i := range 5 {
		agent := newExternalACPAgent(fmt.Sprintf("pagination-agent-%d", i))
		require.NoError(t, s.CreateAgent(ctx, agent))
		created = append(created, agent)
	}

	var received []*runtimetypes.Agent
	var cursor *time.Time
	const limit = 2

	page1, err := s.ListAgents(ctx, cursor, limit)
	require.NoError(t, err)
	require.Len(t, page1, 2)
	received = append(received, page1...)
	cursor = &page1[len(page1)-1].CreatedAt

	page2, err := s.ListAgents(ctx, cursor, limit)
	require.NoError(t, err)
	require.Len(t, page2, 2)
	received = append(received, page2...)
	cursor = &page2[len(page2)-1].CreatedAt

	page3, err := s.ListAgents(ctx, cursor, limit)
	require.NoError(t, err)
	require.Len(t, page3, 1)
	received = append(received, page3...)

	page4, err := s.ListAgents(ctx, &page3[0].CreatedAt, limit)
	require.NoError(t, err)
	require.Empty(t, page4)

	require.Len(t, received, 5)
	require.Equal(t, created[4].ID, received[0].ID)
	require.Equal(t, created[3].ID, received[1].ID)
	require.Equal(t, created[2].ID, received[2].ID)
	require.Equal(t, created[1].ID, received[3].ID)
	require.Equal(t, created[0].ID, received[4].ID)
}

func TestUnit_Agents_UniqueNameConstraint(t *testing.T) {
	ctx, s := runtimetypes.SetupStore(t)

	agent1 := newExternalACPAgent("unique-agent-name")
	require.NoError(t, s.CreateAgent(ctx, agent1))

	agent2 := *agent1
	agent2.ID = uuid.New().String()

	err := s.CreateAgent(ctx, &agent2)
	require.Error(t, err, "duplicate name must be rejected")
}

func TestUnit_Agents_DeleteAndRecreate(t *testing.T) {
	ctx, s := runtimetypes.SetupStore(t)

	agent := newExternalACPAgent("recreate-me")
	require.NoError(t, s.CreateAgent(ctx, agent))
	require.NoError(t, s.DeleteAgent(ctx, agent.ID))

	newAgent := *agent
	newAgent.ID = uuid.New().String()
	require.NoError(t, s.CreateAgent(ctx, &newAgent), "should allow recreating with same name after deletion")

	got, err := s.GetAgentByName(ctx, agent.Name)
	require.NoError(t, err)
	require.Equal(t, newAgent.ID, got.ID)
}

func TestUnit_Agents_NotFoundCases(t *testing.T) {
	ctx, s := runtimetypes.SetupStore(t)

	t.Run("get_by_id_not_found", func(t *testing.T) {
		_, err := s.GetAgent(ctx, uuid.New().String())
		require.Error(t, err)
		require.True(t, errors.Is(err, libdb.ErrNotFound))
	})

	t.Run("get_by_name_not_found", func(t *testing.T) {
		_, err := s.GetAgentByName(ctx, "non-existent-agent")
		require.Error(t, err)
		require.True(t, errors.Is(err, libdb.ErrNotFound))
	})

	t.Run("update_non_existent", func(t *testing.T) {
		err := s.UpdateAgent(ctx, newExternalACPAgent("ghost"))
		require.Error(t, err)
		require.True(t, errors.Is(err, libdb.ErrNotFound))
	})

	t.Run("delete_non_existent", func(t *testing.T) {
		err := s.DeleteAgent(ctx, uuid.New().String())
		require.Error(t, err)
	})
}

func TestUnit_Agents_EstimateCount(t *testing.T) {
	ctx, s := runtimetypes.SetupStore(t)

	for i := range 3 {
		require.NoError(t, s.CreateAgent(ctx, newExternalACPAgent(fmt.Sprintf("count-agent-%d", i))))
	}

	_, err := s.EstimateAgentCount(ctx)
	require.NoError(t, err)
}

func TestUnit_Agents_ExternalACPConfig_WrongKindErrors(t *testing.T) {
	agent := &runtimetypes.Agent{
		Name:       "wrong-kind",
		Kind:       runtimetypes.AgentKindChain,
		ConfigJSON: json.RawMessage(`{}`),
	}
	_, err := agent.ExternalACPConfig()
	require.Error(t, err)
}
