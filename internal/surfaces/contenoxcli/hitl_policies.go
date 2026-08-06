package contenoxcli

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
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

//go:embed hitl-policy-beam.json
var hitlPolicyBeam string

//go:embed hitl-policy-oracle.json
var hitlPolicyOracle string

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
	{"hitl-policy-beam.json", hitlPolicyBeam},
	{"hitl-policy-oracle.json", hitlPolicyOracle},
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

// presetStateFile records each preset's sha256 as last written, so
// upgradeEmbeddedHITLPolicies can tell an untouched preset (safe to refresh)
// from an operator-edited one (never overwritten). Installs that predate this
// file have no provenance and are reported as stale instead (hitl_policy_staleness.go).
const presetStateFile = ".preset-state.json"

func presetSHA(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func readPresetState(contenoxDir string) map[string]string {
	state := map[string]string{}
	raw, err := os.ReadFile(filepath.Join(contenoxDir, presetStateFile))
	if err != nil {
		return state
	}
	_ = json.Unmarshal(raw, &state)
	return state
}

func writePresetState(contenoxDir string, state map[string]string) {
	raw, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return
	}
	// Best-effort: losing the record costs a future upgrade, never correctness.
	_ = os.WriteFile(filepath.Join(contenoxDir, presetStateFile), raw, 0644)
}

// writeEmbeddedHITLPolicies writes the embedded policy presets to contenoxDir,
// refreshing any preset whose on-disk bytes still match a previous build's;
// anything that looks hand-edited is left untouched. overwrite=true forces
// every preset (the setup wizard's --force).
func writeEmbeddedHITLPolicies(contenoxDir string, overwrite bool) error {
	_, err := upgradeEmbeddedHITLPolicies(contenoxDir, overwrite)
	return err
}

func upgradeEmbeddedHITLPolicies(contenoxDir string, overwrite bool) (stale []string, err error) {
	if err := os.MkdirAll(contenoxDir, 0750); err != nil {
		return nil, fmt.Errorf("failed to create config dir: %w", err)
	}
	state := readPresetState(contenoxDir)
	changed := false
	for _, p := range HITLPolicyPresets {
		dst := filepath.Join(contenoxDir, p.Name)
		shipped := presetSHA(p.Content)
		if !overwrite {
			if onDisk, readErr := os.ReadFile(dst); readErr == nil {
				current := presetSHA(string(onDisk))
				if current == shipped {
					// Byte-identical to what this build ships: record it so
					// future upgrades treat it as ours.
					if state[p.Name] != shipped {
						state[p.Name] = shipped
						changed = true
					}
					continue
				}
				if recorded, ok := state[p.Name]; !ok || recorded != current {
					// Hand-edited or unrecorded: the operator's file wins and
					// is left alone; hitl_policy_staleness.go computes what
					// it's missing.
					stale = append(stale, p.Name)
					continue
				}
			}
		}
		if err := os.WriteFile(dst, []byte(p.Content), 0644); err != nil {
			return stale, fmt.Errorf("failed to write %s: %w", p.Name, err)
		}
		state[p.Name] = shipped
		changed = true
	}
	if changed {
		writePresetState(contenoxDir, state)
	}
	return stale, nil
}

// refreshExistingHITLPolicies overwrites only the presets contenoxDir already
// holds with this build's content, recording provenance the way
// upgradeEmbeddedHITLPolicies does. Absent presets stay absent: seeding a
// workspace dir would widen its shadow over ~/.contenox.
func refreshExistingHITLPolicies(contenoxDir string) (written []string, err error) {
	state := readPresetState(contenoxDir)
	changed := false
	for _, p := range HITLPolicyPresets {
		dst := filepath.Join(contenoxDir, p.Name)
		if _, statErr := os.Stat(dst); statErr != nil {
			continue
		}
		if writeErr := os.WriteFile(dst, []byte(p.Content), 0644); writeErr != nil {
			err = fmt.Errorf("failed to write %s: %w", dst, writeErr)
			break
		}
		state[p.Name] = presetSHA(p.Content)
		changed = true
		written = append(written, dst)
	}
	if changed {
		writePresetState(contenoxDir, state)
	}
	return written, err
}
