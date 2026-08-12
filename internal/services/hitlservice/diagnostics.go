package hitlservice

import (
	"encoding/json"
	"fmt"
)

// PolicyDiagnostic is one true statement about an envelope that parsed
// cleanly; Field is the dotted JSON path, Message says what to rely on instead.
type PolicyDiagnostic struct {
	Field   string
	Message string
}

// String renders a diagnostic as one line, shared by `contenox vet` and the
// durable runtime record.
func (d PolicyDiagnostic) String() string {
	return fmt.Sprintf("%s: %s", d.Field, d.Message)
}

// TrustedBinaryDiagnostics reports every declared trusted-binary entry that
// is not OK on this host; returns nil for a document that does not parse or
// declares nothing.
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
