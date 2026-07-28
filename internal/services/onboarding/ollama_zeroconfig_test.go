package onboarding

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/contenox/beam/internal/libdbexec"
	"github.com/contenox/beam/internal/services/clikv"
	"github.com/contenox/beam/internal/services/setupcheck"
	"github.com/contenox/beam/internal/store/runtimetypes"
)

// TestDecide pins the pure decision table: only a virgin install with a reachable, chat-capable probe fires.
func TestDecide(t *testing.T) {
	t.Parallel()

	virgin := setupcheck.Result{BackendCount: 0}
	reachableWithChat := OllamaProbe{
		Reachable: true,
		BaseURL:   "http://127.0.0.1:11434",
		Models: []OllamaModel{
			{Name: "nomic-embed-text:latest", CanChat: false},
			{Name: "qwen2.5:7b", CanChat: true},
		},
	}

	cases := []struct {
		name      string
		res       setupcheck.Result
		probe     OllamaProbe
		wantFire  bool
		wantModel string
	}{
		{
			name:      "virgin install and ollama serving a chat model fires",
			res:       virgin,
			probe:     reachableWithChat,
			wantFire:  true,
			wantModel: "qwen2.5:7b",
		},
		{
			name:     "existing backend never fires",
			res:      setupcheck.Result{BackendCount: 1},
			probe:    reachableWithChat,
			wantFire: false,
		},
		{
			name:     "default model already set never fires",
			res:      setupcheck.Result{BackendCount: 0, DefaultModel: "gpt-5-mini"},
			probe:    reachableWithChat,
			wantFire: false,
		},
		{
			name:     "default provider already set never fires",
			res:      setupcheck.Result{BackendCount: 0, DefaultProvider: "openai"},
			probe:    reachableWithChat,
			wantFire: false,
		},
		{
			name:     "unreachable probe never fires",
			res:      virgin,
			probe:    OllamaProbe{Reachable: false},
			wantFire: false,
		},
		{
			name: "reachable but no models never fires",
			res:  virgin,
			probe: OllamaProbe{
				Reachable: true,
				BaseURL:   "http://127.0.0.1:11434",
			},
			wantFire: false,
		},
		{
			name: "reachable but only embedding models never fires",
			res:  virgin,
			probe: OllamaProbe{
				Reachable: true,
				BaseURL:   "http://127.0.0.1:11434",
				Models: []OllamaModel{
					{Name: "nomic-embed-text:latest", CanChat: false},
					{Name: "all-minilm:latest", CanChat: false},
				},
			},
			wantFire: false,
		},
		{
			name: "prefers DefaultOllamaSuggestModel among served chat models",
			res:  virgin,
			probe: OllamaProbe{
				Reachable: true,
				BaseURL:   "http://127.0.0.1:11434",
				Models: []OllamaModel{
					{Name: "llama3.1:8b", CanChat: true},
					{Name: setupcheck.DefaultOllamaSuggestModel, CanChat: true},
				},
			},
			wantFire:  true,
			wantModel: setupcheck.DefaultOllamaSuggestModel,
		},
		{
			name: "falls back to first chat model when the suggested one is absent",
			res:  virgin,
			probe: OllamaProbe{
				Reachable: true,
				BaseURL:   "http://127.0.0.1:11434",
				Models: []OllamaModel{
					{Name: "llama3.1:8b", CanChat: true},
					{Name: "mistral:7b", CanChat: true},
				},
			},
			wantFire:  true,
			wantModel: "llama3.1:8b",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := Decide(tc.res, tc.probe)
			if got.Fire != tc.wantFire {
				t.Fatalf("Fire = %v, want %v", got.Fire, tc.wantFire)
			}
			if tc.wantFire && got.Model != tc.wantModel {
				t.Fatalf("Model = %q, want %q", got.Model, tc.wantModel)
			}
			if !tc.wantFire && got.Model != "" {
				t.Fatalf("non-firing Decision must not name a model, got %q", got.Model)
			}
		})
	}
}

func TestIsVirginInstall(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		res  setupcheck.Result
		want bool
	}{
		{"empty result is virgin", setupcheck.Result{}, true},
		{"backend present is not virgin", setupcheck.Result{BackendCount: 1}, false},
		{"default model present is not virgin", setupcheck.Result{DefaultModel: "x"}, false},
		{"default provider present is not virgin", setupcheck.Result{DefaultProvider: "x"}, false},
		{"whitespace-only defaults still virgin", setupcheck.Result{DefaultModel: "  ", DefaultProvider: "\t"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := IsVirginInstall(tc.res); got != tc.want {
				t.Fatalf("IsVirginInstall(%+v) = %v, want %v", tc.res, got, tc.want)
			}
		})
	}
}

func TestChatModels(t *testing.T) {
	t.Parallel()
	p := OllamaProbe{Models: []OllamaModel{
		{Name: "embed", CanChat: false},
		{Name: "chatty", CanChat: true},
	}}
	got := p.ChatModels()
	if len(got) != 1 || got[0] != "chatty" {
		t.Fatalf("ChatModels() = %v, want [chatty]", got)
	}
}

// setupOnboardingDB opens a throwaway SQLite DB so Apply/tryZeroConfig can
// run against real store code rather than a mock.
func setupOnboardingDB(t *testing.T) (context.Context, libdbexec.DBManager) {
	t.Helper()
	ctx := context.Background()
	db, err := libdbexec.NewSQLiteDBManager(ctx, filepath.Join(t.TempDir(), "onboarding.db"), runtimetypes.SchemaSQLite)
	if err != nil {
		t.Fatalf("open sqlite db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return ctx, db
}

// TestApply_HappyPath pins that Apply registers exactly one ollama backend and persists default-provider/default-model.
func TestApply_HappyPath(t *testing.T) {
	t.Parallel()
	ctx, db := setupOnboardingDB(t)

	probe := OllamaProbe{
		Reachable: true,
		BaseURL:   "http://127.0.0.1:11434",
		Models:    []OllamaModel{{Name: "qwen2.5:7b", CanChat: true}},
	}
	if err := Apply(ctx, db, probe, "qwen2.5:7b"); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	store := runtimetypes.New(db.WithoutTransaction())
	backends, err := store.ListBackends(ctx, nil, runtimetypes.MAXLIMIT)
	if err != nil {
		t.Fatalf("ListBackends: %v", err)
	}
	if len(backends) != 1 {
		t.Fatalf("expected exactly 1 registered backend, got %d", len(backends))
	}
	b := backends[0]
	if b.Type != "ollama" || b.Name != "ollama" || b.BaseURL != probe.BaseURL {
		t.Fatalf("unexpected backend: %+v", b)
	}

	if got := clikv.Read(ctx, store, "default-provider"); got != "ollama" {
		t.Fatalf("default-provider = %q, want ollama", got)
	}
	if got := clikv.Read(ctx, store, "default-model"); got != "qwen2.5:7b" {
		t.Fatalf("default-model = %q, want qwen2.5:7b", got)
	}
}

func TestApply_RejectsEmptyModelOrBaseURL(t *testing.T) {
	t.Parallel()
	ctx, db := setupOnboardingDB(t)

	if err := Apply(ctx, db, OllamaProbe{BaseURL: "http://127.0.0.1:11434"}, ""); err == nil {
		t.Fatal("expected error for empty model")
	}
	if err := Apply(ctx, db, OllamaProbe{}, "qwen2.5:7b"); err == nil {
		t.Fatal("expected error for empty base URL")
	}

	store := runtimetypes.New(db.WithoutTransaction())
	backends, err := store.ListBackends(ctx, nil, runtimetypes.MAXLIMIT)
	if err != nil {
		t.Fatalf("ListBackends: %v", err)
	}
	if len(backends) != 0 {
		t.Fatalf("rejected Apply must not register a backend, got %d", len(backends))
	}
}

// TestTryZeroConfig_NonVirginNeverProbes pins that the virgin-install gate runs before any I/O, including the probe itself.
func TestTryZeroConfig_NonVirginNeverProbes(t *testing.T) {
	t.Parallel()
	ctx, db := setupOnboardingDB(t)

	called := false
	probe := func(context.Context) OllamaProbe {
		called = true
		return OllamaProbe{Reachable: true, BaseURL: "http://127.0.0.1:11434", Models: []OllamaModel{{Name: "x", CanChat: true}}}
	}

	decision, err := tryZeroConfig(ctx, db, setupcheck.Result{BackendCount: 1}, probe)
	if err != nil {
		t.Fatalf("tryZeroConfig: %v", err)
	}
	if decision.Fire {
		t.Fatal("non-virgin install must never fire")
	}
	if called {
		t.Fatal("non-virgin install must never probe")
	}

	store := runtimetypes.New(db.WithoutTransaction())
	backends, err := store.ListBackends(ctx, nil, runtimetypes.MAXLIMIT)
	if err != nil {
		t.Fatalf("ListBackends: %v", err)
	}
	if len(backends) != 0 {
		t.Fatalf("must not register a backend, got %d", len(backends))
	}
}

// TestTryZeroConfig_InjectedProbe_HappyPath pins the full probe->decide->apply orchestration with an injected prober.
func TestTryZeroConfig_InjectedProbe_HappyPath(t *testing.T) {
	t.Parallel()
	ctx, db := setupOnboardingDB(t)

	probe := func(context.Context) OllamaProbe {
		return OllamaProbe{
			Reachable: true,
			BaseURL:   "http://127.0.0.1:11434",
			Models: []OllamaModel{
				{Name: "nomic-embed-text:latest", CanChat: false},
				{Name: "qwen2.5:7b", CanChat: true},
			},
		}
	}

	decision, err := tryZeroConfig(ctx, db, setupcheck.Result{}, probe)
	if err != nil {
		t.Fatalf("tryZeroConfig: %v", err)
	}
	if !decision.Fire || decision.Model != "qwen2.5:7b" {
		t.Fatalf("decision = %+v, want Fire=true Model=qwen2.5:7b", decision)
	}

	store := runtimetypes.New(db.WithoutTransaction())
	backends, err := store.ListBackends(ctx, nil, runtimetypes.MAXLIMIT)
	if err != nil {
		t.Fatalf("ListBackends: %v", err)
	}
	if len(backends) != 1 || backends[0].Type != "ollama" {
		t.Fatalf("expected 1 registered ollama backend, got %+v", backends)
	}
	if got := clikv.Read(ctx, store, "default-provider"); got != "ollama" {
		t.Fatalf("default-provider = %q", got)
	}
	if got := clikv.Read(ctx, store, "default-model"); got != "qwen2.5:7b" {
		t.Fatalf("default-model = %q", got)
	}
}

// TestTryZeroConfig_InjectedProbe_NoChatModelDoesNothing pins that "Ollama up but no chat models" never registers anything, even on a virgin install.
func TestTryZeroConfig_InjectedProbe_NoChatModelDoesNothing(t *testing.T) {
	t.Parallel()
	ctx, db := setupOnboardingDB(t)

	probe := func(context.Context) OllamaProbe {
		return OllamaProbe{Reachable: true, BaseURL: "http://127.0.0.1:11434"}
	}

	decision, err := tryZeroConfig(ctx, db, setupcheck.Result{}, probe)
	if err != nil {
		t.Fatalf("tryZeroConfig: %v", err)
	}
	if decision.Fire {
		t.Fatal("no chat models must never fire")
	}

	store := runtimetypes.New(db.WithoutTransaction())
	backends, err := store.ListBackends(ctx, nil, runtimetypes.MAXLIMIT)
	if err != nil {
		t.Fatalf("ListBackends: %v", err)
	}
	if len(backends) != 0 {
		t.Fatalf("must not register a backend, got %d", len(backends))
	}
}

// fakeOllamaServer stands in for a local Ollama daemon: GET /api/tags lists
// models, POST /api/show reports capabilities for one of them.
func fakeOllamaServer(t *testing.T, models []struct {
	name         string
	capabilities []string
}) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/tags", func(w http.ResponseWriter, r *http.Request) {
		type listModel struct {
			Name  string `json:"name"`
			Model string `json:"model"`
		}
		resp := struct {
			Models []listModel `json:"models"`
		}{}
		for _, m := range models {
			resp.Models = append(resp.Models, listModel{Name: m.name, Model: m.name})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	})
	mux.HandleFunc("/api/show", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		var caps []string
		for _, m := range models {
			if m.name == req.Model {
				caps = m.capabilities
				break
			}
		}
		resp := struct {
			Capabilities []string       `json:"capabilities"`
			ModelInfo    map[string]any `json:"model_info"`
		}{Capabilities: caps, ModelInfo: map[string]any{}}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// TestProbeOllamaModels_RealHTTPAgainstFakeServer pins that ProbeOllamaModels correctly separates a chat-capable model from an embedding-only one over real HTTP.
func TestProbeOllamaModels_RealHTTPAgainstFakeServer(t *testing.T) {
	srv := fakeOllamaServer(t, []struct {
		name         string
		capabilities []string
	}{
		{name: "nomic-embed-text:latest", capabilities: []string{"embedding"}},
		{name: "qwen2.5:7b", capabilities: []string{"completion", "tools"}},
	})
	t.Setenv("OLLAMA_HOST", srv.URL)

	probe := ProbeOllamaModels(context.Background())
	if !probe.Reachable {
		t.Fatal("expected reachable probe")
	}
	chatModels := probe.ChatModels()
	if len(chatModels) != 1 || chatModels[0] != "qwen2.5:7b" {
		t.Fatalf("ChatModels() = %v, want [qwen2.5:7b]", chatModels)
	}

	decision := Decide(setupcheck.Result{}, probe)
	if !decision.Fire || decision.Model != "qwen2.5:7b" {
		t.Fatalf("decision = %+v, want Fire=true Model=qwen2.5:7b", decision)
	}
}

// TestTryZeroConfig_EndToEndAgainstFakeServer pins TryZeroConfig's public entry point end to end against a fake server and a real SQLite DB.
func TestTryZeroConfig_EndToEndAgainstFakeServer(t *testing.T) {
	srv := fakeOllamaServer(t, []struct {
		name         string
		capabilities []string
	}{
		{name: "qwen2.5:7b", capabilities: []string{"completion"}},
	})
	t.Setenv("OLLAMA_HOST", srv.URL)
	ctx, db := setupOnboardingDB(t)

	decision, err := TryZeroConfig(ctx, db, setupcheck.Result{})
	if err != nil {
		t.Fatalf("TryZeroConfig: %v", err)
	}
	if !decision.Fire || decision.Model != "qwen2.5:7b" {
		t.Fatalf("decision = %+v", decision)
	}

	store := runtimetypes.New(db.WithoutTransaction())
	backends, err := store.ListBackends(ctx, nil, runtimetypes.MAXLIMIT)
	if err != nil {
		t.Fatalf("ListBackends: %v", err)
	}
	if len(backends) != 1 {
		t.Fatalf("expected 1 backend, got %d", len(backends))
	}
	if !strings.EqualFold(backends[0].Type, "ollama") {
		t.Fatalf("unexpected backend type %q", backends[0].Type)
	}
}
