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

// presetStateFile records the sha256 of every preset THIS build wrote, so a
// later build can tell an untouched preset (safe to upgrade) from one the
// operator edited (never overwrite).
//
// THIS BUILD IS THE TRANSITION POINT. Installs made before the state file
// existed have presets with no provenance at all: "untouched output of an older
// build" and "hand-edited envelope" are indistinguishable from disk, and the
// only safe reading of an unprovable file is the operator's. Those installs are
// therefore NOT upgraded silently — they are DETECTED (which shipped toolsets
// their envelope never mentions) and told, once, by doctor and by beam's
// startup line, with `contenox init --refresh-policies` as the verb. See
// hitl_policy_staleness.go.
//
// Forward from here the ambiguity is gone: every write records its hash, and a
// file that still matches what we last wrote — or that already matches this
// build byte for byte — is adopted and upgraded automatically, with no notice
// and no operator involvement.
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

// writeEmbeddedHITLPolicies writes the embedded policy presets to contenoxDir
// and UPGRADES the ones this build has outgrown.
//
// The rule, in order: a missing preset is written; a preset whose bytes still
// match what a previous build recorded writing is refreshed (nobody edited it,
// so holding a stale envelope back only means shipped tools ask for approvals
// they should not — the failure every unit test passes through); anything else
// is left exactly as the operator left it and named in the returned stale list
// so a caller can say so. overwrite=true forces all of them (the setup
// wizard's --force).
//
// The returned names are presets that differ from this build's and were NOT
// touched because they look hand-edited.
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
					// ADOPTION: byte-identical to what this build ships, so
					// whatever wrote it, it is provably ours. Record it, and the
					// NEXT upgrade proceeds automatically — this is how an
					// install with no state file rejoins the automatic path
					// without anything ever being overwritten.
					if state[p.Name] != shipped {
						state[p.Name] = shipped
						changed = true
					}
					continue
				}
				if recorded, ok := state[p.Name]; !ok || recorded != current {
					// Hand-edited, or predates the record: the operator's
					// file wins — it is a security boundary, not a cache. Name
					// it so the caller can offer a refresh; what the envelope
					// is actually MISSING (and therefore which toolsets now ask
					// for approval) is computed separately, semantically, by
					// hitl_policy_staleness.go.
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
