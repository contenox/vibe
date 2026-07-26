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

// HITLPolicyPresets lists the names and content of all embedded HITL policy presets
// in the order they should be written to disk.
var HITLPolicyPresets = []struct {
	Name    string
	Content string
}{
	{"hitl-policy-default.json", hitlPolicyDefault},
	{"hitl-policy-strict.json", hitlPolicyStrict},
	{"hitl-policy-dev.json", hitlPolicyDev},
	{"hitl-policy-acp.json", hitlPolicyACP},
	{"hitl-policy-acpx.json", hitlPolicyACPX},
}

// embeddedPolicyNames returns the preset file names in preset order, for the
// ACP /policy listing.
func embeddedPolicyNames() []string {
	names := make([]string, len(HITLPolicyPresets))
	for i, p := range HITLPolicyPresets {
		names[i] = p.Name
	}
	return names
}

// writeEmbeddedHITLPolicies writes the embedded policy presets to contenoxDir.
// If overwrite is false, existing files are left untouched (returns false for that file).
func writeEmbeddedHITLPolicies(contenoxDir string, overwrite bool) error {
	if err := os.MkdirAll(contenoxDir, 0750); err != nil {
		return fmt.Errorf("failed to create config dir: %w", err)
	}
	for _, p := range HITLPolicyPresets {
		dst := filepath.Join(contenoxDir, p.Name)
		if !overwrite {
			if _, err := os.Stat(dst); err == nil {
				continue
			}
		}
		if err := os.WriteFile(dst, []byte(p.Content), 0644); err != nil {
			return fmt.Errorf("failed to write %s: %w", p.Name, err)
		}
	}
	return nil
}
