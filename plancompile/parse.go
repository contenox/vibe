package plancompile

import (
	"fmt"
	"regexp"
	"strings"
)

var (
	reGoal = regexp.MustCompile(`(?i)^\*\*Goal:\*\*\s*(.+)$`)
	// Matches: - [ ] 1. description  OR  - [x] 2. ...
	reStepLine = regexp.MustCompile(`^\s*-\s*\[[^\]]*\]\s*(\d+)\.\s*(.+)$`)
)

// ParsedPlan is the result of parsing plan Markdown (same shape as planservice.renderMarkdown).
type ParsedPlan struct {
	Goal  string
	Steps []string
}

// ParseMarkdown extracts goal and ordered step descriptions from rendered plan Markdown.
// It understands lines like "**Goal:** ...", "## Steps", and checklist items "- [ ] 1. text".
// Indented lines starting with ">" (execution result excerpts) are skipped.
func ParseMarkdown(md string) (*ParsedPlan, error) {
	lines := strings.Split(md, "\n")
	var goal string
	var steps []string

	for _, line := range lines {
		trim := strings.TrimRight(line, "\r")
		if strings.HasPrefix(strings.TrimLeft(trim, " \t"), ">") {
			continue
		}
		if m := reGoal.FindStringSubmatch(strings.TrimSpace(trim)); m != nil {
			goal = strings.TrimSpace(m[1])
			continue
		}
		lower := strings.ToLower(strings.TrimSpace(trim))
		if strings.HasPrefix(lower, "## steps") {
			continue
		}
		if goal == "" && strings.HasPrefix(lower, "#") {
			continue
		}
		if m := reStepLine.FindStringSubmatch(trim); m != nil {
			desc := strings.TrimSpace(m[2])
			if desc != "" {
				steps = append(steps, desc)
			}
		}
	}

	if len(steps) == 0 {
		return nil, fmt.Errorf("plancompile: no steps found in markdown (expected lines like \"- [ ] 1. ...\" under ## Steps)")
	}
	return &ParsedPlan{Goal: goal, Steps: steps}, nil
}
