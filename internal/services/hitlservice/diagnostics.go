package hitlservice

// diagnostics.go carries the vocabulary for envelope fields that parse and
// validate cleanly but enforce less than their plain reading suggests. The
// bar is high: a field that does what it says gets none, and a field that
// lies is rejected outright rather than warned about (OnExhaustedPauseAsk).
// Diagnostics never fail a vet run; a policy that warns still loads,
// validates, and governs. TrustedBinaryDiagnostics is the one producer.

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

// TrustedBinaryDiagnostics reports every declared trusted-binary entry that is
// not OK on THIS host: the declarations describe one machine, so a missing,
// mismatched, unreadable, or outside-dirs entry is a warning rather than a
// defect — the envelope stays valid and the runtime's answer for such an entry
// is a refusal, never a silent pass. Reads and hashes files, so it belongs to
// host-aware callers (vet, doctor). Returns nil for a document that does not
// parse or declares nothing.
func TrustedBinaryDiagnostics(data []byte) []PolicyDiagnostic {
	var p Policy
	if err := json.Unmarshal(data, &p); err != nil {
		return nil
	}
	statuses := CheckTrustedBinaries(p.TrustedBinaries)
	if len(statuses) == 0 {
		return nil
	}
	out := make([]PolicyDiagnostic, 0, len(statuses))
	for _, s := range statuses {
		out = append(out, PolicyDiagnostic{Field: "trusted_binaries.hashes", Message: s.String()})
	}
	return out
}
