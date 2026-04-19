package chatsessionmodes

import (
	"strings"

	"github.com/contenox/contenox/runtime/planservice"
)

// ModeRegistry lists injectors to run in order for a given mode id (lowercased).
type ModeRegistry struct {
	byMode map[string][]Injector
}

// NewDefaultModeRegistry wires standard injectors: plan mode prepends ActivePlanInjector when planSvc is non-nil.
func NewDefaultModeRegistry(planSvc planservice.Service) *ModeRegistry {
	client := ClientArtifactInjector{}
	planInj := &ActivePlanInjector{Plans: planSvc}

	r := &ModeRegistry{byMode: make(map[string][]Injector)}
	// chat / prompt / build: client artifacts only (build compiles active plan in code)
	r.byMode["chat"] = []Injector{client}
	r.byMode["prompt"] = []Injector{client}
	r.byMode["build"] = []Injector{client}
	// plan: active plan snapshot first, then client artifacts
	if planSvc != nil {
		r.byMode["plan"] = []Injector{planInj, client}
	} else {
		r.byMode["plan"] = []Injector{client}
	}
	return r
}

// Injectors returns injectors for mode, defaulting to chat when unknown.
func (r *ModeRegistry) Injectors(mode string) []Injector {
	if r == nil {
		return []Injector{ClientArtifactInjector{}}
	}
	m := strings.TrimSpace(strings.ToLower(mode))
	if m == "" {
		m = "chat"
	}
	if inj, ok := r.byMode[m]; ok {
		out := make([]Injector, len(inj))
		copy(out, inj)
		return out
	}
	// Unknown mode: still allow client artifacts (resolver will have failed earlier if chain missing)
	if inj, ok := r.byMode["chat"]; ok {
		return inj
	}
	return []Injector{ClientArtifactInjector{}}
}
