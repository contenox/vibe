package agentdecl_test

import (
	"encoding/json"
	"testing"

	"github.com/contenox/contenox/internal/services/agentdecl"
	"github.com/contenox/contenox/internal/services/hitlservice"
)

const declMissionAgent = `---
name: worker
description: Does a unit of unattended work
tools: Read, Grep
---

You do the work.
`

func vetEmitted(t *testing.T, p *hitlservice.Policy) error {
	t.Helper()
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return hitlservice.VetPolicy(raw)
}

// The existing exit test builds a primary-role IR, so it never set maxTurns and
// never saw this: a real declaration parses as mission-role, and the shipped
// default of 24 made every one of them fail the validator `contenox vet` runs.
func TestUnit_EmittedPolicyForAMissionRoleAgentPassesVet(t *testing.T) {
	t.Parallel()
	cfg := mustConfig(t)
	ir, err := agentdecl.ParseClaudeCode("worker.md", []byte(declMissionAgent), cfg)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !ir.RunsAsSubagent() {
		t.Fatal("a plain declaration must parse as mission-role, or this test proves nothing")
	}
	p, err := agentdecl.EmitPolicy(ir, cfg)
	if err != nil {
		t.Fatalf("emit: %v", err)
	}
	if err := vetEmitted(t, p); err != nil {
		t.Fatalf("emitted policy fails the gate contenox vet applies: %v", err)
	}
}

// A turn cap the runtime cannot honour is worse than none: it reads as enforced
// and does nothing. Only 1 is emitted.
func TestUnit_TurnCapOnlyEmittedWhenItMeansSomething(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		maxTurns int
		want     int
	}{
		{"default keeps the nudge", 0, 0},
		{"one drops the nudge", 1, 1},
		{"above the ceiling is not emitted", 24, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := mustConfig(t)
			cfg.Policy.Compute.MaxTurns = tc.maxTurns
			ir, err := agentdecl.ParseClaudeCode("worker.md", []byte(declMissionAgent), cfg)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			p, err := agentdecl.EmitPolicy(ir, cfg)
			if err != nil {
				t.Fatalf("emit: %v", err)
			}
			got := 0
			if p.Compute != nil {
				got = p.Compute.MaxTurns
			}
			if got != tc.want {
				t.Fatalf("maxTurns = %d, want %d", got, tc.want)
			}
			if err := vetEmitted(t, p); err != nil {
				t.Fatalf("policy fails vet: %v", err)
			}
		})
	}
}
