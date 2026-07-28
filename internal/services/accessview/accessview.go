// Package accessview computes HITL policy verdicts (reachability plus
// read/write decisions) for a batch of workspace-relative paths, always
// returning the full reason rather than only the interesting cases.
package accessview

import (
	"context"
	"os"

	"github.com/contenox/contenox/internal/services/hitlservice"
	"github.com/contenox/contenox/internal/services/vfs"
)

// DimensionVerdict is the policy decision for one access dimension; Rule is
// set only when Reason is ReasonMatchedRule.
type DimensionVerdict struct {
	Action string `json:"action"`
	Reason string `json:"reason,omitempty"`
	Rule   *int   `json:"rule,omitempty"`
}

// PathVerdict is the evaluated access for one path; Read/Write are nil when
// Reachable is false.
type PathVerdict struct {
	Path      string            `json:"path"`
	Reachable bool              `json:"reachable"`
	Read      *DimensionVerdict `json:"read,omitempty"`
	Write     *DimensionVerdict `json:"write,omitempty"`
}

// probe learns the active policy name when no path in the batch is reachable.
const (
	probeTools = "local_fs"
	probeTool  = "read_file"
)

// Evaluator binds a workspace View to a HITL policy evaluator, reused
// across a batch; it holds no per-path state.
type Evaluator struct {
	view *vfs.View
	hitl hitlservice.Service
}

// NewEvaluator binds a workspace view and an already-resolved HITL service.
func NewEvaluator(view *vfs.View, hitl hitlservice.Service) *Evaluator {
	return &Evaluator{view: view, hitl: hitl}
}

// Evaluate resolves the policy verdict for every path in paths, in order,
// alongside the resolved policy name.
func (e *Evaluator) Evaluate(ctx context.Context, paths []string) (policyName string, verdicts []PathVerdict) {
	if e == nil || e.view == nil || e.hitl == nil {
		return "", nil
	}
	verdicts = make([]PathVerdict, len(paths))
	for i, p := range paths {
		v, resolved := e.verdict(ctx, p)
		verdicts[i] = v
		if policyName == "" && resolved != "" {
			policyName = resolved
		}
	}
	if policyName == "" {
		if res, err := e.hitl.Evaluate(ctx, probeTools, probeTool, map[string]any{"path": "."}); err == nil {
			policyName = res.PolicyName
		}
	}
	return policyName, verdicts
}

// verdict evaluates one path: an unreachable path short-circuits with no
// policy eval; otherwise isDir (via stat) gates the read tool choice.
func (e *Evaluator) verdict(ctx context.Context, rootRelPath string) (PathVerdict, string) {
	pv := PathVerdict{Path: rootRelPath}

	abs, err := e.view.Resolve(rootRelPath)
	if err != nil {
		return pv, ""
	}
	pv.Reachable = true

	isDir := false
	if info, statErr := os.Stat(abs); statErr == nil {
		isDir = info.IsDir()
	}

	args := map[string]any{"path": rootRelPath}
	readTool := "read_file"
	if isDir {
		readTool = "list_dir"
	}

	var policyName string
	if res, err := e.hitl.Evaluate(ctx, "local_fs", readTool, args); err == nil {
		pv.Read = toDimensionVerdict(res)
		policyName = res.PolicyName
	}
	if res, err := e.hitl.Evaluate(ctx, "local_fs", "write_file", args); err == nil {
		pv.Write = toDimensionVerdict(res)
		policyName = res.PolicyName
	}
	return pv, policyName
}

// toDimensionVerdict maps res onto the wire shape, keeping Rule only when it decided the action.
func toDimensionVerdict(res hitlservice.EvaluationResult) *DimensionVerdict {
	dv := &DimensionVerdict{
		Action: string(res.Action),
		Reason: res.Reason,
	}
	if res.Reason == hitlservice.ReasonMatchedRule {
		dv.Rule = res.MatchedRule
	}
	return dv
}
