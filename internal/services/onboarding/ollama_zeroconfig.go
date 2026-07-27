// Package onboarding is beam's first-run zero-config path (blueprint
// beam-tui.md section 3 item 7 / section 4.3): the mutating sibling to
// setupcheck.EnrichResultWithOllamaProbe that actually registers a probed
// local Ollama backend and sets it as default, instead of only rewriting an
// Issue's text.
//
// It deliberately does NOT live inside internal/services/setupcheck. Every
// exported decision function there (Evaluate, BlockingIssues, Ready,
// Summary) is documented "no I/O", and setupcheck's only I/O helpers —
// GatherInput, ProbeLocalOllamaAPI, EnrichResultWithOllamaProbe — are all
// reads: a DB read, an HTTP GET, and a Result field rewrite, never a write
// to the store. Registering a backend (backendservice.Create) and
// persisting defaults (clikv.WriteConfig) is a categorically different
// contract; a package whose entire existing surface promises "never
// mutates" is the wrong place to quietly add the one function that does.
//
// It is also not the larger onboarding/apply extraction blueprint section 3
// item 6 describes (moving registerSetupBackend, the setupProviders menu,
// and runSetup's KV-persist tail out of internal/surfaces/contenoxcli so
// both the CLI and beam can share them). That is a separate, bigger move
// this package deliberately does not make — it is scoped to exactly the
// zero-config local-Ollama path, small enough to sit beside backendservice
// in spirit without actually widening backendservice's own contract either
// (backendservice is a plain CRUD service over one runtimetypes.Backend; it
// has no business knowing about setupcheck.Result, probes, or firing
// rules).
package onboarding

import (
	"context"
	"fmt"
	"strings"

	"github.com/contenox/beam/internal/libdbexec"
	"github.com/contenox/beam/internal/models/backendservice"
	"github.com/contenox/beam/internal/models/modelrepo"
	// Blank-imported so the "ollama" catalog provider is registered even if
	// no other import in the test/binary's dependency graph already pulled
	// it in. In production this is always redundant (runtimestate's
	// catalogimports.go already does it, and setupcheck imports
	// runtimestate), but this package's own correctness should not depend
	// on that transitive detail surviving future refactors.
	_ "github.com/contenox/beam/internal/models/modelrepo/ollama"
	"github.com/contenox/beam/internal/services/clikv"
	"github.com/contenox/beam/internal/services/setupcheck"
	"github.com/contenox/beam/internal/store/runtimetypes"
	"github.com/google/uuid"
)

// OllamaModel is one model summarized from a zero-config probe of local
// Ollama.
type OllamaModel struct {
	Name    string
	CanChat bool
}

// OllamaProbe is what the zero-config path learns about local Ollama before
// deciding whether to auto-register it. It is a superset of
// setupcheck.ProbeLocalOllamaAPI: that call only confirms /api/tags answers;
// this one also learns which served models are chat-capable, so Decide can
// require at least one before firing (blueprint 4.3: "Ollama up but no
// models" must never auto-fire).
type OllamaProbe struct {
	// Reachable mirrors setupcheck.ProbeLocalOllamaAPI's ok return.
	Reachable bool
	// BaseURL is the resolved Ollama HTTP base (OLLAMA_HOST or the
	// 127.0.0.1:11434 default) — empty only when Reachable is false and
	// even the URL failed to resolve.
	BaseURL string
	// Models is every model observed via /api/tags, enriched with
	// capability truth from /api/show. Empty when unreachable, or when
	// listing/describing failed after tags responded.
	Models []OllamaModel
}

// ChatModels returns the served model names this probe judged chat-capable,
// in probe (i.e. /api/tags) order.
func (p OllamaProbe) ChatModels() []string {
	var names []string
	for _, m := range p.Models {
		if m.CanChat {
			names = append(names, m.Name)
		}
	}
	return names
}

// ProbeOllamaModels probes local Ollama (OLLAMA_HOST, or the default
// 127.0.0.1:11434 — the same resolution setupcheck.ProbeLocalOllamaAPI uses)
// and summarizes which served models are chat-capable.
//
// Capability comes from the same catalog machinery the real backend sync
// uses (modelrepo.NewCatalogProvider + the "ollama" vendor's /api/show
// lookup, internal/models/modelrepo/ollama/catalog.go) rather than a
// name-based guess — so this probe's verdict agrees with what BuildEngine's
// own sync will find a moment later, and a fired zero-config path does not
// turn around and fail readiness anyway.
//
// An unreachable daemon, or an error while listing or describing its
// models, degrades to a probe with no chat models rather than an error
// return: Decide already treats "found nothing" as "do not fire", so there
// is no separate error path a first-run gate needs to render.
func ProbeOllamaModels(ctx context.Context) OllamaProbe {
	base, ok := setupcheck.ProbeLocalOllamaAPI(ctx)
	if !ok {
		return OllamaProbe{BaseURL: base}
	}

	provider, err := modelrepo.NewCatalogProvider(modelrepo.BackendSpec{Type: "ollama", BaseURL: base})
	if err != nil {
		return OllamaProbe{Reachable: true, BaseURL: base}
	}
	observed, err := provider.ListModels(ctx)
	if err != nil {
		return OllamaProbe{Reachable: true, BaseURL: base}
	}

	models := make([]OllamaModel, 0, len(observed))
	for _, m := range observed {
		models = append(models, OllamaModel{Name: m.Name, CanChat: m.CanChat})
	}
	return OllamaProbe{Reachable: true, BaseURL: base, Models: models}
}

// IsVirginInstall reports whether res describes an install even worth
// probing for the zero-config path: no backend registered yet, and no
// default model or provider set. This check runs — and must return true —
// before any Ollama probe: blueprint beam-tui.md 4.3's acceptance criteria
// requires "the silent path never touches an install with existing
// backends [or defaults]", and skipping the probe entirely is how a
// broken-but-configured install stays untouched, not just unregistered.
func IsVirginInstall(res setupcheck.Result) bool {
	return res.BackendCount == 0 &&
		strings.TrimSpace(res.DefaultModel) == "" &&
		strings.TrimSpace(res.DefaultProvider) == ""
}

// Decision is Decide's verdict: whether the zero-config path should fire,
// and — only when Fire is true — which served model to set as the default.
type Decision struct {
	Fire  bool
	Model string
}

// Decide is the zero-config firing rule from blueprint beam-tui.md section
// 4.3: fires only when the install is virgin (IsVirginInstall) AND the
// Ollama probe both reached a daemon and reported at least one chat-capable
// model. Pure — no I/O — so it is table-testable against canned
// Result/OllamaProbe values without a live Ollama daemon.
func Decide(res setupcheck.Result, probe OllamaProbe) Decision {
	if !IsVirginInstall(res) {
		return Decision{}
	}
	if !probe.Reachable {
		return Decision{}
	}
	chatModels := probe.ChatModels()
	if len(chatModels) == 0 {
		return Decision{}
	}
	return Decision{Fire: true, Model: chooseModel(chatModels)}
}

// chooseModel prefers setupcheck.DefaultOllamaSuggestModel when it is one of
// the served chat models (the same model `contenox setup`'s Ollama path
// defaults to), and otherwise falls back to the first chat-capable model in
// probe order.
func chooseModel(chatModels []string) string {
	for _, m := range chatModels {
		if m == setupcheck.DefaultOllamaSuggestModel {
			return m
		}
	}
	return chatModels[0]
}

// Apply performs the zero-config registration once Decide has said to fire:
// it registers a local Ollama backend through the exact same
// backendservice.Create path `contenox backend add ollama --type ollama`
// and the setup wizard's registerSetupBackend both use (respectively
// internal/surfaces/contenoxcli/backend_cmd.go and setup_cmd.go) — no
// second Create-backend implementation — then writes
// default-provider/default-model through the same clikv.WriteConfig path
// `contenox config set` uses.
//
// It does not recompute setupcheck.Result or rebuild any engine: this
// package has no opinion on what "recompute" means for a given caller
// (beam's composition root rebuilds its own in-process engine; a future ACP
// caller might not). Callers do that themselves after Apply returns nil.
func Apply(ctx context.Context, db libdbexec.DBManager, probe OllamaProbe, model string) error {
	model = strings.TrimSpace(model)
	if model == "" {
		return fmt.Errorf("onboarding: cannot register local ollama without a model")
	}
	baseURL := strings.TrimSpace(probe.BaseURL)
	if baseURL == "" {
		return fmt.Errorf("onboarding: cannot register local ollama without a probed base URL")
	}

	svc := backendservice.New(db)
	backend := &runtimetypes.Backend{
		ID:      uuid.NewString(),
		Name:    "ollama",
		Type:    "ollama",
		BaseURL: baseURL,
	}
	if err := svc.Create(ctx, backend); err != nil {
		return fmt.Errorf("register local ollama backend: %w", err)
	}

	store := runtimetypes.New(db.WithoutTransaction())
	// "global": default-model/default-provider are not workspace-scoped
	// keys (clikv.go's workspaceScopedKeys), so this matches WriteConfig's
	// own fallback exactly — same as setup_cmd.go's runSetup tail.
	if err := clikv.WriteConfig(ctx, store, "global", "default-provider", "ollama"); err != nil {
		return fmt.Errorf("set default-provider: %w", err)
	}
	if err := clikv.WriteConfig(ctx, store, "global", "default-model", model); err != nil {
		return fmt.Errorf("set default-model: %w", err)
	}
	return nil
}

// probeFunc is the shape ProbeOllamaModels satisfies. TryZeroConfig takes it
// as an internal parameter (always ProbeOllamaModels in production) so
// tests can inject a canned probe outcome instead of depending on a live
// Ollama daemon or an httptest double.
type probeFunc func(ctx context.Context) OllamaProbe

// TryZeroConfig is first-run's zero-keystroke path (blueprint beam-tui.md
// section 4.3): when res looks like a virgin install (IsVirginInstall), it
// probes local Ollama and — only if Decide says to fire — registers it via
// Apply and returns Decision{Fire: true, Model: <chosen>}. A non-virgin
// install skips the probe entirely and touches nothing; an unreachable
// probe, or a virgin install with no chat-capable model, probes but still
// touches nothing (Decision{}, nil).
//
// The caller (beam_cmd.go's gate) owns recomputing setupcheck.Result and
// rebuilding its engine afterwards — this function only decides and
// applies.
func TryZeroConfig(ctx context.Context, db libdbexec.DBManager, res setupcheck.Result) (Decision, error) {
	return tryZeroConfig(ctx, db, res, ProbeOllamaModels)
}

func tryZeroConfig(ctx context.Context, db libdbexec.DBManager, res setupcheck.Result, probe probeFunc) (Decision, error) {
	if !IsVirginInstall(res) {
		return Decision{}, nil
	}
	outcome := probe(ctx)
	decision := Decide(res, outcome)
	if !decision.Fire {
		return Decision{}, nil
	}
	if err := Apply(ctx, db, outcome, decision.Model); err != nil {
		return Decision{}, err
	}
	return decision, nil
}
