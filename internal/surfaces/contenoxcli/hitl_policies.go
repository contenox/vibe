package contenoxcli

import (
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
)

//go:embed hitl-policy-default.json
var hitlPolicyDefault string

//go:embed hitl-policy-strict.json
var hitlPolicyStrict string

//go:embed hitl-policy-dev.json
var hitlPolicyDev string

//go:embed hitl-policy-acp.json
var hitlPolicyACP string

//go:embed hitl-policy-acpx.json
var hitlPolicyACPX string

//go:embed hitl-policy-oracle.json
var hitlPolicyOracle string

// HITLPolicyPresets are the policies earlier builds seeded into ~/.contenox.
// Nothing writes them there any more — envelopes render into .generated
// instead — so they are kept for the copies still on operator machines: the
// staleness detector reads them to say what a file predates, the refresh verb
// rewrites a file that already exists, and the envelope ratchet holds the
// shipped envelopes to them.
var HITLPolicyPresets = []struct {
	Name    string
	Content string
}{
	{"hitl-policy-default.json", hitlPolicyDefault},
	{"hitl-policy-strict.json", hitlPolicyStrict},
	{"hitl-policy-dev.json", hitlPolicyDev},
	{"hitl-policy-acp.json", hitlPolicyACP},
	{"hitl-policy-acpx.json", hitlPolicyACPX},
	{"hitl-policy-oracle.json", hitlPolicyOracle},
}

func embeddedPolicyNames() []string {
	names := make([]string, len(HITLPolicyPresets))
	for i, p := range HITLPolicyPresets {
		names[i] = p.Name
	}
	return names
}

// refreshExistingHITLPolicies rewrites the preset copies a directory already
// holds. It never creates one: a file that is not there is an envelope's to
// render, and seeding it back would only widen the shadow.
func refreshExistingHITLPolicies(contenoxDir string) (written []string, err error) {
	for _, p := range HITLPolicyPresets {
		dst := filepath.Join(contenoxDir, p.Name)
		if _, statErr := os.Stat(dst); statErr != nil {
			continue
		}
		if writeErr := os.WriteFile(dst, []byte(p.Content), 0644); writeErr != nil {
			err = fmt.Errorf("failed to write %s: %w", dst, writeErr)
			break
		}
		written = append(written, dst)
	}
	return written, err
}
