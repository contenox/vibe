package llmrepo

// bounds.go is the ENFORCEMENT half of the envelope's model/backend allowlist
// (hitlservice.ComputeBounds.ModelAllowlist / BackendAllowlist). The envelope
// declares which models and backends a mission's total compute may touch; this
// file is the one place in the runtime that can hold it to that, because this is
// the one place a model and a backend are actually CHOSEN — every generative and
// embedding call funnels through llmresolver here, whatever built the request.
//
// Why here and not host-side, next to maxTurns/maxToolCalls: those bounds are
// host-observable (the drive loop counts the turns it itself issues; the
// unattended answerer counts the tool dispatches it itself gates). Model choice
// is not. A dispatched unit is this runtime re-invoked as an ACP peer over stdio
// (agentinstance's chain branch), it resolves its own models in its own process,
// and the ACP session/update contract carries no model or backend identity at all
// (libacp.SessionUpdate has usage counters and nothing else) — so the supervising
// process is structurally blind to the choice. What crosses the process boundary
// is the BOUND, not the observation: the dispatcher puts the envelope's allowlists
// into the unit's session/new `_meta` (missionservice.MissionMeta), the unit's
// transport binds them onto every turn context (acpsvc), and they arrive here on
// the same context that already carries the session cache key and the per-session
// HITL policy name.
//
// The check is a REFUSAL BEFORE THE CALL, not a report after it: resolution
// happens, the choice is measured against the bound, and a choice outside the
// bound returns an error naming the bound with nothing sent to any provider. That
// is the same shape as the turn and tool ceilings — a bound crossing is never
// silent and never partially spent.
//
// Absent bounds are UNBOUNDED. An ordinary chat session, a `contenox run`, a unit
// on a mission whose envelope declares no allowlist: none of them carry a bound on
// the context, and every one of them resolves exactly as it did before this file
// existed. Bounds only ever RESTRICT.

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	libmodelprovider "github.com/contenox/beam/internal/models/modelrepo"
)

// ErrResolutionOutOfBounds is the sentinel every allowlist refusal wraps, so a
// caller (or a test) can tell an envelope refusal apart from a resolution failure
// or a provider error. It is deliberately NOT a resolver error: the resolver did
// its job and found a usable model — the envelope is what says no.
var ErrResolutionOutOfBounds = errors.New("resolution outside the mission envelope")

// resolutionBoundLead is the stable, greppable lead of every allowlist refusal
// reason. It mirrors fleetservice's computeBoundLead for the ceilings, so a
// compute-bound refusal reads the same way whichever bound produced it.
const resolutionBoundLead = "compute bound refused"

// ResolutionBounds is the envelope's model/backend allowlist as it travels to the
// resolution seam: the same two lists hitlservice.ComputeBounds declares, carried
// as a neutral value so this layer never imports a service.
//
// Both lists are ALLOWLISTS and both are OPT-IN: an empty list is unbounded for
// that dimension, so declaring only ModelAllowlist bounds the models and leaves
// backend choice alone. Matching is exact after trimming, case-insensitively —
// deliberately NOT the resolver's NormalizeModelName, which strips version tags
// and punctuation ("llama2:7b" and "llama2:70b" both collapse to "llama2"). A
// security boundary must not silently widen; an operator who writes a model name
// gets exactly that model name.
//
// The allowlist is TOTAL, not per-kind: it bounds chat, prompt, stream AND embed
// alike, because it bounds what the mission may SPEND and an embedding call spends
// too. A mission that embeds must name its embedding model in the list. That is
// the fail-closed reading, and the one an operator who wrote "this mission may
// only use X" actually means.
type ResolutionBounds struct {
	// Models bounds which model names may be resolved. Empty means unbounded.
	Models []string
	// Backends bounds which backends may serve them. An entry matches a backend's
	// operator-facing NAME (what `contenox backend list` shows) or its id, so an
	// operator never has to paste a uuid into an envelope to express "only my
	// local ollama". Empty means unbounded.
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
// nothing are not stored at all, which keeps the unbounded path allocation-free
// and byte-identical to the behavior before this existed.
//
// It is deliberately NOT idempotent-by-merge: a second call REPLACES the bound
// rather than intersecting with it, because the only writer is the transport
// binding a unit's own envelope onto its own turn, and a silent merge would make
// the effective bound depend on call order.
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

// enforceResolutionBounds holds one resolved model/backend pair to the envelope
// bound on ctx. It is called AFTER resolution and BEFORE the provider call, so a
// refusal costs nothing but the resolution itself.
//
// op names the operation in the refusal ("chat", "stream", "prompt", "embed") so
// a mission's terminal record says which call the envelope stopped, not merely
// that something was stopped.
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

// backendAllowed reports whether backendID names a backend the envelope permits.
// A backend is identified two ways on purpose: by its operator-facing name, which
// is what an operator writes and what `contenox backend list` prints, and by its
// id, which is what the resolver returns and what a machine-generated envelope may
// carry. Either match admits it.
//
// A backend the runtime state cannot name (an id with no state row — a backend
// removed mid-run) matches only by id. That fails CLOSED for a name-based
// allowlist, which is the right direction: an envelope that named its backends
// does not silently admit one the runtime can no longer identify.
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
// cannot drift apart: trim surrounding space, compare case-insensitively, require
// the whole string to match. No globs, no normalization, no prefixes — an operator
// reading their envelope can predict the outcome without reading this file.
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

// quotedList renders an allowlist for a human, sorted so the same envelope always
// produces the same refusal text (a reason lands in a durable mission record; it
// must not churn with map or slice ordering upstream).
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
