// Package agentview computes, for a workspace path, the access the agent would
// actually have — "see the tree as the agent sees it". A verdict is produced by
// running the agent's OWN gates against a hypothetical access, never a parallel
// reimplementation (which would drift). Two gates, both already server-side:
//
//   - Reachability — runtime/vfs View.Resolve (containment + symlink-escape
//     resolution). A path that escapes the workspace root is unreachable.
//   - Policy verdict — hitlservice.Evaluate for the read and write sub-tools of
//     local_fs, so "what would the policy do if the agent read/wrote path P" is
//     exact: the same engine that gates real calls.
//
// The synthetic path argument is workspace-root-relative — the same form the
// local_fs tool passes ("relative to the project root") — so verdicts match what
// the agent would actually get.
package agentview

import (
	"context"
	"fmt"

	"github.com/contenox/beam/internal/services/hitlservice"
	"github.com/contenox/beam/internal/services/vfs"
)

// Op names the access being evaluated. Kept as a small vocabulary so callers can
// talk about the two dimensions a Verdict reports.
type Op string

const (
	// OpRead is a read access (read_file for files, list_dir for directories).
	OpRead Op = "read"
	// OpWrite is a write access (write_file / create-inside for directories).
	OpWrite Op = "write"
)

// Verdict is the agent's access to one path. Read/Write mirror
// hitlservice.Action ("allow" | "approve" | "deny"); they are empty when the
// path is not Reachable (no policy is evaluated in that case — the boundary is
// the answer). ReadReason/WriteReason explain a non-allow verdict (which rule /
// why) and are omitted for the uninteresting allow case to keep the payload
// quiet.
type Verdict struct {
	Reachable   bool               `json:"reachable"`
	Read        hitlservice.Action `json:"read,omitempty"`
	Write       hitlservice.Action `json:"write,omitempty"`
	ReadReason  string             `json:"readReason,omitempty"`
	WriteReason string             `json:"writeReason,omitempty"`
}

// Evaluator binds a workspace View to a HITL policy evaluator. It holds no
// per-path state, so a single Evaluator is reused across every listed entry.
//
// The policy the verdicts reflect is the one baked into hitl at construction;
// policyName records that policy's name (for reference/annotation) — the
// hitlservice.Service is the authority that actually evaluates.
type Evaluator struct {
	view       *vfs.View
	hitl       hitlservice.Service
	policyName string
}

// NewEvaluator binds a workspace view + a HITL service already resolved to the
// session's active policy (policyName names that policy).
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

// Verdict evaluates one workspace-root-relative path. Reachability is checked
// first; an unreachable path short-circuits to {Reachable:false} with empty
// actions (no policy evaluation). For directories Read is evaluated as
// local_fs.list_dir; Write is evaluated as local_fs.write_file for both files
// and directories (the create-inside proxy — the policy globs that gate writes
// key on the path prefix, which the directory path itself carries).
func (e *Evaluator) Verdict(ctx context.Context, rootRelPath string, isDir bool) Verdict {
	if e == nil || e.view == nil || e.hitl == nil {
		return Verdict{}
	}

	// Reachability gate. Resolve contains the path within the view root and
	// follows symlinks, so an entry linking outside the root is caught here.
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

// interestingReason returns a short explanation for a non-allow verdict, and ""
// for allow (whether by rule or default) — the boring case the UI need not
// annotate.
func interestingReason(res hitlservice.EvaluationResult) string {
	if res.Action == hitlservice.ActionAllow {
		return ""
	}
	if res.MatchedRule != nil {
		return fmt.Sprintf("matched rule %d", *res.MatchedRule)
	}
	return "default action"
}
