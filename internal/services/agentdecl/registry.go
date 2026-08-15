package agentdecl

import (
	"fmt"
	"strings"
)

// ErrUnknownTool reports declaration tool names that resolve to nothing
// connected. It names every unresolved entry rather than the first.
type ErrUnknownTool struct {
	Tools []string
}

func (e *ErrUnknownTool) Error() string {
	return fmt.Sprintf("agentdecl: none of this declaration's tools resolve to anything connected here: %s. "+
		"contenox hosts local_fs, local_shell, webtools and git; anything else is a tool you connect "+
		"(an MCP server, an OpenAPI spec, a shell command). Connect one, then name it under [tools] in %s.",
		strings.Join(e.Tools, ", "), ConfigFilename)
}

// MapTools resolves declaration tool names to connected tools, preserving order
// and dropping duplicates. A name containing a dot is an MCP tool and passes
// through unchanged.
//
// An unresolved name is dropped and reported, matching how the declaration
// format behaves elsewhere; only a list where nothing at all resolves is an
// error, since that agent has no way to act.
func (c Config) MapTools(names []string) ([]string, []Unmapped, error) {
	var (
		out     []string
		unknown []string
		skipped []Unmapped
	)
	seen := map[string]bool{}
	for _, raw := range names {
		name := strings.TrimSpace(raw)
		if name == "" {
			continue
		}
		mapped, ok := c.Tools[name]
		if !ok && strings.Contains(name, ".") {
			mapped, ok = name, true
		}
		if !ok {
			unknown = append(unknown, name)
			skipped = append(skipped, Unmapped{
				Field:  "tools",
				Value:  name,
				Reason: "resolves to nothing connected here; the agent runs without it. Connect it and name it under [tools] in " + ConfigFilename,
			})
			continue
		}
		if seen[mapped] {
			continue
		}
		seen[mapped] = true
		out = append(out, mapped)
	}
	if len(out) == 0 && len(unknown) > 0 {
		return nil, nil, &ErrUnknownTool{Tools: unknown}
	}
	return out, skipped, nil
}

// MapModel resolves a declaration's model name to a provider and id. An
// unrecognized name yields ok false, which leaves routing templated.
func (c Config) MapModel(name string) (provider, id string, ok bool) {
	v, found := c.Models[strings.TrimSpace(name)]
	if !found {
		return "", "", false
	}
	provider, id, found = strings.Cut(v, ":")
	if !found {
		return "", "", false
	}
	return provider, id, true
}

// ToolSets returns the distinct toolset names covering the given connected
// tools, which is the vocabulary execute_config.tools takes.
func ToolSets(connected []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, t := range connected {
		set, _, found := strings.Cut(t, ".")
		if !found || seen[set] {
			continue
		}
		seen[set] = true
		out = append(out, set)
	}
	return out
}
