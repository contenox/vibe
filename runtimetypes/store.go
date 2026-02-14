package runtimetypes

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"
	"testing"
	"time"

	libdb "github.com/contenox/vibe/libdbexec"
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
	Model     string `json:"model" example:"mistral:instruct"`
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
	Model         string    `json:"model" example:"mistral:instruct"`
	ContextLength int       `json:"contextLength" example:"8192"`
	CanChat       bool      `json:"canChat" example:"true"`
	CanEmbed      bool      `json:"canEmbed" example:"false"`
	CanPrompt     bool      `json:"canPrompt" example:"true"`
	CanStream     bool      `json:"canStream" example:"true"`
	CreatedAt     time.Time `json:"createdAt" example:"2023-11-15T14:30:45Z"`
	UpdatedAt     time.Time `json:"updatedAt" example:"2023-11-15T14:30:45Z"`
}

// AffinityGroup represents a logical grouping that defines preferred relationships
// between models and backends. Entities can
// belong to multiple affinity groups simultaneously.
//
// Affinity groups enable:
//   - Selective model-backend relationships (not all backends serve all models)
//   - Performance tiering (assigning models to appropriate backend tiers)
//   - Custom routing strategies based on application requirements
//
// Example use cases:
//   - "embedding-affinity": Contains embedding models and their dedicated backends
//   - "low-latency-affinity": Contains critical models that need fastest response
//   - "high-throughput-affinity": Contains models that benefit from batch processing
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
	Payload      json.RawMessage `json:"payload" example:"{\"model\":\"mistral:instruct\",\"backend\":\"b7d9e1a3-8f0c-4a7d-9b1e-2f3a4b5c6d7e\"}"`
	ScheduledFor int64           `json:"scheduledFor" example:"1717020800"`
	ValidUntil   int64           `json:"validUntil" example:"1717024400"`
	RetryCount   int             `json:"retryCount" example:"0"`
	CreatedAt    time.Time       `json:"createdAt" example:"2023-11-15T14:30:45Z"`
}

// KV represents a key-value pair in the database
type KV struct {
	Key       string          `json:"key" example:"config:default-model"`
	Value     json.RawMessage `json:"value" example:"\"mistral:instruct\""`
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

// RemoteHook represents a remote hook configuration
type RemoteHook struct {
	ID          string            `json:"id" example:"h1a2b3c4-d5e6-f7g8-h9i0-j1k2l3m4n5o6"`
	Name        string            `json:"name" example:"mailing-tools"`
	EndpointURL string            `json:"endpointUrl" example:"http://hooks-endpoint:port"`
	TimeoutMs   int               `json:"timeoutMs" example:"5000"`
	Headers     map[string]string `json:"headers,omitempty" example:"Authorization:Bearer token,Content-Type:application/json"`
	Properties  InjectionArg      `json:"properties"`
	CreatedAt   time.Time         `json:"createdAt" example:"2023-11-15T14:30:45Z"`
	UpdatedAt   time.Time         `json:"updatedAt" example:"2023-11-15T14:30:45Z"`
}

type Store interface {
	CreateBackend(ctx context.Context, backend *Backend) error
	GetBackend(ctx context.Context, id string) (*Backend, error)
	UpdateBackend(ctx context.Context, backend *Backend) error
	DeleteBackend(ctx context.Context, id string) error
	ListAllBackends(ctx context.Context) ([]*Backend, error)
	ListBackends(ctx context.Context, createdAtCursor *time.Time, limit int) ([]*Backend, error)
	GetBackendByName(ctx context.Context, name string) (*Backend, error)
	EstimateBackendCount(ctx context.Context) (int64, error)

	AppendModel(ctx context.Context, model *Model) error
	GetModel(ctx context.Context, id string) (*Model, error)
	GetModelByName(ctx context.Context, name string) (*Model, error)
	DeleteModel(ctx context.Context, modelName string) error
	ListAllModels(ctx context.Context) ([]*Model, error)
	UpdateModel(ctx context.Context, data *Model) error
	ListModels(ctx context.Context, createdAtCursor *time.Time, limit int) ([]*Model, error)
	EstimateModelCount(ctx context.Context) (int64, error)

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
	GetKV(ctx context.Context, key string, out interface{}) error
	DeleteKV(ctx context.Context, key string) error
	ListKV(ctx context.Context, createdAtCursor *time.Time, limit int) ([]*KV, error)
	ListKVPrefix(ctx context.Context, prefix string, createdAtCursor *time.Time, limit int) ([]*KV, error)
	EstimateKVCount(ctx context.Context) (int64, error)

	CreateRemoteHook(ctx context.Context, hook *RemoteHook) error
	GetRemoteHook(ctx context.Context, id string) (*RemoteHook, error)
	GetRemoteHookByName(ctx context.Context, name string) (*RemoteHook, error)
	UpdateRemoteHook(ctx context.Context, hook *RemoteHook) error
	DeleteRemoteHook(ctx context.Context, id string) error
	ListRemoteHooks(ctx context.Context, createdAtCursor *time.Time, limit int) ([]*RemoteHook, error)
	EstimateRemoteHookCount(ctx context.Context) (int64, error)

	EnforceMaxRowCount(ctx context.Context, count int64) error
}

//go:embed schema.sql
var Schema string

//go:embed schema_sqlite.sql
var SchemaSQLite string

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

// sqliteCountableTables is the whitelist for SELECT COUNT(*) fallback when estimate_row_count is not available (e.g. SQLite).
var sqliteCountableTables = map[string]bool{
	"job_queue_v2": true, "kv": true, "remote_hooks": true,
	"ollama_models": true, "llm_affinity_group": true, "llm_backends": true,
}

func (s *store) estimateCount(ctx context.Context, table string) (int64, error) {
	var count int64
	err := s.Exec.QueryRowContext(ctx, `
		SELECT estimate_row_count($1)
	`, table).Scan(&count)
	if err == nil {
		return count, nil
	}
	// SQLite has no estimate_row_count; fall back to COUNT(*) for whitelisted tables only.
	if !strings.Contains(err.Error(), "no such function") {
		return 0, err
	}
	if !sqliteCountableTables[table] {
		return 0, err
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

func quiet() func() {
	null, _ := os.Open(os.DevNull)
	sout := os.Stdout
	serr := os.Stderr
	os.Stdout = null
	os.Stderr = null
	log.SetOutput(null)
	return func() {
		defer null.Close()
		os.Stdout = sout
		os.Stderr = serr
		log.SetOutput(os.Stderr)
	}
}

// setupStore initializes a test Postgres instance and returns the store.
func SetupStore(t *testing.T) (context.Context, Store) {
	t.Helper()

	// Silence logs
	unquiet := quiet()
	t.Cleanup(unquiet)

	ctx := context.TODO()
	connStr, _, cleanup, err := libdb.SetupLocalInstance(ctx, "test", "test", "test")
	require.NoError(t, err)

	dbManager, err := libdb.NewPostgresDBManager(ctx, connStr, Schema)
	require.NoError(t, err)

	// Cleanup DB and container
	t.Cleanup(func() {
		require.NoError(t, dbManager.Close())
		cleanup()
	})

	s := New(dbManager.WithoutTransaction())
	return ctx, s
}
