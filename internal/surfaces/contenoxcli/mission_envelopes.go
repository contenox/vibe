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

// missionEnvelopes is an acpsvc.MissionEnvelopeSource over a policy search path.
// It stats the filesystem on every call rather than caching a startup snapshot.
type missionEnvelopes struct{ dirs []string }

var _ acpsvc.MissionEnvelopeSource = missionEnvelopes{}

func newMissionEnvelopes(contenoxDir string) missionEnvelopes {
	return missionEnvelopes{dirs: policyDirs(contenoxDir)}
}

// ListEnvelopes returns every hitl-policy-*.json on the search path, sorted
// within each directory and deduplicated by name.
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
// carrying a path separator is refused outright.
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

// envelopeSummary renders an envelope's character in one line: what an unruled
// call does, the declared tool-call ceiling, and who may answer the unit's
// questions. Empty when the file cannot be read as an envelope.
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
