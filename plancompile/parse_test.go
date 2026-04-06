package plancompile

import (
	"strings"
	"testing"
)

func TestParseMarkdown_renderMarkdownShape(t *testing.T) {
	md := `# Plan: p1

**Goal:** ship the feature

**Status:** active

## Steps

- [ ] 1. first thing
  > old result line
- [x] 2. second thing
`
	p, err := ParseMarkdown(md)
	if err != nil {
		t.Fatal(err)
	}
	if p.Goal != "ship the feature" {
		t.Fatalf("goal: %q", p.Goal)
	}
	if len(p.Steps) != 2 || p.Steps[0] != "first thing" || p.Steps[1] != "second thing" {
		t.Fatalf("steps: %#v", p.Steps)
	}
}

func TestParseMarkdown_stepsWithoutHeader(t *testing.T) {
	md := `- [ ] 1. only step
`
	p, err := ParseMarkdown(md)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Steps) != 1 || p.Steps[0] != "only step" {
		t.Fatalf("%#v", p.Steps)
	}
}

func TestParseMarkdown_rejectsNoSteps(t *testing.T) {
	_, err := ParseMarkdown("# No steps\n")
	if err == nil || !strings.Contains(err.Error(), "no steps") {
		t.Fatalf("err: %v", err)
	}
}
