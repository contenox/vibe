package llmrepo

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	libmodelprovider "github.com/contenox/contenox/internal/models/modelrepo"
)

// ErrResolutionOutOfBounds is the sentinel every allowlist refusal wraps.
var ErrResolutionOutOfBounds = errors.New("resolution outside the mission envelope")

const resolutionBoundLead = "compute bound refused"

// ResolutionBounds is the envelope's model/backend allowlist enforced at the resolution seam; matching is exact and case-insensitive, and applies across chat, prompt, stream, and embed alike.
type ResolutionBounds struct {
	// Models bounds which model names may be resolved; empty means unbounded.
	Models []string
	// Backends bounds which backends may serve them, matched by operator-facing name or id; empty means unbounded.
	Backends []string
}

// IsZero reports whether these bounds restrict nothing, which is the case for
// every request that is not a bounded mission's.
func (b ResolutionBounds) IsZero() bool {
	return len(b.Models) == 0 && len(b.Backends) == 0
}

type resolutionBoundsContextKeyType struct{}

var resolutionBoundsContextKey resolutionBoundsContextKeyType

// WithResolutionBounds binds an envelope's model/backend allowlist to ctx, replacing any bound already present.
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

func (e *modelManager) backendAllowed(ctx context.Context, allowed []string, backendID string) bool {
	if allowlistContains(allowed, backendID) {
		return true
	}
	name := e.backendName(ctx, backendID)
	return name != "" && allowlistContains(allowed, name)
}

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

func (e *modelManager) backendLabel(ctx context.Context, backendID string) string {
	if name := e.backendName(ctx, backendID); name != "" {
		return fmt.Sprintf("%s (%s)", name, backendID)
	}
	return backendID
}

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

func outOfBoundsError(op, bound, picked string, allowed []string) error {
	if picked == "" {
		picked = "(unidentified)"
	}
	return fmt.Errorf("%s: %s — this mission's envelope permits only %s; %s resolution picked %q. Nothing was sent: %w",
		resolutionBoundLead, bound, quotedList(allowed), op, picked, ErrResolutionOutOfBounds)
}

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
