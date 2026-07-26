// Package accessview computes STRUCTURED HITL policy verdicts for a batch of
// workspace-root-relative paths: for each path, whether it is reachable inside
// the workspace root and, if so, what the policy would decide for a read and a
// write access to it. It is the batch, structured-reason sibling of
// runtime/agentview — agentview answers "what would the agent's OWN gates do
// for a single path, rendered as a quiet UI verdict"; accessview answers "give
// me the full policy decision (action, reason, matched rule index) for every
// path in this list, from the server's own idea of what's there", so a caller
// (Beam's access-preview panel) can render exactly why a path is gated without
// re-deriving the policy engine's reasoning client-side.
//
// Same two gates as agentview, run directly rather than reimplemented:
//
//   - Reachability — runtime/vfs View.Resolve (containment + symlink-escape
//     resolution). An unreachable path short-circuits with no policy eval.
//   - Policy verdict — hitlservice.Service.Evaluate for the read and write
//     sub-tools of local_fs, exactly as the live agent's tool dispatch gates.
//
// Bare paths in, enriched verdicts out: the client sends only path strings
// (never isDir or reachability — those would let a dishonest or buggy client
// manufacture a corrupted verdict), and the server derives everything else
// from its own view of the filesystem.
package accessview

import (
	"context"
	"os"

	"github.com/contenox/beam/internal/services/hitlservice"
	"github.com/contenox/beam/internal/services/vfs"
)

// DimensionVerdict is the policy decision for one access dimension (read or
// write) of a path. Action mirrors hitlservice.Action ("allow" | "approve" |
// "deny") as a plain string so this package's wire shape carries no import of
// hitlservice into JSON consumers. Reason is always populated (never omitted
// for "allow" the way agentview.Verdict's reason is) — the whole point of this
// package over agentview is a caller that wants the full "why", not just the
// interesting cases. Rule is set only when Reason is ReasonMatchedRule.
type DimensionVerdict struct {
	Action string `json:"action"`
	Reason string `json:"reason,omitempty"`
	Rule   *int   `json:"rule,omitempty"`
}

// PathVerdict is the evaluated access for one requested path. Read and Write
// are nil when the path is not Reachable — no policy is evaluated for a path
// outside the workspace root, so there is no verdict to report.
type PathVerdict struct {
	Path      string            `json:"path"`
	Reachable bool              `json:"reachable"`
	Read      *DimensionVerdict `json:"read,omitempty"`
	Write     *DimensionVerdict `json:"write,omitempty"`
}

// probe is a throwaway read_file evaluation used ONLY to learn which policy is
// in effect (EvaluationResult.PolicyName) when the batch itself yields no
// reachable path to evaluate — an empty request, or a batch that is entirely
// unreachable. "." is always a valid args value for this purpose: the policy
// resolution hitlservice performs (active-policy KV -> constructor fallback ->
// built-in default) does not depend on the tool args at all, only on which
// rule/default ends up applying to them, which this call discards.
const (
	probeTools = "local_fs"
	probeTool  = "read_file"
)

// Evaluator binds a workspace View to a HITL policy evaluator, built ONCE per
// request and reused across every path in the batch — see Evaluate. It holds
// no per-path state: hitlservice.Service is an interface (Evaluate is its only
// exported decision entry point), so this package always calls through it
// rather than loading and matching a Policy document itself, which would
// duplicate hitlservice's rule-matching logic instead of running it.
type Evaluator struct {
	view *vfs.View
	hitl hitlservice.Service
}

// NewEvaluator binds a workspace view + a HITL service already resolved to the
// policy the caller wants evaluated (see hitlservice.NewWithDefaultPolicy /
// the PolicyEvaluatorFactory pattern serverapi and localfileapi use).
func NewEvaluator(view *vfs.View, hitl hitlservice.Service) *Evaluator {
	return &Evaluator{view: view, hitl: hitl}
}

// Evaluate resolves the policy verdict for every path in paths, in the order
// given, using the ONE Evaluator binding (no per-path rebuild). It returns the
// resolved policy name (hitlservice.EvaluationResult.PolicyName — the ACTUAL
// policy that gated the decisions: the active-policy KV value or the
// configured fallback, not merely whatever name the caller requested) alongside
// the per-path verdicts. policyName is resolved from the first path evaluation
// that actually reaches the policy engine; when none does (an empty batch, or
// every path is unreachable) a single throwaway probe call learns it, so the
// response always names the policy that would have gated the batch.
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

// verdict evaluates one workspace-root-relative path: reachability first (a
// resolution failure — including a symlink escape — short-circuits to
// {Reachable:false} with nil Read/Write and no policy eval), then isDir (via a
// stat of the resolved path; a path that resolves but does not exist on disk is
// treated as a file — "what WOULD happen" if the agent acted on it now), then
// both policy dimensions.
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

	// Mirrors agentview.Evaluator.Verdict's read/write verb mapping
	// (agentview.go:96-107): read_file/list_dir by isDir, write_file for both.
	// agentview discards the structured reason; this package keeps it.
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

// toDimensionVerdict maps a hitlservice.EvaluationResult onto the wire shape,
// carrying the rule index only when it is what actually decided the action.
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
