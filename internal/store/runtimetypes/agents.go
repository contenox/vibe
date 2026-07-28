package runtimetypes

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	libdb "github.com/contenox/beam/internal/libdbexec"
	"github.com/google/uuid"
)

// Agent kinds.
//
//   - AgentKindChain: one of the runtime's own task chains, addressable as an
//     agent. The kernel re-executes this binary (`contenox acp`) bound to the
//     named chain file, so a chain runs as an ACP peer over stdio. The only
//     kind users declare; seeded by discovery from chain files on disk (see
//     runtime/chainagents, ChainConfig).
//   - AgentKindExternalACP: internal only, not user-registerable — no CLI,
//     API, or UI path creates one. It is the spawn-config shape the instance
//     kernel and the agenthost one-shot driver build to run an ACP peer over
//     stdio (see ExternalACPConfig), reused by the chain path and test harness.
const (
	AgentKindExternalACP = "external_acp"
	AgentKindChain       = "chain"
)

// Agent.Source values — system-managed provenance, never part of the
// user-editable run spec.
//
//   - AgentSourceRegistry: seeded from the ACP agent catalog.
//   - AgentSourceManual: registered by hand from a bare command.
//   - AgentSourceDiscovered: seeded by chain-agent discovery from a chain file
//     on disk (see runtime/chainagents). Discovery owns these rows — it
//     re-upserts on every startup and disables ones whose chain file is gone;
//     rows with any other Source are never touched by it.
const (
	AgentSourceRegistry   = "registry"
	AgentSourceManual     = "manual"
	AgentSourceDiscovered = "discovered"
)

// ExternalACPConfig is the config_json shape for an AgentKindExternalACP
// agent: how to reach the external ACP agent the runtime spawns/drives.
//
// Transport selects which of the remaining fields apply:
//   - "stdio": the runtime spawns Command with Args/Env/Cwd and speaks ACP
//     over the subprocess's stdin/stdout (see runtime/agenthost, which wraps
//     libacp/acpexec for this).
//   - "endpoint": the runtime dials URL directly instead of spawning a
//     process. Not implemented yet (runtime/agenthost returns a clear error);
//     the field exists so the config shape doesn't need to change later.
type ExternalACPConfig struct {
	Transport string            `json:"transport" example:"stdio"` // "stdio" | "endpoint"
	Command   string            `json:"command,omitempty" example:"my-acp-agent"`
	Args      []string          `json:"args,omitempty" example:"['--flag']" openapi_include_type:"string"`
	Env       map[string]string `json:"env,omitempty"`
	Cwd       string            `json:"cwd,omitempty" example:"/workspace"`
	URL       string            `json:"url,omitempty" example:"https://agent.example.com/acp"`

	// McpServers is the explicit allowlist of registered MCP server names
	// forwarded to this agent in ACP session/new — per-agent consent, named
	// server by named server, deliberately no "all servers" wildcard.
	// contenox-side auth synthesis (authToken/authEnvKey/oauth/injectParams)
	// is never forwarded. Empty means forward nothing.
	McpServers []string `json:"mcp_servers,omitempty" example:"['filesystem']" openapi_include_type:"string"`
}

// Transport values accepted by ExternalACPConfig.Transport.
const (
	ExternalACPTransportStdio    = "stdio"
	ExternalACPTransportEndpoint = "endpoint"
)

// Validate checks the transport-specific requirements of an ExternalACPConfig
// in isolation: it does not know (or care) about the owning Agent's name or
// kind — that belongs to the caller (the registry service validates name
// uniqueness and kind separately; see runtime/agentregistryservice).
func (c ExternalACPConfig) Validate() error {
	switch c.Transport {
	case ExternalACPTransportStdio:
		if c.Command == "" {
			return fmt.Errorf("external_acp: command is required for stdio transport")
		}
	case ExternalACPTransportEndpoint:
		if c.URL == "" {
			return fmt.Errorf("external_acp: url is required for endpoint transport")
		}
	case "":
		return fmt.Errorf("external_acp: transport is required (stdio or endpoint)")
	default:
		return fmt.Errorf("external_acp: unknown transport %q: must be stdio or endpoint", c.Transport)
	}
	for _, name := range c.McpServers {
		if strings.TrimSpace(name) == "" {
			return fmt.Errorf("external_acp: mcp_servers entries must be non-empty registered server names")
		}
	}
	return nil
}

// ChainConfig is the config_json shape for an AgentKindChain agent: which of
// the runtime's own task chains a unit launched from this template runs. Much
// smaller than ExternalACPConfig because the chain file already says
// everything — no command, no credentials, no transport choice.
//
// Path, not a chain id or name, is the field: the spawned runtime resolves its
// chain from a file (runtime/acpsvc's chain registry reads a path), avoiding a
// second implementation of the lookup discovery already performs. Required to
// be absolute because the spawned process's working directory is the
// session's, not the declarer's.
type ChainConfig struct {
	Path string `json:"path" example:"/home/user/.contenox/agent-reviewer.json"`

	// ChainID is the "id" field inside that chain file, recorded at declaration
	// time so a listing can show which chain a unit runs without reading every
	// file. A display copy, not resolved through: it goes stale until the next
	// discovery pass rewrites it. Optional.
	ChainID string `json:"chainId,omitempty" example:"agent-reviewer"`
}

// Validate checks a ChainConfig in isolation, knowing nothing about the
// owning Agent. It deliberately does not stat the path — existence can
// change between declaration and spawn, and a missing file surfaces there,
// where the ACP chain loader fails closed with the path in the message.
func (c ChainConfig) Validate() error {
	if strings.TrimSpace(c.Path) == "" {
		return fmt.Errorf("chain: path is required (the chain file this agent runs)")
	}
	if !filepath.IsAbs(c.Path) {
		return fmt.Errorf("chain: path %q must be absolute", c.Path)
	}
	return nil
}

// Agent represents a persisted, declared agent resource: something the
// runtime can spawn/drive as an ACP peer, either somebody else's program
// (kind "external_acp") or one of the runtime's own task chains (kind "chain").
//
// ConfigJSON carries the kind-specific config as raw JSON rather than flat
// columns, since agents is polymorphic and flat-columns-per-kind would have
// been a migration trap the moment a second kind was implemented.
type Agent struct {
	ID          string          `json:"id" example:"a1b2c3d4-e5f6-7890-abcd-ef1234567890"`
	Name        string          `json:"name" example:"local-claude-code"`
	Kind        string          `json:"kind" example:"external_acp"`
	Enabled     bool            `json:"enabled" example:"true"`
	ConfigJSON  json.RawMessage `json:"configJson" example:"{\"transport\":\"stdio\",\"command\":\"claude-code-acp\"}"`
	HarnessID   *string         `json:"harnessId,omitempty"`   // reserved FK seam; nil = implicit serve harness
	WorkspaceID *string         `json:"workspaceId,omitempty"` // reserved scoping seam, consistent with kv/message_indices

	// Source, RegistryID, and RegistryVersion are system-managed provenance for
	// display and updates, not part of the user-editable run spec (ConfigJSON).
	// RegistryID/RegistryVersion record the catalog entry a registry-sourced
	// agent was seeded from; nil for agents predating provenance tracking.
	Source          *string `json:"source,omitempty"`
	RegistryID      *string `json:"registryId,omitempty"`
	RegistryVersion *string `json:"registryVersion,omitempty"`

	CreatedAt time.Time `json:"createdAt" example:"2024-01-15T10:00:00Z"`
	UpdatedAt time.Time `json:"updatedAt" example:"2024-01-15T10:00:00Z"`
}

// ExternalACPConfig unmarshals ConfigJSON as an ExternalACPConfig. It returns
// an error if the agent's Kind is not AgentKindExternalACP or the stored JSON
// doesn't parse — the typed accessor a polymorphic config_json column needs
// so callers never have to unmarshal raw JSON themselves.
func (a *Agent) ExternalACPConfig() (*ExternalACPConfig, error) {
	if a.Kind != AgentKindExternalACP {
		return nil, fmt.Errorf("agent %q: kind is %q, not %q", a.Name, a.Kind, AgentKindExternalACP)
	}
	var cfg ExternalACPConfig
	if len(a.ConfigJSON) == 0 {
		return &cfg, nil
	}
	if err := json.Unmarshal(a.ConfigJSON, &cfg); err != nil {
		return nil, fmt.Errorf("agent %q: unmarshal external_acp config: %w", a.Name, err)
	}
	return &cfg, nil
}

// SetExternalACPConfig marshals cfg into ConfigJSON and sets Kind to
// AgentKindExternalACP.
func (a *Agent) SetExternalACPConfig(cfg ExternalACPConfig) error {
	raw, err := json.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("agent: marshal external_acp config: %w", err)
	}
	a.Kind = AgentKindExternalACP
	a.ConfigJSON = raw
	return nil
}

// ChainConfig unmarshals ConfigJSON as a ChainConfig, the AgentKindChain
// counterpart of ExternalACPConfig() and identical in contract: it errors if
// the agent's Kind is not AgentKindChain or the stored JSON doesn't parse.
func (a *Agent) ChainConfig() (*ChainConfig, error) {
	if a.Kind != AgentKindChain {
		return nil, fmt.Errorf("agent %q: kind is %q, not %q", a.Name, a.Kind, AgentKindChain)
	}
	var cfg ChainConfig
	if len(a.ConfigJSON) == 0 {
		return &cfg, nil
	}
	if err := json.Unmarshal(a.ConfigJSON, &cfg); err != nil {
		return nil, fmt.Errorf("agent %q: unmarshal chain config: %w", a.Name, err)
	}
	return &cfg, nil
}

// SetChainConfig marshals cfg into ConfigJSON and sets Kind to AgentKindChain.
func (a *Agent) SetChainConfig(cfg ChainConfig) error {
	raw, err := json.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("agent: marshal chain config: %w", err)
	}
	a.Kind = AgentKindChain
	a.ConfigJSON = raw
	return nil
}

func (s *store) CreateAgent(ctx context.Context, agent *Agent) error {
	now := time.Now().UTC()
	agent.CreatedAt = now
	agent.UpdatedAt = now
	if agent.ID == "" {
		agent.ID = uuid.NewString()
	}
	configJSON := agent.ConfigJSON
	if len(configJSON) == 0 {
		configJSON = json.RawMessage("{}")
	}

	_, err := s.Exec.ExecContext(ctx, `
		INSERT INTO agents
		(id, name, kind, enabled, config_json, harness_id, workspace_id, source, registry_id, registry_version, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`,
		agent.ID, agent.Name, agent.Kind, agent.Enabled, string(configJSON),
		agent.HarnessID, agent.WorkspaceID, agent.Source, agent.RegistryID, agent.RegistryVersion,
		agent.CreatedAt, agent.UpdatedAt,
	)
	return err
}

func (s *store) GetAgent(ctx context.Context, id string) (*Agent, error) {
	return s.scanAgent(ctx, `
		SELECT id, name, kind, enabled, config_json, harness_id, workspace_id, source, registry_id, registry_version, created_at, updated_at
		FROM agents WHERE id = $1`, id)
}

func (s *store) GetAgentByName(ctx context.Context, name string) (*Agent, error) {
	return s.scanAgent(ctx, `
		SELECT id, name, kind, enabled, config_json, harness_id, workspace_id, source, registry_id, registry_version, created_at, updated_at
		FROM agents WHERE name = $1`, name)
}

func (s *store) scanAgent(ctx context.Context, query string, arg any) (*Agent, error) {
	var agent Agent
	var configJSON string
	err := s.Exec.QueryRowContext(ctx, query, arg).Scan(
		&agent.ID, &agent.Name, &agent.Kind, &agent.Enabled, &configJSON,
		&agent.HarnessID, &agent.WorkspaceID, &agent.Source, &agent.RegistryID, &agent.RegistryVersion,
		&agent.CreatedAt, &agent.UpdatedAt,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, libdb.ErrNotFound
		}
		return nil, err
	}
	agent.ConfigJSON = json.RawMessage(configJSON)
	return &agent, nil
}

func (s *store) UpdateAgent(ctx context.Context, agent *Agent) error {
	agent.UpdatedAt = time.Now().UTC()
	configJSON := agent.ConfigJSON
	if len(configJSON) == 0 {
		configJSON = json.RawMessage("{}")
	}

	result, err := s.Exec.ExecContext(ctx, `
		UPDATE agents
		SET name=$2, kind=$3, enabled=$4, config_json=$5, harness_id=$6, workspace_id=$7, source=$8, registry_id=$9, registry_version=$10, updated_at=$11
		WHERE id=$1`,
		agent.ID, agent.Name, agent.Kind, agent.Enabled, string(configJSON),
		agent.HarnessID, agent.WorkspaceID, agent.Source, agent.RegistryID, agent.RegistryVersion,
		agent.UpdatedAt,
	)
	if err != nil {
		return err
	}
	return checkRowsAffected(result)
}

func (s *store) DeleteAgent(ctx context.Context, id string) error {
	result, err := s.Exec.ExecContext(ctx, `DELETE FROM agents WHERE id = $1`, id)
	if err != nil {
		return err
	}
	return checkRowsAffected(result)
}

func (s *store) ListAgents(ctx context.Context, createdAtCursor *time.Time, limit int) ([]*Agent, error) {
	cursor := time.Now().UTC()
	if createdAtCursor != nil {
		cursor = *createdAtCursor
	}
	if limit > MAXLIMIT {
		return nil, ErrLimitParamExceeded
	}

	rows, err := s.Exec.QueryContext(ctx, `
		SELECT id, name, kind, enabled, config_json, harness_id, workspace_id, source, registry_id, registry_version, created_at, updated_at
		FROM agents
		WHERE created_at < $1
		ORDER BY created_at DESC, id DESC
		LIMIT $2`, cursor, limit)
	if err != nil {
		return nil, fmt.Errorf("agents: list query: %w", err)
	}
	defer rows.Close()

	var out []*Agent
	for rows.Next() {
		var agent Agent
		var configJSON string
		if err := rows.Scan(
			&agent.ID, &agent.Name, &agent.Kind, &agent.Enabled, &configJSON,
			&agent.HarnessID, &agent.WorkspaceID, &agent.Source, &agent.RegistryID, &agent.RegistryVersion,
			&agent.CreatedAt, &agent.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("agents: scan row: %w", err)
		}
		agent.ConfigJSON = json.RawMessage(configJSON)
		out = append(out, &agent)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("agents: rows error: %w", err)
	}
	return out, nil
}

func (s *store) EstimateAgentCount(ctx context.Context) (int64, error) {
	return s.estimateCount(ctx, "agents")
}
