package agentdecl

import (
	"fmt"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// ErrAmbiguousDialect reports a file whose source product could not be
// established. Guessing would apply the wrong tool vocabulary and silently
// mis-map, so detection refuses instead.
type ErrAmbiguousDialect struct{ Path string }

func (e *ErrAmbiguousDialect) Error() string {
	return fmt.Sprintf("agentdecl: cannot tell which product %s was written for; "+
		"it carries no field unique to one and sits outside a known agents directory. "+
		"Name it with --dialect", e.Path)
}

var anchors = []struct {
	segments []string
	dialect  Dialect
}{
	{[]string{".gemini", "config", "agents"}, DialectAntigravity},
	{[]string{".config", "opencode", "agents"}, DialectOpenCode},
	{[]string{".claude", "agents"}, DialectClaudeCode},
	{[]string{".agents", "agents"}, DialectAntigravity},
	{[]string{".github", "agents"}, DialectCopilot},
	{[]string{".opencode", "agents"}, DialectOpenCode},
	{[]string{".cursor", "agents"}, DialectCursor},
}

var fingerprints = map[string]Dialect{
	"disallowedTools":        DialectClaudeCode,
	"isolation":              DialectClaudeCode,
	"skills":                 DialectClaudeCode,
	"memory":                 DialectClaudeCode,
	"color":                  DialectClaudeCode,
	"maxTurns":               DialectClaudeCode,
	"commandExecutionPolicy": DialectAntigravity,
	"mainAgent":              DialectAntigravity,
	"handoffs":               DialectCopilot,
	"user-invokable":         DialectCopilot,
	"argument-hint":          DialectCopilot,
	"target":                 DialectCopilot,
	"mode":                   DialectOpenCode,
	"maxSteps":               DialectOpenCode,
	"temperature":            DialectOpenCode,
	"disable":                DialectOpenCode,
	"readonly":               DialectCursor,
	"is_background":          DialectCursor,
}

// DetectDialect establishes which product a file was written for, strongest
// signal first: the nearest ancestor agents directory, then the filename shape,
// then a frontmatter key unique to one dialect. It refuses rather than guess.
func DetectDialect(path string, data []byte) (Dialect, error) {
	base := filepath.Base(path)
	parts := strings.Split(filepath.ToSlash(filepath.Clean(path)), "/")

	for _, a := range anchors {
		if !containsSegments(parts, a.segments) {
			continue
		}
		if a.segments[0] == ".agents" && base != "agent.md" && fingerprintOf(data) != a.dialect {
			continue
		}
		return a.dialect, nil
	}

	if strings.HasSuffix(base, ".agent.md") {
		return DialectCopilot, nil
	}
	if base == "agent.md" {
		return DialectAntigravity, nil
	}
	if d := fingerprintOf(data); d != "" {
		return d, nil
	}
	return "", &ErrAmbiguousDialect{Path: path}
}

// Parse detects the dialect and delegates. Callers that already know the
// dialect should call its parser directly.
func Parse(path string, data []byte, cfg Config) (*AgentIR, error) {
	dialect, err := DetectDialect(path, data)
	if err != nil {
		return nil, err
	}
	return ParseAs(dialect, path, data, cfg)
}

// ParseAs parses with the dialect stated rather than detected.
func ParseAs(dialect Dialect, path string, data []byte, cfg Config) (*AgentIR, error) {
	switch dialect {
	case DialectClaudeCode:
		return ParseClaudeCode(path, data, cfg)
	case DialectAntigravity:
		return ParseAntigravity(path, data, cfg)
	default:
		return nil, fmt.Errorf("agentdecl: no parser for dialect %q yet", dialect)
	}
}

func containsSegments(parts, want []string) bool {
	for i := 0; i+len(want) <= len(parts); i++ {
		if slicesEqual(parts[i:i+len(want)], want) {
			return true
		}
	}
	return false
}

func slicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func fingerprintOf(data []byte) Dialect {
	front, _, ok := splitFrontmatter(data)
	if !ok {
		return ""
	}
	fields := map[string]any{}
	if err := yaml.Unmarshal(front, &fields); err != nil {
		return ""
	}
	var found Dialect
	for key := range fields {
		d, ok := fingerprints[key]
		if !ok {
			continue
		}
		if found != "" && found != d {
			return ""
		}
		found = d
	}
	return found
}
