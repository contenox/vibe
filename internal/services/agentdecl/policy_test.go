package agentdecl_test

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/contenox/contenox/internal/services/agentdecl"
	"github.com/contenox/contenox/internal/services/hitlservice"
)

func irWithPosture(t *testing.T, posture agentdecl.Posture, tools []string) *agentdecl.AgentIR {
	t.Helper()
	return &agentdecl.AgentIR{
		Source:       agentdecl.Source{Dialect: agentdecl.DialectClaudeCode},
		Name:         "fixture",
		Description:  "fixture agent",
		SystemPrompt: "Body.",
		Tools:        agentdecl.Tools{Allow: tools},
		Posture:      posture,
	}
}

// permissiveness orders actions so a test can assert an emitted rule never
// grants more than the posture intended.
func permissiveness(a hitlservice.Action) int {
	switch a {
	case hitlservice.ActionDeny:
		return 0
	case hitlservice.ActionApprove:
		return 1
	case hitlservice.ActionAllow:
		return 2
	}
	return -1
}

func ruleFor(p *hitlservice.Policy, tools, tool string) (hitlservice.Rule, bool) {
	for _, r := range p.Rules {
		if r.Tools == tools && r.Tool == tool && len(r.When) == 0 {
			return r, true
		}
	}
	return hitlservice.Rule{}, false
}

// TestUnit_EmittedPolicyPassesVet is the slice's exit test: the policy an
// import produces must satisfy the same validator `contenox vet` runs.
func TestUnit_EmittedPolicyPassesVet(t *testing.T) {
	t.Parallel()
	for _, posture := range []agentdecl.Posture{
		agentdecl.PostureReadOnly,
		agentdecl.PostureAskAlways,
		agentdecl.PostureAutoEdit,
	} {
		t.Run(string(posture), func(t *testing.T) {
			t.Parallel()
			p, err := agentdecl.EmitPolicy(irWithPosture(t, posture, []string{"local_fs.read_file"}), mustConfig(t))
			if err != nil {
				t.Fatalf("emit: %v", err)
			}
			raw, err := json.Marshal(p)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if err := hitlservice.VetPolicy(raw); err != nil {
				t.Fatalf("emitted policy fails contenox vet: %v\n%s", err, raw)
			}
		})
	}
}

// TestUnit_PolicyDeniesCredentialPathsUnderEveryPosture pins the first
// non-negotiable rule: a source format cannot express credential paths, so it
// cannot consent to them either.
func TestUnit_PolicyDeniesCredentialPathsUnderEveryPosture(t *testing.T) {
	t.Parallel()
	for _, posture := range []agentdecl.Posture{
		agentdecl.PostureReadOnly,
		agentdecl.PostureAskAlways,
		agentdecl.PostureAutoEdit,
	} {
		p, err := agentdecl.EmitPolicy(irWithPosture(t, posture, []string{"local_fs.read_file"}), mustConfig(t))
		if err != nil {
			t.Fatalf("%s: emit: %v", posture, err)
		}
		var found bool
		for i, r := range p.Rules {
			if len(r.When) == 0 || r.When[0].Op != hitlservice.OpGlob {
				continue
			}
			found = true
			if r.Action != hitlservice.ActionDeny {
				t.Errorf("%s: credential-path rule is %q, want deny", posture, r.Action)
			}
			// First match wins, so a deny placed after a grant is inert.
			if i != 0 {
				t.Errorf("%s: credential deny is rule %d; it must precede every grant", posture, i)
			}
		}
		if !found {
			t.Errorf("%s: emitted no credential-path deny", posture)
		}
	}
}

// TestUnit_PostureNeverWidensPastIntent pins the second non-negotiable rule.
// auto_edit means edits, not shell.
func TestUnit_PostureNeverWidensPastIntent(t *testing.T) {
	t.Parallel()
	want := map[agentdecl.Posture]struct{ read, write, shell hitlservice.Action }{
		agentdecl.PostureReadOnly:  {hitlservice.ActionAllow, hitlservice.ActionDeny, hitlservice.ActionDeny},
		agentdecl.PostureAskAlways: {hitlservice.ActionAllow, hitlservice.ActionApprove, hitlservice.ActionApprove},
		agentdecl.PostureAutoEdit:  {hitlservice.ActionAllow, hitlservice.ActionAllow, hitlservice.ActionApprove},
	}
	for posture, exp := range want {
		p, err := agentdecl.EmitPolicy(irWithPosture(t, posture, []string{"local_fs.read_file"}), mustConfig(t))
		if err != nil {
			t.Fatalf("%s: emit: %v", posture, err)
		}
		for _, c := range []struct {
			tools, tool string
			want        hitlservice.Action
		}{
			{"local_fs", "read_file", exp.read},
			{"local_fs", "write_file", exp.write},
			{"local_fs", "edit_file", exp.write},
			{"local_fs", "sed", exp.write},
			{"local_shell", "local_shell", exp.shell},
		} {
			r, ok := ruleFor(p, c.tools, c.tool)
			if !ok {
				t.Errorf("%s: no rule for %s.%s", posture, c.tools, c.tool)
				continue
			}
			if permissiveness(r.Action) > permissiveness(c.want) {
				t.Errorf("%s: %s.%s granted %q, wider than the posture's %q",
					posture, c.tools, c.tool, r.Action, c.want)
			}
			if r.Action != c.want {
				t.Errorf("%s: %s.%s = %q, want %q", posture, c.tools, c.tool, r.Action, c.want)
			}
		}
	}
}

// TestUnit_UnsafePostureRefusedByPolicyEmitter pins the third non-negotiable
// rule, at the policy half.
func TestUnit_UnsafePostureRefusedByPolicyEmitter(t *testing.T) {
	t.Parallel()
	_, err := agentdecl.EmitPolicy(irWithPosture(t, agentdecl.PostureUnsafe, nil), mustConfig(t))
	var unsafe *agentdecl.ErrUnsafePosture
	if !errors.As(err, &unsafe) {
		t.Fatalf("want ErrUnsafePosture, got %v", err)
	}
}

// TestUnit_OperatorMappedToolFallsToDefaultAction covers the case the guide
// warns about: mapping a name makes a tool reachable, not permitted.
func TestUnit_OperatorMappedToolFallsToDefaultAction(t *testing.T) {
	t.Parallel()
	p, err := agentdecl.EmitPolicy(
		irWithPosture(t, agentdecl.PostureAutoEdit, []string{"tavily.search"}), mustConfig(t))
	if err != nil {
		t.Fatalf("emit: %v", err)
	}
	if _, ok := ruleFor(p, "tavily", "search"); ok {
		t.Error("a tool the postures do not name must not receive a rule")
	}
	if p.DefaultAction != hitlservice.ActionApprove {
		t.Errorf("default_action = %q, want approve so an unruled tool asks a human", p.DefaultAction)
	}
}

func TestUnit_MaxTurnsOnlyTightensTheToolCallCeiling(t *testing.T) {
	t.Parallel()
	cfg := mustConfig(t)

	tight := 5
	ir := irWithPosture(t, agentdecl.PostureAskAlways, []string{"local_fs.read_file"})
	ir.Budgets.MaxTurns = &tight
	p, err := agentdecl.EmitPolicy(ir, cfg)
	if err != nil {
		t.Fatalf("emit: %v", err)
	}
	if p.Compute == nil || p.Compute.MaxToolCalls != tight {
		t.Errorf("a tighter source bound must apply, got %+v", p.Compute)
	}

	loose := cfg.Policy.Compute.MaxToolCalls + 1000
	ir.Budgets.MaxTurns = &loose
	p, err = agentdecl.EmitPolicy(ir, cfg)
	if err != nil {
		t.Fatalf("emit: %v", err)
	}
	if p.Compute.MaxToolCalls != cfg.Policy.Compute.MaxToolCalls {
		t.Errorf("a looser source bound must not widen the shipped ceiling, got %d want %d",
			p.Compute.MaxToolCalls, cfg.Policy.Compute.MaxToolCalls)
	}
}

// TestUnit_AlwaysAllowGrantsAConnectedToolWithoutReachingCredentials is the
// operator's answer to a connected tool that should run unattended.
func TestUnit_AlwaysAllowGrantsAConnectedToolWithoutReachingCredentials(t *testing.T) {
	t.Parallel()
	cfg := mustConfig(t)
	cfg.Policy.AlwaysAllow = []agentdecl.StandingRule{{Tools: "tavily", Tool: "search"}}

	p, err := agentdecl.EmitPolicy(irWithPosture(t, agentdecl.PostureAskAlways, []string{"tavily.search"}), cfg)
	if err != nil {
		t.Fatalf("emit: %v", err)
	}
	r, ok := ruleFor(p, "tavily", "search")
	if !ok || r.Action != hitlservice.ActionAllow {
		t.Fatalf("connected tool not granted: %+v", p.Rules)
	}
	// The credential deny must still come first, or a grant could outrank it.
	if len(p.Rules) == 0 || p.Rules[0].Action != hitlservice.ActionDeny {
		t.Fatalf("rule 0 is %+v, want the credential deny", p.Rules[0])
	}
}
