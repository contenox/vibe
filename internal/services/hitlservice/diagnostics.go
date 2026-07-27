package hitlservice

// diagnostics.go answers a question the validator alone cannot: an envelope field
// can be perfectly SHAPED and still not do what the operator who wrote it
// believes. validatePolicy and VetPolicy both reject malformed envelopes; neither
// has any way to say "this parses, and it does less than it reads like".
//
// That gap matters more here than in most config, because an envelope is a
// SECURITY document. A field that is declared, accepted, and then not enforced is
// a false claim made to the only person it was written for. The blueprints hold
// one standard for this — a security claim is stated at exactly its true strength
// or not at all — and a Go doc comment does not meet it: doc comments protect the
// developer reading the source, not the operator writing
// `hitl-policy-mission.json` who will never open it.
//
// So every envelope field whose enforcement is weaker than its plain reading gets
// a DIAGNOSTIC here, surfaced at authoring time by `contenox vet`. The bar for
// adding one is deliberately high, and it is not "this field has caveats":
//
//   - A field that does what it says gets NO diagnostic, however subtle its
//     semantics. Warning about everything is the same as warning about nothing.
//   - A field that is enforced but BEST-EFFORT (maxTokens, which bounds a mission
//     only as far as the unit's provider reports usage) states that where the
//     operator sets it — in the preset's own annotation — not as a warning on
//     every run.
//   - A field that is ACCEPTED AND NOT IMPLEMENTED gets a diagnostic, every time,
//     until it is implemented or removed. That is the whole list today: exactly
//     one entry.
//
// An envelope that uses none of these is silent. Diagnostics never fail a vet run
// — they are true statements about working files, not defects — so a policy that
// warns still loads, still validates, and still governs.

import (
	"encoding/json"
	"fmt"
)

// PolicyDiagnostic is one true-but-uncomfortable statement about an envelope that
// parsed cleanly. Field is the dotted path an operator can search their JSON for;
// Message says what the field does today and what to rely on instead.
type PolicyDiagnostic struct {
	Field   string
	Message string
}

// String renders a diagnostic as one line, the form both `contenox vet` and a
// durable runtime record use so the operator reads the same sentence in both
// places.
func (d PolicyDiagnostic) String() string {
	return fmt.Sprintf("%s: %s", d.Field, d.Message)
}

// PolicyDiagnostics reports every declared-but-not-fully-enforced field in a
// policy document. It parses leniently and returns nothing for a document that
// does not parse: a broken file's problem is its parse error, which the validator
// already reports, and guessing at the intent of malformed JSON would produce
// warnings about fields the operator never wrote.
//
// Returns nil — not an empty slice — when the envelope claims nothing it cannot
// deliver, so the common case is trivially testable as "silent".
func PolicyDiagnostics(data []byte) []PolicyDiagnostic {
	var p Policy
	if err := json.Unmarshal(data, &p); err != nil {
		return nil
	}
	return ComputeDiagnostics(p.Compute)
}

// ComputeDiagnostics is PolicyDiagnostics for an already-loaded compute block —
// the form the runtime uses, where the envelope was loaded rather than read from
// disk. A nil block (no compute bounds at all) reports nothing.
func ComputeDiagnostics(c *ComputeBounds) []PolicyDiagnostic {
	if c == nil {
		return nil
	}
	var out []PolicyDiagnostic
	if c.OnExhausted == OnExhaustedPauseAsk {
		out = append(out, PolicyDiagnostic{
			Field:   "compute.onExhausted",
			Message: unenforcedPauseAskMessage,
		})
	}
	return out
}

// unenforcedPauseAskMessage is the ONE unenforced-field message today, kept in a
// constant because it is stated in three places that must not drift: `contenox
// vet` at authoring time, the compute-bound test that pins this contract, and the
// terminal mission record at the moment the substitution actually happens
// (fleetservice names it in the stuck reason, so an operator who never ran vet
// still learns it exactly when it cost them something).
const unenforcedPauseAskMessage = `"pause_ask" is ACCEPTED BUT NOT IMPLEMENTED — a mission that hits a compute bound is finished stuck, exactly as onExhausted:"finish_stuck" would. It does not pause, and it files no ask for you to answer. Until it is implemented, rely on the mission finishing at StatusStuck with a reason naming the bound (visible on the board, in the inbox, and to "mission fire --wait"), and set the ceiling you are willing to spend rather than one you plan to extend.`
