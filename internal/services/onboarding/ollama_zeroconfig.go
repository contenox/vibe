// Package onboarding is beam's first-run zero-config path: the mutating
// sibling to setupcheck.EnrichResultWithOllamaProbe that registers a probed
// local Ollama backend and sets it as default. It lives outside setupcheck
// because every exported setupcheck decision is documented "no I/O" —
// registering a backend and persisting defaults is a different contract.
package onboarding

import (
	"context"
	"fmt"
	"strings"

	"github.com/contenox/contenox/internal/models/backendservice"
	"github.com/contenox/contenox/internal/models/modelrepo"
	"github.com/contenox/contenox/libdbexec"
	// Blank-imported so the "ollama" catalog provider is registered even if
	// no other import already pulled it in; production wiring does this
	// transitively, but this package should not depend on that surviving refactors.
	_ "github.com/contenox/contenox/internal/models/modelrepo/ollama"
	"github.com/contenox/contenox/internal/services/clikv"
	"github.com/contenox/contenox/internal/services/setupcheck"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	"github.com/google/uuid"
)

// OllamaModel is one model summarized from a zero-config probe of local
// Ollama.
type OllamaModel struct {
	Name    string
	CanChat bool
}

// OllamaProbe is what the zero-config path learns about local Ollama before
// deciding whether to auto-register it: a superset of
// setupcheck.ProbeLocalOllamaAPI that also learns which served models are
// chat-capable, so Decide can require at least one before firing.
type OllamaProbe struct {
	// Reachable mirrors setupcheck.ProbeLocalOllamaAPI's ok return.
	Reachable bool
	// BaseURL is the resolved Ollama HTTP base; empty only when unreachable.
	BaseURL string
	// Models is every observed model, enriched with capability truth from /api/show.
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

// ProbeOllamaModels probes local Ollama (OLLAMA_HOST, or the
// 127.0.0.1:11434 default) and summarizes which served models are
// chat-capable, using the same catalog machinery the real backend sync
// uses. An unreachable daemon, or a listing/describe error, degrades to a
// probe with no chat models rather than an error return.
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

// IsVirginInstall reports whether res describes an install worth probing
// for zero-config: no backend registered, and no default model or provider set.
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

// Decide is the zero-config firing rule: fires only when the install is
// virgin (IsVirginInstall) and the Ollama probe reached a daemon reporting
// at least one chat-capable model. Pure — no I/O — so it is table-testable.
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

// chooseModel prefers setupcheck.DefaultOllamaSuggestModel when served,
// otherwise the first chat-capable model in probe order.
func chooseModel(chatModels []string) string {
	for _, m := range chatModels {
		if m == setupcheck.DefaultOllamaSuggestModel {
			return m
		}
	}
	return chatModels[0]
}

// Apply performs the zero-config registration once Decide says to fire: it
// registers a local Ollama backend through backendservice.Create (the same
// path `contenox backend add` uses), then writes
// default-provider/default-model through clikv.WriteConfig. It does not
// recompute setupcheck.Result or rebuild any engine; callers do that after
// Apply returns nil.
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
	// Workspace ID "": default-model/default-provider are global keys, and the
	// empty string is the canonical no-workspace spelling (scopeFor discards
	// the dimension for global keys either way; "" cannot leave a stray row).
	if err := clikv.WriteConfig(ctx, store, "", "default-provider", "ollama"); err != nil {
		return fmt.Errorf("set default-provider: %w", err)
	}
	if err := clikv.WriteConfig(ctx, store, "", "default-model", model); err != nil {
		return fmt.Errorf("set default-model: %w", err)
	}
	return nil
}

// probeFunc is the shape ProbeOllamaModels satisfies; TryZeroConfig takes
// it as a parameter so tests can inject a canned probe outcome.
type probeFunc func(ctx context.Context) OllamaProbe

// TryZeroConfig is first-run's zero-keystroke path: when res looks like a
// virgin install, it probes local Ollama and, only if Decide says to fire,
// registers it via Apply. A non-virgin install touches nothing; an
// unreachable probe or one with no chat-capable model probes but still
// touches nothing. The caller owns recomputing setupcheck.Result and
// rebuilding its engine afterwards.
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
