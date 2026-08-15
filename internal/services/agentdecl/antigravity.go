package agentdecl

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

const specDateAntigravity = "2026-08-14"

// ParseAntigravity parses an Antigravity agent definition. Its name comes from
// the enclosing directory when the frontmatter omits it, since the format fixes
// the filename at agent.md.
func ParseAntigravity(path string, data []byte, cfg Config) (*AgentIR, error) {
	front, body, ok := splitFrontmatter(data)
	if !ok {
		return nil, &ErrNoFrontmatter{Path: path}
	}

	fields := map[string]any{}
	if err := yaml.Unmarshal(front, &fields); err != nil {
		return nil, fmt.Errorf("agentdecl: parse %s frontmatter: %w", path, err)
	}

	sum := sha256.Sum256(data)
	ir := &AgentIR{
		Source: Source{
			Dialect:  DialectAntigravity,
			SpecDate: specDateAntigravity,
			Path:     path,
			SHA256:   hex.EncodeToString(sum[:]),
		},
		SystemPrompt: strings.TrimSpace(string(body)),
		Role:         RoleMission,
		Posture:      PostureAskAlways,
	}

	ir.Name = strings.TrimSpace(stringField(fields, "name"))
	if ir.Name == "" {
		ir.Name = enclosingDirName(path)
	}
	if ir.Name == "" {
		return nil, &ErrMissingField{Path: path, Field: "name"}
	}
	ir.Description = strings.TrimSpace(stringField(fields, "description"))
	if ir.Description == "" {
		return nil, &ErrMissingField{Path: path, Field: "description"}
	}
	delete(fields, "name")
	delete(fields, "description")

	if raw := strings.TrimSpace(stringField(fields, "model")); raw != "" {
		ir.Model.Raw = raw
		if provider, id, ok := cfg.MapModel(raw); ok {
			ir.Model.Provider, ir.Model.ID = provider, id
		}
	}
	delete(fields, "model")

	if named, ok := fields["tools"]; ok && named != nil {
		allow, skipped, err := cfg.MapTools(listField(fields, "tools"))
		if err != nil {
			return nil, err
		}
		ir.Tools.Allow = allow
		ir.Unmapped = append(ir.Unmapped, skipped...)
	} else {
		// An omitted tools list inherits every tool, per the declaration format.
		ir.Tools.Inherit = true
	}
	delete(fields, "tools")

	ir.Role = antigravityRole(fields)
	delete(fields, "mainAgent")
	delete(fields, "subagent")

	if mode := strings.TrimSpace(stringField(fields, "permissionMode")); mode != "" {
		posture, note := postureFromAntigravity(mode)
		ir.Posture = posture
		if note != "" {
			ir.AddUnmapped("permissionMode", mode, note)
		}
	}
	delete(fields, "permissionMode")

	if policy := strings.TrimSpace(stringField(fields, "commandExecutionPolicy")); policy == "auto" {
		ir.AddUnmapped("commandExecutionPolicy", policy,
			"auto-run shell has no posture here; imported so shell still asks a human, which is tighter than the source asked for")
	}
	delete(fields, "commandExecutionPolicy")

	for field, value := range fields {
		ir.AddUnmapped(field, summarize(value), unmappedReasonAntigravity(field))
	}
	return ir, nil
}

func antigravityRole(fields map[string]any) Role {
	main, _ := fields["mainAgent"].(bool)
	sub, _ := fields["subagent"].(bool)
	switch {
	case main && sub:
		return RoleBoth
	case main:
		return RolePrimary
	default:
		return RoleMission
	}
}

func postureFromAntigravity(mode string) (Posture, string) {
	switch mode {
	case "acceptEdits":
		return PostureAutoEdit, ""
	case "readOnly":
		return PostureReadOnly, ""
	case "bypassPermissions":
		return PostureUnsafe, ""
	default:
		return PostureAskAlways, ""
	}
}

func unmappedReasonAntigravity(field string) string {
	switch field {
	case "hooks":
		return "lifecycle hooks are shell commands; only PreToolUse denies have a policy equivalent"
	case "skills":
		return "no contenox equivalent"
	default:
		return "unrecognized field for this dialect"
	}
}

// enclosingDirName is the agent name Antigravity takes from the directory,
// since every definition is called agent.md.
func enclosingDirName(path string) string {
	dir := filepath.Dir(filepath.Clean(path))
	if dir == "." || dir == string(filepath.Separator) {
		return ""
	}
	return filepath.Base(dir)
}
