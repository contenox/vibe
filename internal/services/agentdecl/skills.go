package agentdecl

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// SkillDirName is where procedures live inside a contenox directory, beside the
// agents that use them.
const SkillDirName = "skills"

// SkillsMacro is what a declaration writes to pull the inventory in. It expands
// when the chain is generated, not per request.
const SkillsMacro = "{{skills}}"

// Skill is one procedure: how to orchestrate several tools for a repeated job.
// The runtime does not load or execute it; the agent reads it with its file tool.
type Skill struct {
	Name        string
	Description string
	Path        string
}

// DiscoverSkills reads every skill under the given contenox directories, nearest
// root first, in either the flat `timesheet.md` or the `timesheet/SKILL.md`
// layout. A nearer skill shadows one of the same name further out, and one
// outside workspaceRoot is left out because the agent could not address it.
func DiscoverSkills(contenoxDirs []string, workspaceRoot string) []Skill {
	seen := map[string]bool{}
	var out []Skill
	for _, dir := range contenoxDirs {
		if strings.TrimSpace(dir) == "" {
			continue
		}
		for _, skill := range skillsIn(filepath.Join(dir, SkillDirName)) {
			if seen[skill.Name] {
				continue
			}
			rel, ok := readablePath(skill.Path, workspaceRoot)
			if !ok {
				continue
			}
			skill.Path = rel
			seen[skill.Name] = true
			out = append(out, skill)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func readablePath(path, workspaceRoot string) (string, bool) {
	if strings.TrimSpace(workspaceRoot) == "" {
		return "", false
	}
	rel, err := filepath.Rel(workspaceRoot, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", false
	}
	return filepath.ToSlash(rel), true
}

func skillsIn(root string) []Skill {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	var out []Skill
	for _, entry := range entries {
		path := filepath.Join(root, entry.Name())
		if entry.IsDir() {
			path = filepath.Join(path, "SKILL.md")
			if info, err := os.Stat(path); err != nil || info.IsDir() {
				continue
			}
		} else if !strings.EqualFold(filepath.Ext(entry.Name()), ".md") {
			continue
		}
		if skill, ok := readSkill(path, entry); ok {
			out = append(out, skill)
		}
	}
	return out
}

func readSkill(path string, entry fs.DirEntry) (Skill, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Skill{}, false
	}
	name := strings.TrimSuffix(entry.Name(), filepath.Ext(entry.Name()))
	if entry.IsDir() {
		name = entry.Name()
	}
	skill := Skill{Name: name, Path: path}

	// Frontmatter is optional: a bare Markdown procedure is still a skill.
	if front, _, ok := splitFrontmatter(data); ok {
		fields := map[string]any{}
		if err := yaml.Unmarshal(front, &fields); err == nil {
			if v := strings.TrimSpace(stringField(fields, "name")); v != "" {
				skill.Name = v
			}
			skill.Description = strings.TrimSpace(stringField(fields, "description"))
		}
	}
	if skill.Description == "" {
		skill.Description = firstProseLine(data)
	}
	return skill, true
}

func firstProseLine(data []byte) string {
	_, body, ok := splitFrontmatter(data)
	if !ok {
		body = data
	}
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if len(line) > 200 {
			line = line[:197] + "..."
		}
		return line
	}
	return ""
}

// RenderSkillInventory is what {{skills}} becomes: one line per procedure, with
// the path to read. The index, not the bodies.
func RenderSkillInventory(skills []Skill) string {
	if len(skills) == 0 {
		return "No skills are available."
	}
	b := strings.Builder{}
	b.WriteString("Skills are procedures for repeated work. When a request matches one, read its file before starting, then follow it.\n")
	for _, skill := range skills {
		if skill.Description == "" {
			fmt.Fprintf(&b, "\n- %s — read %s", skill.Name, skill.Path)
			continue
		}
		fmt.Fprintf(&b, "\n- %s: %s — read %s", skill.Name, skill.Description, skill.Path)
	}
	return b.String()
}

func expandSkills(prompt string, skills []Skill) string {
	if !strings.Contains(prompt, SkillsMacro) {
		return prompt
	}
	return strings.ReplaceAll(prompt, SkillsMacro, RenderSkillInventory(skills))
}
