package runtimetypes

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	libdb "github.com/contenox/contenox/libdbexec"
	"github.com/stretchr/testify/require"
)

const MAXLIMIT = 1000

var ErrLimitParamExceeded = fmt.Errorf("limit exceeds maximum allowed value")
var ErrAppendLimitExceeded = fmt.Errorf("append limit exceeds maximum allowed values")

type Status struct {
	Status    string `json:"status" example:"downloading"`
	Digest    string `json:"digest,omitempty" example:"sha256:9e3a6c0d3b5e7f8a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6e7f8a9b0c1d2e3f4a"`
	Total     int64  `json:"total,omitempty" example:"1000000"`
	Completed int64  `json:"completed,omitempty" example:"250000"`
	Model     string `json:"model" example:"llama2:7b"`
	BaseURL   string `json:"baseUrl" example:"http://ollama-prod.internal:11434"`
}

type QueueItem struct {
	URL   string `json:"url" example:"http://ollama-prod.internal:11434"`
	Model string `json:"model" example:"llama2:latest"`
}

type Backend struct {
	ID      string `json:"id" example:"b7d9e1a3-8f0c-4a7d-9b1e-2f3a4b5c6d7e"`
	Name    string `json:"name" example:"ollama-production"`
	BaseURL string `json:"baseUrl" example:"http://ollama-prod.internal:11434"`
	Type    string `json:"type" example:"ollama"`

	CreatedAt time.Time `json:"createdAt" example:"2023-11-15T14:30:45Z"`
	UpdatedAt time.Time `json:"updatedAt" example:"2023-11-15T14:30:45Z"`
}

type Model struct {
	ID            string    `json:"id" example:"m7d8e9f0a-1b2c-3d4e-5f6a-7b8c9d0e1f2a"`
	Model         string    `json:"model" example:"llama2:7b"`
	ContextLength int       `json:"contextLength" example:"8192"`
	CanChat       bool      `json:"canChat" example:"true"`
	CanEmbed      bool      `json:"canEmbed" example:"false"`
	CanPrompt     bool      `json:"canPrompt" example:"true"`
	CanStream     bool      `json:"canStream" example:"true"`
	CreatedAt     time.Time `json:"createdAt" example:"2023-11-15T14:30:45Z"`
	UpdatedAt     time.Time `json:"updatedAt" example:"2023-11-15T14:30:45Z"`
}

// AffinityGroup is a logical grouping of preferred model-backend
// relationships (e.g. tiering, routing strategy); entities can belong to
// multiple groups at once.
type AffinityGroup struct {
	ID          string `json:"id" example:"p9a8b7c6-d5e4-f3a2-b1c0-d9e8f7a6b5c4"`
	Name        string `json:"name" example:"production-chat"`
	PurposeType string `json:"purposeType" example:"Internal Tasks"`

	CreatedAt time.Time `json:"createdAt" example:"2023-11-15T14:30:45Z"`
	UpdatedAt time.Time `json:"updatedAt" example:"2023-11-15T14:30:45Z"`
}

type Job struct {
	ID           string          `json:"id" example:"j1a2b3c4-d5e6-f7g8-h9i0-j1k2l3m4n5o6"`
	TaskType     string          `json:"taskType" example:"model-download"`
	Payload      json.RawMessage `json:"payload" example:"{\"model\":\"llama2:7b\",\"backend\":\"b7d9e1a3-8f0c-4a7d-9b1e-2f3a4b5c6d7e\"}"`
	ScheduledFor int64           `json:"scheduledFor" example:"1717020800"`
	ValidUntil   int64           `json:"validUntil" example:"1717024400"`
	RetryCount   int             `json:"retryCount" example:"0"`
	CreatedAt    time.Time       `json:"createdAt" example:"2023-11-15T14:30:45Z"`
}

// KV represents a key-value pair in the database
type KV struct {
	Key       string          `json:"key" example:"config:default-model"`
	Value     json.RawMessage `json:"value" example:"\"llama2:7b\""`
	CreatedAt time.Time       `json:"createdAt" example:"2023-11-15T14:30:45Z"`
	UpdatedAt time.Time       `json:"updatedAt" example:"2023-11-15T14:30:45Z"`
}

const (
	LocationPath  = "path"
	LocationQuery = "query"
	LocationBody  = "body"
)

type InjectionArg struct {
	Name  string `json:"name" example:"access_token"`
	Value any    `json:"value" example:"secret-token"`
	In    string `json:"in" example:"body"`
}

// AuthFlow describes how to authenticate before calling a remote tool.
type AuthFlow struct {
	Type            string `json:"type" example:"http_handshake"`
	LoginMethod     string `json:"loginMethod" example:"POST"`
	LoginURL        string `json:"loginUrl" example:"https://erp.local/api/method/login"`
	LoginBody       string `json:"loginBody" example:"{\"usr\":\"${FRAPPE_USER}\", \"pwd\":\"${FRAPPE_PASS}\"}"`
	ExtractCookie   string `json:"extractCookie,omitempty" example:"sid"`
	ExtractJSONPath string `json:"extractJsonPath,omitempty" example:"$.data.token"`
	InjectHeader    string `json:"injectHeader,omitempty" example:"Cookie"`
	InjectFormat    string `json:"injectFormat,omitempty" example:"sid=%s"`
}

// RemoteTools represents a remote tools configuration
type RemoteTools struct {
	ID                 string            `json:"id" example:"h1a2b3c4-d5e6-f7g8-h9i0-j1k2l3m4n5o6"`
	Name               string            `json:"name" example:"mailing-tools"`
	EndpointURL        string            `json:"endpointUrl" example:"http://tools-endpoint:port"`
	SpecURL            string            `json:"specUrl,omitempty"` // optional; file:///abs/path or https://... — when set, spec is loaded from here instead of EndpointURL+/openapi.json
	TimeoutMs          int               `json:"timeoutMs" example:"5000"`
	Headers            map[string]string `json:"headers,omitempty" example:"Authorization:Bearer token,Content-Type:application/json"`
	Properties         InjectionArg      `json:"properties"`
	InjectParams       map[string]string `json:"injectParams,omitempty"` // injected as tool call args, hidden from model schema
	AuthFlow           *AuthFlow         `json:"authFlow,omitempty"`
	InsecureSkipVerify bool              `json:"insecureSkipVerify"`
	CreatedAt          time.Time         `json:"createdAt" example:"2023-11-15T14:30:45Z"`
	UpdatedAt          time.Time         `json:"updatedAt" example:"2023-11-15T14:30:45Z"`
}

// BackendStore defines persistence operations for LLM backend configurations.
type BackendStore interface {
	CreateBackend(ctx context.Context, backend *Backend) error
	GetBackend(ctx context.Context, id string) (*Backend, error)
	UpdateBackend(ctx context.Context, backend *Backend) error
	DeleteBackend(ctx context.Context, id string) error
	ListAllBackends(ctx context.Context) ([]*Backend, error)
	ListBackends(ctx context.Context, createdAtCursor *time.Time, limit int) ([]*Backend, error)
	GetBackendByName(ctx context.Context, name string) (*Backend, error)
	EstimateBackendCount(ctx context.Context) (int64, error)
}

// ModelStore defines persistence operations for declared model configurations.
type ModelStore interface {
	AppendModel(ctx context.Context, model *Model) error
	GetModel(ctx context.Context, id string) (*Model, error)
	GetModelByName(ctx context.Context, name string) (*Model, error)
	DeleteModel(ctx context.Context, modelName string) error
	ListAllModels(ctx context.Context) ([]*Model, error)
	UpdateModel(ctx context.Context, data *Model) error
	ListModels(ctx context.Context, createdAtCursor *time.Time, limit int) ([]*Model, error)
	EstimateModelCount(ctx context.Context) (int64, error)
}

// ModelRegistryStore defines persistence operations for registered downloaded model metadata.
type ModelRegistryStore interface {
	CreateModelRegistryEntry(ctx context.Context, e *ModelRegistryEntry) error
	GetModelRegistryEntry(ctx context.Context, id string) (*ModelRegistryEntry, error)
	GetModelRegistryEntryByName(ctx context.Context, name string) (*ModelRegistryEntry, error)
	UpdateModelRegistryEntry(ctx context.Context, e *ModelRegistryEntry) error
	DeleteModelRegistryEntry(ctx context.Context, id string) error
	ListModelRegistryEntries(ctx context.Context, cursor *time.Time, limit int) ([]*ModelRegistryEntry, error)
	EstimateModelRegistryEntryCount(ctx context.Context) (int64, error)
}

type Store interface {
	BackendStore
	ModelStore
	ModelRegistryStore

	CreateAffinityGroup(ctx context.Context, group *AffinityGroup) error
	GetAffinityGroup(ctx context.Context, id string) (*AffinityGroup, error)
	GetAffinityGroupByName(ctx context.Context, name string) (*AffinityGroup, error)
	UpdateAffinityGroup(ctx context.Context, group *AffinityGroup) error
	DeleteAffinityGroup(ctx context.Context, id string) error
	ListAllAffinityGroups(ctx context.Context) ([]*AffinityGroup, error)
	ListAffinityGroups(ctx context.Context, createdAtCursor *time.Time, limit int) ([]*AffinityGroup, error)
	ListAffinityGroupByPurpose(ctx context.Context, purposeType string, createdAtCursor *time.Time, limit int) ([]*AffinityGroup, error)
	EstimateAffinityGroupCount(ctx context.Context) (int64, error)

	AssignBackendToAffinityGroup(ctx context.Context, groupID string, backendID string) error
	RemoveBackendFromAffinityGroup(ctx context.Context, groupID string, backendID string) error
	ListBackendsForAffinityGroup(ctx context.Context, groupID string) ([]*Backend, error)
	ListAffinityGroupsForBackend(ctx context.Context, backendID string) ([]*AffinityGroup, error)

	AssignModelToAffinityGroup(ctx context.Context, groupID string, modelID string) error
	RemoveModelFromAffinityGroup(ctx context.Context, groupID string, modelID string) error
	ListModelsForAffinityGroup(ctx context.Context, groupID string) ([]*Model, error)
	ListAffinityGroupsForModel(ctx context.Context, modelID string) ([]*AffinityGroup, error)

	AppendJob(ctx context.Context, job Job) error
	AppendJobs(ctx context.Context, jobs ...*Job) error
	PopAllJobs(ctx context.Context) ([]*Job, error)
	PopJobsForType(ctx context.Context, taskType string) ([]*Job, error)
	PopNJobsForType(ctx context.Context, taskType string, n int) ([]*Job, error)
	PopJobForType(ctx context.Context, taskType string) (*Job, error)
	GetJobsForType(ctx context.Context, taskType string) ([]*Job, error)
	ListJobs(ctx context.Context, createdAtCursor *time.Time, limit int) ([]*Job, error)
	EstimateJobCount(ctx context.Context) (int64, error)

	SetKV(ctx context.Context, key string, value json.RawMessage) error
	UpdateKV(ctx context.Context, key string, value json.RawMessage) error
	UpdateKVIfUnchanged(ctx context.Context, key string, expected, value json.RawMessage) error
	GetKV(ctx context.Context, key string, out interface{}) error
	GetKVRaw(ctx context.Context, key string) (json.RawMessage, error)
	DeleteKV(ctx context.Context, key string) error
	ListKV(ctx context.Context, createdAtCursor *time.Time, limit int) ([]*KV, error)
	ListKVPrefix(ctx context.Context, prefix string, createdAtCursor *time.Time, limit int) ([]*KV, error)
	EstimateKVCount(ctx context.Context) (int64, error)
	SetWorkspaceKV(ctx context.Context, workspaceID string, key string, value json.RawMessage) error
	GetWorkspaceKV(ctx context.Context, workspaceID string, key string, out interface{}) error
	DeleteWorkspaceKV(ctx context.Context, workspaceID string, key string) error

	CreateRemoteTools(ctx context.Context, tools *RemoteTools) error
	GetRemoteTools(ctx context.Context, id string) (*RemoteTools, error)
	GetRemoteToolsByName(ctx context.Context, name string) (*RemoteTools, error)
	UpdateRemoteTools(ctx context.Context, tools *RemoteTools) error
	DeleteRemoteTools(ctx context.Context, id string) error
	ListRemoteTools(ctx context.Context, createdAtCursor *time.Time, limit int) ([]*RemoteTools, error)
	EstimateRemoteToolsCount(ctx context.Context) (int64, error)

	CreateMCPServer(ctx context.Context, srv *MCPServer) error
	GetMCPServer(ctx context.Context, id string) (*MCPServer, error)
	GetMCPServerByName(ctx context.Context, name string) (*MCPServer, error)
	UpdateMCPServer(ctx context.Context, srv *MCPServer) error
	DeleteMCPServer(ctx context.Context, id string) error
	ListMCPServers(ctx context.Context, createdAtCursor *time.Time, limit int) ([]*MCPServer, error)
	EstimateMCPServerCount(ctx context.Context) (int64, error)
	// UpsertMCPServerByName inserts or updates an MCP server record keyed by
	// name, updating in place (same ID) when one already exists.
	UpsertMCPServerByName(ctx context.Context, srv *MCPServer) error

	CreateAgent(ctx context.Context, agent *Agent) error
	GetAgent(ctx context.Context, id string) (*Agent, error)
	GetAgentByName(ctx context.Context, name string) (*Agent, error)
	UpdateAgent(ctx context.Context, agent *Agent) error
	DeleteAgent(ctx context.Context, id string) error
	ListAgents(ctx context.Context, createdAtCursor *time.Time, limit int) ([]*Agent, error)
	EstimateAgentCount(ctx context.Context) (int64, error)

	// HITL* methods back runtime/hitlservice's durable pending-ask table (see hitl_approvals.go).
	CreateHITLApproval(ctx context.Context, a *HITLApproval) error
	GetHITLApproval(ctx context.Context, id string) (*HITLApproval, error)
	ResolveHITLApproval(ctx context.Context, id string, state HITLApprovalState, resolution json.RawMessage, resolvedAt time.Time) error
	ResolveHITLApprovalWithinBound(ctx context.Context, id string, bound AgentAnswerBound, state HITLApprovalState, resolution json.RawMessage, resolvedAt time.Time) error
	ListExpiredHITLApprovals(ctx context.Context, asOf time.Time, limit int) ([]*HITLApproval, error)
	ListHITLApprovals(ctx context.Context, state HITLApprovalState, createdAtCursor *time.Time, limit int) ([]*HITLApproval, error)
	ListHITLApprovalsForMission(ctx context.Context, missionID string, limit int) ([]*HITLApproval, error)
	ListPendingHITLApprovalsForSession(ctx context.Context, sessionID string, limit int) ([]*HITLApproval, error)
	EstimateHITLApprovalCount(ctx context.Context) (int64, error)

	// Chain checkpoints back the suspend/resume machinery: a suspended
	// chain run is a row here, keyed by the approval ID whose verdict resumes
	// it (see runtimetypes/checkpoints.go).
	CreateChainCheckpoint(ctx context.Context, cp *ChainCheckpoint) error
	GetChainCheckpoint(ctx context.Context, id string) (*ChainCheckpoint, error)
	ClaimChainCheckpoint(ctx context.Context, id string, now, staleBefore time.Time) error
	TouchChainCheckpointClaim(ctx context.Context, id string, now time.Time) error
	UpdateChainCheckpointPayload(ctx context.Context, id string, payload json.RawMessage) error
	SetChainCheckpointFailure(ctx context.Context, id string, failure string) error
	DeleteChainCheckpoint(ctx context.Context, id string) error
	ListChainCheckpoints(ctx context.Context, createdAtCursor *time.Time, limit int) ([]*ChainCheckpoint, error)

	// Workspace semantic index: an immutable index-config generation plus its
	// chunks and their FTS5 lexical mirror; UpdateWorkspaceIndexConfig is
	// absent because a config is create-once.
	CreateWorkspaceIndexConfig(ctx context.Context, cfg *WorkspaceIndexConfig) error
	GetWorkspaceIndexConfig(ctx context.Context, id string) (*WorkspaceIndexConfig, error)
	GetActiveWorkspaceIndexConfig(ctx context.Context, workspaceID string) (*WorkspaceIndexConfig, error)
	ListWorkspaceIndexConfigs(ctx context.Context, workspaceID string, createdAtCursor *time.Time, limit int) ([]*WorkspaceIndexConfig, error)
	DeleteWorkspaceIndexConfig(ctx context.Context, id string) error

	AppendWorkspaceChunks(ctx context.Context, chunks ...*WorkspaceChunk) error
	ListWorkspaceIndexedFiles(ctx context.Context, configID string) ([]WorkspaceIndexedFile, error)
	DeleteWorkspaceChunksForPaths(ctx context.Context, configID string, paths ...string) error
	DeleteWorkspaceChunksForConfig(ctx context.Context, configID string) error
	SearchWorkspaceChunks(ctx context.Context, configID string, match string, limit int) ([]*WorkspaceChunk, error)
	ScanWorkspaceChunks(ctx context.Context, configID string, limit int) ([]*WorkspaceChunk, error)
	CountWorkspaceChunks(ctx context.Context, configID string) (int64, error)

	EnforceMaxRowCount(ctx context.Context, count int64) error
}

//go:embed schema_sqlite.sql
var SchemaSQLite string

//go:embed schema_postgres.sql
var SchemaPostgres string

type store struct {
	libdb.Exec
}

func New(exec libdb.Exec) Store {
	if exec == nil {
		panic("SERVER BUG: store.New called with nil exec")
	}
	return &store{exec}
}

const MaxRowsCount = 100000

var countableTables = map[string]bool{
	"job_queue_v2": true, "kv": true, "remote_tools": true,
	"ollama_models": true, "llm_affinity_group": true, "llm_backends": true,
	"mcp_servers": true, "llm_model_registry": true, "agents": true,
	"hitl_approvals": true,
}

func (s *store) estimateCount(ctx context.Context, table string) (int64, error) {
	var count int64
	err := s.Exec.QueryRowContext(ctx, `
		SELECT estimate_row_count($1)
	`, table).Scan(&count)
	if err == nil && count >= 0 {
		return count, nil
	}
	if err != nil && !strings.Contains(err.Error(), "no such function") {
		return 0, err
	}
	if !countableTables[table] {
		return count, err
	}
	err = s.Exec.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table).Scan(&count)
	return count, err
}

func (s *store) EnforceMaxRowCount(ctx context.Context, count int64) error {
	if count >= MaxRowsCount {
		return fmt.Errorf("row limit reached (max %d)", MaxRowsCount)
	}
	return nil
}

type TestBackend string

const (
	TestBackendSQLite   TestBackend = "sqlite"
	TestBackendPostgres TestBackend = "postgres"
)

const TestBackendEnv = "CONTENOX_TEST_STORE_BACKEND"

func TestBackendDefault() TestBackend {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(TestBackendEnv))) {
	case "", string(TestBackendSQLite):
		return TestBackendSQLite
	case string(TestBackendPostgres):
		return TestBackendPostgres
	default:
		panic(fmt.Sprintf("%s must be %q or %q, got %q", TestBackendEnv, TestBackendSQLite, TestBackendPostgres, os.Getenv(TestBackendEnv)))
	}
}

func resolveTestBackend(backend []TestBackend) TestBackend {
	if len(backend) > 0 && backend[0] != "" {
		return backend[0]
	}
	return TestBackendDefault()
}

func SetupStore(t *testing.T, backend ...TestBackend) (context.Context, Store) {
	t.Helper()
	ctx, dbManager := SetupDBManager(t, backend...)
	return ctx, New(dbManager.WithoutTransaction())
}

func SetupStoreExec(t *testing.T, backend ...TestBackend) (context.Context, Store, libdb.Exec) {
	t.Helper()
	ctx, dbManager := SetupDBManager(t, backend...)
	exec := dbManager.WithoutTransaction()
	return ctx, New(exec), exec
}

func SetupDBManager(t *testing.T, backend ...TestBackend) (context.Context, libdb.DBManager) {
	t.Helper()

	ctx := context.TODO()
	switch b := resolveTestBackend(backend); b {
	case TestBackendSQLite:
		dbManager, err := libdb.NewSQLiteDBManager(ctx, filepath.Join(t.TempDir(), "test.db"), SchemaSQLite)
		require.NoError(t, err)

		t.Cleanup(func() {
			require.NoError(t, dbManager.Close())
		})

		return ctx, dbManager
	case TestBackendPostgres:
		return ctx, setupTestPostgres(t, ctx)
	default:
		t.Fatalf("unknown store test backend %q", b)
		return ctx, nil
	}
}
