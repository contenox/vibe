// mission_envelopes.go discovers the HITL policy files a mission may name as
// its envelope. It reads the same search path the policy loader reads
// (policyDirs: the resolved .contenox dir first, then ~/.contenox, first
// match wins) and the same hitl-policy-*.json convention `contenox vet`
// classifies by name, so what a session offers is what a unit would load.
package contenoxcli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/contenox/contenox/internal/services/hitlservice"
	"github.com/contenox/contenox/internal/surfaces/acpsvc"
)

// missionEnvelopeGlob matches the policy files offered as mission envelopes.
const missionEnvelopeGlob = "hitl-policy-*.json"

// missionEnvelopes is an acpsvc.MissionEnvelopeSource over a policy search
// path. It stats the filesystem on every call rather than caching a startup
// snapshot: an envelope written while a session is open is offered without a
// restart, and one deleted stops being offered.
type missionEnvelopes struct{ dirs []string }

var _ acpsvc.MissionEnvelopeSource = missionEnvelopes{}

// newMissionEnvelopes builds the source for a surface rooted at contenoxDir.
func newMissionEnvelopes(contenoxDir string) missionEnvelopes {
	return missionEnvelopes{dirs: policyDirs(contenoxDir)}
}

// ListEnvelopes returns every hitl-policy-*.json on the search path, sorted
// within each directory and deduplicated by name, so a workspace copy
// shadows the home one exactly as the loader resolves it.
func (m missionEnvelopes) ListEnvelopes() []acpsvc.MissionEnvelope {
	var out []acpsvc.MissionEnvelope
	seen := map[string]bool{}
	for _, dir := range m.dirs {
		if dir == "" {
			continue
		}
		matches, err := filepath.Glob(filepath.Join(dir, missionEnvelopeGlob))
		if err != nil {
			continue // a malformed pattern is impossible here; an unreadable dir is not fatal
		}
		sort.Strings(matches)
		for _, path := range matches {
			name := filepath.Base(path)
			if seen[name] {
				continue
			}
			seen[name] = true
			raw, readErr := os.ReadFile(path)
			if readErr != nil {
				continue
			}
			out = append(out, acpsvc.MissionEnvelope{
				Name:    name,
				Path:    path,
				Summary: envelopeSummary(raw),
			})
		}
	}
	return out
}

// LookupEnvelope resolves one envelope name against the search path. A name
// carrying a path separator is refused outright: /mission takes a file NAME,
// and a traversal must not reach outside the config dirs.
func (m missionEnvelopes) LookupEnvelope(name string) (acpsvc.MissionEnvelope, bool) {
	name = strings.TrimSpace(name)
	if name == "" || name != filepath.Base(name) || strings.ContainsAny(name, `/\`) {
		return acpsvc.MissionEnvelope{}, false
	}
	path, raw, ok := readPolicyFile(m.dirs, name)
	if !ok {
		return acpsvc.MissionEnvelope{}, false
	}
	return acpsvc.MissionEnvelope{Name: name, Path: path, Summary: envelopeSummary(raw)}, true
}

// envelopeDoc is the minimal shape a one-line character sketch needs. Parsing
// is deliberately tolerant: an envelope that does not parse gets no summary,
// never a wrong one about someone's security boundary.
type envelopeDoc struct {
	DefaultAction string `json:"default_action"`
	Compute       struct {
		MaxToolCalls int `json:"maxToolCalls"`
	} `json:"compute"`
	Attention struct {
		AllowAgentAnswers bool `json:"allowAgentAnswers"`
		MaxAgentAnswers   int  `json:"maxAgentAnswers"`
	} `json:"attention"`
}

// envelopeSummary renders an envelope's character in one line: what an
// unruled call does, the declared tool-call ceiling, and who may answer the
// unit's questions. maxToolCalls is labelled "declared, not enforced" because
// its one enforcement seam is the unattended permission answerer, which no
// shipped host wires (hitlservice.ComputeBounds) — a picker must not assert a
// ceiling the host does not hold. Empty when the file cannot be read as an
// envelope.
func envelopeSummary(raw []byte) string {
	var doc envelopeDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		return ""
	}
	parts := []string{envelopeDefaultActionPhrase(doc.DefaultAction)}
	if doc.Compute.MaxToolCalls > 0 {
		parts = append(parts, fmt.Sprintf("≤%d tool calls (declared, not enforced)", doc.Compute.MaxToolCalls))
	}
	if doc.Attention.AllowAgentAnswers {
		bounds := hitlservice.AttentionBounds{
			AllowAgentAnswers: true,
			MaxAgentAnswers:   doc.Attention.MaxAgentAnswers,
		}
		parts = append(parts, fmt.Sprintf("an agent may answer %d questions", bounds.EffectiveMaxAgentAnswers()))
	} else {
		parts = append(parts, "questions always wait for a human")
	}
	return strings.Join(parts, " · ")
}

// envelopeDefaultActionPhrase states what happens to a call no rule matches.
// An empty default_action is the loader's fail-closed approve (hitlservice
// policy.go), the same reading staleFallthrough takes.
func envelopeDefaultActionPhrase(action string) string {
	switch strings.TrimSpace(action) {
	case "", "approve":
		return "unruled calls stop for approval"
	case "deny":
		return "unruled calls are denied"
	case "allow":
		return "unruled calls run unreviewed"
	default:
		return fmt.Sprintf("unruled calls take default_action %q", action)
	}
}
