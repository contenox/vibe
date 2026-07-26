package tooleval

import (
	"context"
	"fmt"

	"github.com/contenox/beam/internal/kernel/enginesvc"
	libdb "github.com/contenox/beam/internal/libdbexec"
	"github.com/contenox/beam/internal/libtracker"
	"github.com/contenox/beam/internal/models/llmrepo"
	modelrepo "github.com/contenox/beam/internal/models/modelrepo"
	"github.com/contenox/beam/internal/models/ollamatokenizer"
	"github.com/contenox/beam/internal/store/runtimetypes"
)

// EngineModel is the REAL-model implementation of the Model seam. It stands up the
// same engine the acp path uses (enginesvc.Build) and drives its configured provider
// through llmrepo — one chat turn per Turn, with the tools advertised and tool calls
// parsed exactly as the engine would. It is the "live-model mode for the matrix" half
// of the seam; the scripted responders are the "replayed-response mode for
// determinism" half. The runner does not care which it is handed.
type EngineModel struct {
	name     string
	provider string
	repo     llmrepo.ModelRepo
	ctxLen   int
	seed     *int
	temp     *float64
	stop     func()
}

// EngineModelConfig configures the real-model seam. Model/Provider are the default
// provider/model the engine configures (e.g. qwen2.5:0.5b / ollama for the
// maintainer's local box); Seed/Temperature are forwarded to the provider where it
// honors them and recorded for determinism honesty.
type EngineModelConfig struct {
	Model    string
	Provider string
	// BaseURL is the backend URL to register before the engine's backend cycle runs,
	// so a fresh harness DB has a backend to discover models on (a bare engine build
	// over an empty DB has no backend and every resolution fails). Empty defaults to
	// the local ollama URL. Set it empty AND pre-register your own backend to opt out.
	BaseURL       string
	ContextLength int
	Seed          *int
	Temperature   *float64
}

const (
	defaultEngineCtxLen = 8192
	defaultOllamaURL    = "http://127.0.0.1:11434"
)

// NewEngineModel registers a backend, builds the engine (whose backend cycle then
// discovers the backend's models), and stands up a chat-capable llmrepo over its
// runtime state. The caller owns db; Close tears down the engine this created. It does
// NOT verify a backend is reachable — call Probe for that (so a caller can skip
// cleanly with a teaching message when no model is up).
func NewEngineModel(ctx context.Context, db libdb.DBManager, cfg EngineModelConfig) (*EngineModel, error) {
	ctxLen := cfg.ContextLength
	if ctxLen <= 0 {
		ctxLen = defaultEngineCtxLen
	}
	baseURL := cfg.BaseURL
	if baseURL == "" {
		baseURL = defaultOllamaURL
	}
	// Register the backend up front so Build's backend cycle has a URL to scan; on a
	// fresh temp DB there is otherwise nothing to discover.
	store := runtimetypes.New(db.WithoutTransaction())
	if err := store.CreateBackend(ctx, &runtimetypes.Backend{
		Name:    cfg.Provider,
		Type:    cfg.Provider,
		BaseURL: baseURL,
	}); err != nil {
		return nil, fmt.Errorf("tooleval: register backend: %w", err)
	}
	eng, err := enginesvc.Build(ctx, db, enginesvc.Config{
		DefaultModel:             cfg.Model,
		DefaultProvider:          cfg.Provider,
		ReadinessDefaultModel:    cfg.Model,
		ReadinessDefaultProvider: cfg.Provider,
		ContextLength:            ctxLen,
		NoDeleteModels:           true,
	})
	if err != nil {
		return nil, fmt.Errorf("tooleval: build engine: %w", err)
	}
	repo, err := llmrepo.NewModelManager(eng.State, ollamatokenizer.NewEstimateTokenizer(), llmrepo.ModelManagerConfig{
		DefaultPromptModel:    llmrepo.ModelConfig{Name: cfg.Model, Provider: cfg.Provider},
		DefaultEmbeddingModel: llmrepo.ModelConfig{Name: cfg.Model, Provider: cfg.Provider},
		DefaultChatModel:      llmrepo.ModelConfig{Name: cfg.Model, Provider: cfg.Provider},
	}, libtracker.NoopTracker{})
	if err != nil {
		eng.Stop()
		return nil, fmt.Errorf("tooleval: build model manager: %w", err)
	}
	return &EngineModel{
		name:     cfg.Model,
		provider: cfg.Provider,
		repo:     repo,
		ctxLen:   ctxLen,
		seed:     cfg.Seed,
		temp:     cfg.Temperature,
		stop:     eng.Stop,
	}, nil
}

func (m *EngineModel) Name() string { return m.name }

// Close stops the underlying engine.
func (m *EngineModel) Close() {
	if m.stop != nil {
		m.stop()
	}
}

// Probe runs a minimal chat to confirm a backend/model is actually reachable, so a
// gated test can Skip cleanly rather than fail when nothing is up.
func (m *EngineModel) Probe(ctx context.Context) error {
	_, _, err := m.repo.Chat(ctx, m.request(), []modelrepo.Message{{Role: "user", Content: "ping"}})
	return err
}

func (m *EngineModel) request() llmrepo.Request {
	return llmrepo.Request{
		ModelNames:    []string{m.name},
		ProviderTypes: []string{m.provider},
		ContextLength: m.ctxLen,
	}
}

// Turn runs one real chat turn: advertise the tools, forward seed/temperature, and map
// the provider's response (message text plus tool calls) back into the harness shape.
func (m *EngineModel) Turn(ctx context.Context, convo []Message, tools []ToolSpec) (Assistant, error) {
	msgs := toProviderMessages(convo)

	var args []modelrepo.ChatArgument
	if len(tools) > 0 {
		pts := make([]modelrepo.Tool, 0, len(tools))
		for _, t := range tools {
			pts = append(pts, modelrepo.Tool{
				Type:     "function",
				Function: &modelrepo.FunctionTool{Name: t.Name, Description: t.Description, Parameters: t.Parameters},
			})
		}
		args = append(args, modelrepo.WithTools(pts...))
	}
	if m.temp != nil {
		args = append(args, modelrepo.WithTemperature(*m.temp))
	}
	if m.seed != nil {
		args = append(args, modelrepo.WithSeed(*m.seed))
	}

	res, _, err := m.repo.Chat(ctx, m.request(), msgs, args...)
	if err != nil {
		return Assistant{}, err
	}

	a := Assistant{Content: res.Message.Content}
	// Providers put tool calls either at the top level of ChatResult or on the
	// message; prefer the top-level set and fall back to the message's.
	tcs := res.ToolCalls
	if len(tcs) == 0 {
		tcs = res.Message.ToolCalls
	}
	for _, tc := range tcs {
		a.ToolCalls = append(a.ToolCalls, ToolCall{
			ID:        tc.ID,
			Name:      tc.Function.Name,
			Arguments: tc.Function.Arguments,
		})
	}
	return a, nil
}

// toProviderMessages maps the harness conversation into modelrepo messages, preserving
// assistant tool calls and tool-result linkage so the provider sees a coherent history.
func toProviderMessages(convo []Message) []modelrepo.Message {
	out := make([]modelrepo.Message, 0, len(convo))
	for _, msg := range convo {
		pm := modelrepo.Message{
			Role:       string(msg.Role),
			Content:    msg.Content,
			ToolCallID: msg.ToolCallID,
		}
		for _, tc := range msg.ToolCalls {
			var pc modelrepo.ToolCall
			pc.ID = tc.ID
			pc.Type = "function"
			pc.Function.Name = tc.Name
			pc.Function.Arguments = tc.Arguments
			pm.ToolCalls = append(pm.ToolCalls, pc)
		}
		out = append(out, pm)
	}
	return out
}
