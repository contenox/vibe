package llmrepo

// bounds.go enforces the envelope's model/backend allowlist
// (hitlservice.ComputeBounds.ModelAllowlist / BackendAllowlist) at the one
// place a model and backend are actually chosen — every generative or
// embedding call funnels through llmresolver here. The check is a refusal
// before the call: it runs after resolution and before the provider call, so
// a refusal costs nothing but the resolution itself. Absent bounds are
// unbounded; bounds only ever restrict.

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	libmodelprovider "github.com/contenox/contenox/internal/models/modelrepo"
)

// ErrResolutionOutOfBounds is the sentinel every allowlist refusal wraps, so a
// caller can tell an envelope refusal apart from a resolution failure or a
// provider error: the resolver did its job and found a usable model — the
// envelope is what says no.
var ErrResolutionOutOfBounds = errors.New("resolution outside the mission envelope")

// resolutionBoundLead is the stable, greppable lead of every allowlist refusal
// reason. It mirrors fleetservice's computeBoundLead for the ceilings, so a
// compute-bound refusal reads the same way whichever bound produced it.
const resolutionBoundLead = "compute bound refused"

// ResolutionBounds is the envelope's model/backend allowlist as it travels to
// the resolution seam: the same two lists hitlservice.ComputeBounds declares,
// carried as a neutral value so this layer never imports a service.
//
// Both lists are opt-in allowlists: an empty list is unbounded for that
// dimension. Matching is exact after trimming, case-insensitively —
// deliberately not the resolver's NormalizeModelName, which strips version
// tags ("llama2:7b" and "llama2:70b" both collapse to "llama2"); a security
// boundary must not silently widen.
//
// The allowlist is total, not per-kind: it bounds chat, prompt, stream, and
// embed alike, since an embedding call spends the mission's compute too.
type ResolutionBounds struct {
	// Models bounds which model names may be resolved. Empty means unbounded.
	Models []string
	// Backends bounds which backends may serve them. An entry matches a
	// backend's operator-facing name (what `contenox backend list` shows) or
	// its id. Empty means unbounded.
	Backends []string
}

// IsZero reports whether these bounds restrict nothing, which is the case for
// every request that is not a bounded mission's.
func (b ResolutionBounds) IsZero() bool {
	return len(b.Models) == 0 && len(b.Backends) == 0
}

// resolutionBoundsContextKeyType is unexported so no other package can collide
// with the key; the only way in or out is WithResolutionBounds /
// ResolutionBoundsFromContext.
type resolutionBoundsContextKeyType struct{}

var resolutionBoundsContextKey resolutionBoundsContextKeyType

// WithResolutionBounds binds an envelope's model/backend allowlist to ctx, so
// every resolution made under it is held to the bound. Bounds that restrict
// nothing are not stored at all, keeping the unbounded path allocation-free.
// A second call replaces the bound rather than intersecting with it, since a
// silent merge would make the effective bound depend on call order.
func WithResolutionBounds(ctx context.Context, b ResolutionBounds) context.Context {
	if b.IsZero() {
		return ctx
	}
	return context.WithValue(ctx, resolutionBoundsContextKey, b)
}

// ResolutionBoundsFromContext returns the bound bound onto ctx, or the zero
// (unbounded) value when the request carries none.
func ResolutionBoundsFromContext(ctx context.Context) ResolutionBounds {
	b, _ := ctx.Value(resolutionBoundsContextKey).(ResolutionBounds)
	return b
}

// enforceResolutionBounds holds one resolved model/backend pair to the
// envelope bound on ctx. It runs after resolution and before the provider
// call, so a refusal costs nothing but the resolution itself. op names the
// operation in the refusal ("chat", "stream", "prompt", "embed").
func (e *modelManager) enforceResolutionBounds(ctx context.Context, op string, provider libmodelprovider.Provider, backendID string) error {
	bounds := ResolutionBoundsFromContext(ctx)
	if bounds.IsZero() {
		return nil
	}
	if len(bounds.Models) > 0 && provider != nil {
		model := provider.ModelName()
		if !allowlistContains(bounds.Models, model) {
			return outOfBoundsError(op, "modelAllowlist", model, bounds.Models)
		}
	}
	if len(bounds.Backends) > 0 {
		if !e.backendAllowed(ctx, bounds.Backends, backendID) {
			return outOfBoundsError(op, "backendAllowlist", e.backendLabel(ctx, backendID), bounds.Backends)
		}
	}
	return nil
}

// backendAllowed reports whether backendID names a backend the envelope
// permits, matching either its operator-facing name or its id. A backend the
// runtime state cannot name (an id with no state row) matches only by id —
// fail closed for a name-based allowlist, so a removed backend never silently
// slips through.
func (e *modelManager) backendAllowed(ctx context.Context, allowed []string, backendID string) bool {
	if allowlistContains(allowed, backendID) {
		return true
	}
	name := e.backendName(ctx, backendID)
	return name != "" && allowlistContains(allowed, name)
}

// backendName resolves a backend id to its operator-facing name from runtime
// state, or "" when the state has no row for it.
func (e *modelManager) backendName(ctx context.Context, backendID string) string {
	if backendID == "" || e.runtime == nil {
		return ""
	}
	for _, state := range e.runtime.Get(ctx) {
		if state.Backend.ID == backendID || state.ID == backendID {
			if state.Backend.Name != "" {
				return state.Backend.Name
			}
			return state.Name
		}
	}
	return ""
}

// backendLabel is what a refusal calls the backend it refused: its name when the
// runtime can name it, with the id alongside so the record is unambiguous, and
// the bare id otherwise.
func (e *modelManager) backendLabel(ctx context.Context, backendID string) string {
	if name := e.backendName(ctx, backendID); name != "" {
		return fmt.Sprintf("%s (%s)", name, backendID)
	}
	return backendID
}

// allowlistContains is the matching rule, in one place so models and backends
// cannot drift apart: trim, compare case-insensitively, require the whole
// string to match. No globs, no normalization, no prefixes.
func allowlistContains(allowed []string, value string) bool {
	v := strings.ToLower(strings.TrimSpace(value))
	if v == "" {
		return false
	}
	for _, a := range allowed {
		if strings.ToLower(strings.TrimSpace(a)) == v {
			return true
		}
	}
	return false
}

// outOfBoundsError builds the refusal. It names the bound, what the envelope
// permits, and what resolution actually picked — the three facts an operator
// needs to either widen the envelope or fix the pin, without opening a log.
func outOfBoundsError(op, bound, picked string, allowed []string) error {
	if picked == "" {
		picked = "(unidentified)"
	}
	return fmt.Errorf("%s: %s — this mission's envelope permits only %s; %s resolution picked %q. Nothing was sent: %w",
		resolutionBoundLead, bound, quotedList(allowed), op, picked, ErrResolutionOutOfBounds)
}

// quotedList renders an allowlist for a human, sorted so the same envelope
// always produces the same refusal text (the reason lands in a durable
// mission record and must not churn with upstream ordering).
func quotedList(entries []string) string {
	cleaned := make([]string, 0, len(entries))
	for _, e := range entries {
		if t := strings.TrimSpace(e); t != "" {
			cleaned = append(cleaned, fmt.Sprintf("%q", t))
		}
	}
	if len(cleaned) == 0 {
		return "(nothing)"
	}
	sort.Strings(cleaned)
	return strings.Join(cleaned, ", ")
}
