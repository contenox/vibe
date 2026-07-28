package hitlservice

// diagnostics.go surfaces envelope fields that parse cleanly but enforce
// less than their plain reading suggests — validatePolicy only rejects
// malformed envelopes, not misleading ones. The bar for adding a diagnostic
// is high: a field that does what it says gets none. Diagnostics never fail
// a vet run; a policy that warns still loads, validates, and governs.

import (
	"encoding/json"
	"fmt"
)

// PolicyDiagnostic is one true statement about an envelope that parsed
// cleanly. Field is the dotted JSON path; Message says what to rely on instead.
type PolicyDiagnostic struct {
	Field   string
	Message string
}

// String renders a diagnostic as one line, shared by `contenox vet` and the
// durable runtime record.
func (d PolicyDiagnostic) String() string {
	return fmt.Sprintf("%s: %s", d.Field, d.Message)
}

// PolicyDiagnostics reports every declared-but-not-fully-enforced field in a
// policy document. Returns nil for a document that doesn't parse, or that
// claims nothing it cannot deliver.
func PolicyDiagnostics(data []byte) []PolicyDiagnostic {
	var p Policy
	if err := json.Unmarshal(data, &p); err != nil {
		return nil
	}
	return ComputeDiagnostics(p.Compute)
}

// ComputeDiagnostics is PolicyDiagnostics for an already-loaded compute
// block. A nil block reports nothing.
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

// unenforcedPauseAskMessage is the one unenforced-field message today, kept
// in a constant so `contenox vet` and the terminal mission record agree.
const unenforcedPauseAskMessage = `"pause_ask" is ACCEPTED BUT NOT IMPLEMENTED — a mission that hits a compute bound is finished stuck, exactly as onExhausted:"finish_stuck" would. It does not pause, and it files no ask for you to answer. Until it is implemented, rely on the mission finishing at StatusStuck with a reason naming the bound (visible on the board, in the inbox, and to "mission fire --wait"), and set the ceiling you are willing to spend rather than one you plan to extend.`
