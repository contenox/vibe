// Package agentview computes, for a workspace path, the access the agent
// would actually have, by running the agent's own gates rather than a
// parallel reimplementation.
package agentview

import (
	"context"
	"fmt"

	"github.com/contenox/beam/internal/services/hitlservice"
	"github.com/contenox/beam/internal/services/vfs"
)

// Op names the access being evaluated: OpRead or OpWrite.
type Op string

const (
	// OpRead is a read access (read_file for files, list_dir for directories).
	OpRead Op = "read"
	// OpWrite is a write access (write_file / create-inside for directories).
	OpWrite Op = "write"
)

// Verdict is the agent's access to one path. Read/Write are empty when
// Reachable is false; ReadReason/WriteReason are omitted for allow verdicts.
type Verdict struct {
	Reachable   bool               `json:"reachable"`
	Read        hitlservice.Action `json:"read,omitempty"`
	Write       hitlservice.Action `json:"write,omitempty"`
	ReadReason  string             `json:"readReason,omitempty"`
	WriteReason string             `json:"writeReason,omitempty"`
}

// Evaluator binds a workspace View to a HITL policy evaluator, reused
// across entries. policyName records the policy baked into hitl.
type Evaluator struct {
	view       *vfs.View
	hitl       hitlservice.Service
	policyName string
}

// NewEvaluator binds a workspace view and HITL service to policyName.
func NewEvaluator(view *vfs.View, hitl hitlservice.Service, policyName string) *Evaluator {
	return &Evaluator{view: view, hitl: hitl, policyName: policyName}
}

// PolicyName returns the name of the policy these verdicts reflect.
func (e *Evaluator) PolicyName() string {
	if e == nil {
		return ""
	}
	return e.policyName
}

// Verdict evaluates one path. An unreachable path short-circuits to
// Reachable:false with no policy evaluation.
func (e *Evaluator) Verdict(ctx context.Context, rootRelPath string, isDir bool) Verdict {
	if e == nil || e.view == nil || e.hitl == nil {
		return Verdict{}
	}

	if _, err := e.view.Resolve(rootRelPath); err != nil {
		return Verdict{Reachable: false}
	}

	v := Verdict{Reachable: true}
	args := map[string]any{"path": rootRelPath}

	readTool := "read_file"
	if isDir {
		readTool = "list_dir"
	}
	if res, err := e.hitl.Evaluate(ctx, "local_fs", readTool, args); err == nil {
		v.Read = res.Action
		v.ReadReason = interestingReason(res)
	}
	if res, err := e.hitl.Evaluate(ctx, "local_fs", "write_file", args); err == nil {
		v.Write = res.Action
		v.WriteReason = interestingReason(res)
	}
	return v
}

// interestingReason explains a non-allow verdict; allow returns "".
func interestingReason(res hitlservice.EvaluationResult) string {
	if res.Action == hitlservice.ActionAllow {
		return ""
	}
	if res.MatchedRule != nil {
		return fmt.Sprintf("matched rule %d", *res.MatchedRule)
	}
	return "default action"
}
