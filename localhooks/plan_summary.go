// Package localhooks: plan_summary hook — persists the typed-JSON handover
// produced by the summarizer chain. This hook is the terminal node in the
// per-step summarizer subgraph compiled by plancompile.Compile.
//
// Two tools under one hook name:
//
//   - "persist"  — validates the summarizer's output (strict JSON + required
//                   fields + outcome enum). On success writes it to planstore
//                   and returns "ok". On any validation failure returns
//                   "invalid" WITHOUT writing, so the DAG's repair branch can
//                   take over.
//
//   - "fallback" — unconditional terminal write. Invoked when both the first
//                   summarizer attempt and its repair attempt failed
//                   validation (or when the summarizer LLM errored). Writes
//                   summary_error + a fallback ExecutionResult rendered from
//                   whatever ChatHistory the hook received as input. Always
//                   returns "done".
//
// Plan/step identity: read from ctx via taskengine.PlanStepContextFromContext.
// Set by planservice.Next before calling engine.Execute; this keeps compiled
// chain JSON free of row-identity information.
package localhooks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/contenox/contenox/planstore"
	"github.com/contenox/contenox/taskengine"
	"github.com/getkin/kin-openapi/openapi3"
)

const planSummaryHookName = "plan_summary"

// PlanSummaryHook is registered under the "plan_summary" hook name and dispatches
// to the persist/fallback tools compiled into the per-step summarizer subgraph.
type PlanSummaryHook struct {
	store planstore.Store
}

// NewPlanSummaryHook wires the hook against a planstore.Store so persist and
// fallback tools can write directly to the plan_steps table.
func NewPlanSummaryHook(store planstore.Store) taskengine.HookRepo {
	return &PlanSummaryHook{store: store}
}

// Exec routes to persist / fallback based on HookCall.ToolName.
func (h *PlanSummaryHook) Exec(ctx context.Context, startTime time.Time, input any, debug bool, hookCall *taskengine.HookCall) (any, taskengine.DataType, error) {
	if hookCall == nil {
		return nil, taskengine.DataTypeAny, errors.New("plan_summary: hook call required")
	}
	toolName := hookCall.ToolName
	if toolName == "" {
		toolName = hookCall.Name
	}
	switch toolName {
	case "persist":
		return h.persist(ctx, input)
	case "fallback":
		return h.fallback(ctx, input)
	default:
		return nil, taskengine.DataTypeAny, fmt.Errorf("plan_summary: unknown tool %q", toolName)
	}
}

// persist validates the summarizer's JSON output and writes it on success.
// Returns "ok" on successful persist, "invalid" on any validation failure
// (without writing). The compiled DAG branches on this string.
func (h *PlanSummaryHook) persist(ctx context.Context, input any) (any, taskengine.DataType, error) {
	_, stepID, ok := taskengine.PlanStepContextFromContext(ctx)
	if !ok || stepID == "" {
		return nil, taskengine.DataTypeAny, errors.New("plan_summary: plan/step context not set on ctx (WithPlanStepContext)")
	}

	raw, chatHistoryJSON := extractSummaryPayload(input)
	raw = stripStepDoneMarker(raw)
	if raw == "" {
		return "invalid", taskengine.DataTypeString, nil
	}

	var doc planstore.SummaryDoc
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		return "invalid", taskengine.DataTypeString, nil
	}
	if !validateSummaryDoc(&doc) {
		return "invalid", taskengine.DataTypeString, nil
	}

	// Re-marshal canonical form so DB storage is normalized even if the model
	// emitted a superset of fields.
	canonical, err := json.Marshal(doc)
	if err != nil {
		return "invalid", taskengine.DataTypeString, nil
	}

	if err := h.store.UpdatePlanStepSummary(ctx, stepID, string(canonical), chatHistoryJSON); err != nil {
		return nil, taskengine.DataTypeAny, fmt.Errorf("plan_summary: persist: %w", err)
	}
	return "ok", taskengine.DataTypeString, nil
}

// fallback writes summary_error + a fallback ExecutionResult string rendered
// from whatever ChatHistory the hook received (compiled chain should bind its
// InputVar to the executor's terminal task so this hook sees the executor's
// full conversation, not the summarizer's).
func (h *PlanSummaryHook) fallback(ctx context.Context, input any) (any, taskengine.DataType, error) {
	_, stepID, ok := taskengine.PlanStepContextFromContext(ctx)
	if !ok || stepID == "" {
		return nil, taskengine.DataTypeAny, errors.New("plan_summary: plan/step context not set on ctx (WithPlanStepContext)")
	}

	raw, _ := extractSummaryPayload(input)
	fallbackResult := renderFallbackExecutionResult(input)
	errMsg := "summarizer failed validation after repair attempt"

	if err := h.store.UpdatePlanStepSummaryFailure(ctx, stepID, raw, errMsg, fallbackResult); err != nil {
		return nil, taskengine.DataTypeAny, fmt.Errorf("plan_summary: fallback: %w", err)
	}
	return "done", taskengine.DataTypeString, nil
}

// extractSummaryPayload reduces various input shapes produced by upstream tasks
// into (rawString, chatHistoryJSON). rawString is the best-effort JSON payload
// to validate (the last assistant message's Content when input is a ChatHistory).
// chatHistoryJSON is a marshalled ChatHistory when input was one, else "".
func extractSummaryPayload(input any) (string, string) {
	switch v := input.(type) {
	case nil:
		return "", ""
	case string:
		return v, ""
	case taskengine.ChatHistory:
		last := lastAssistantContent(v)
		hj, _ := json.Marshal(v)
		return last, string(hj)
	case *taskengine.ChatHistory:
		if v == nil {
			return "", ""
		}
		last := lastAssistantContent(*v)
		hj, _ := json.Marshal(*v)
		return last, string(hj)
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return "", ""
		}
		return string(b), ""
	}
}

// lastAssistantContent mirrors planservice.chatHistoryForPlanStepPersistedResult:
// scan backwards for the last assistant message with non-empty Content; fall
// back to the last message's Content. Kept here to avoid a planservice import
// cycle (planstore → planservice would reverse the existing dependency).
func lastAssistantContent(h taskengine.ChatHistory) string {
	if len(h.Messages) == 0 {
		return ""
	}
	for i := len(h.Messages) - 1; i >= 0; i-- {
		if h.Messages[i].Role == "assistant" && h.Messages[i].Content != "" {
			return h.Messages[i].Content
		}
	}
	return h.Messages[len(h.Messages)-1].Content
}

// stripStepDoneMarker removes a trailing "===STEP_DONE===" line (or the marker
// alone) so a summarizer that echoes the marker after its JSON payload still
// validates. Defense against the same lossy pattern that motivated this work.
func stripStepDoneMarker(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	const marker = "===STEP_DONE==="
	if s == marker {
		return ""
	}
	// Trailing marker on its own line.
	lines := strings.Split(s, "\n")
	for len(lines) > 0 {
		last := strings.TrimSpace(lines[len(lines)-1])
		if last == marker {
			lines = lines[:len(lines)-1]
			continue
		}
		break
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

// validateSummaryDoc enforces the locked contract: outcome must be in enum,
// summary and handover_for_next must be non-empty.
func validateSummaryDoc(d *planstore.SummaryDoc) bool {
	if d == nil {
		return false
	}
	if _, ok := planstore.ValidOutcomes()[d.Outcome]; !ok {
		return false
	}
	if strings.TrimSpace(d.Summary) == "" {
		return false
	}
	if strings.TrimSpace(d.HandoverForNext) == "" {
		return false
	}
	return true
}

// renderFallbackExecutionResult produces the legacy single-string representation
// of a step's output when the summarizer failed. Reuses the same last-assistant
// rule applied for years in planservice.formatTaskOutput — but sees *input*
// directly since the hook is invoked with the executor's ChatHistory bound via
// InputVar in the compiled chain.
func renderFallbackExecutionResult(input any) string {
	switch v := input.(type) {
	case nil:
		return ""
	case string:
		return v
	case taskengine.ChatHistory:
		return lastAssistantContent(v)
	case *taskengine.ChatHistory:
		if v == nil {
			return ""
		}
		return lastAssistantContent(*v)
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return ""
		}
		return string(b)
	}
}

// Supports advertises the hook name to resolveHookNames and the MCP/remote
// allowlist machinery. Only the hook NAME is advertised here; tool names
// ("persist", "fallback") live under HookCall.ToolName.
func (h *PlanSummaryHook) Supports(ctx context.Context) ([]string, error) {
	return []string{planSummaryHookName}, nil
}

// GetSchemasForSupportedHooks returns empty: this hook is invoked directly by
// the compiled plan DAG (HandleHook), not surfaced as a tool the model can
// choose via execute_tool_calls.
func (h *PlanSummaryHook) GetSchemasForSupportedHooks(ctx context.Context) (map[string]*openapi3.T, error) {
	return map[string]*openapi3.T{}, nil
}

// GetToolsForHookByName returns empty for the same reason as above.
func (h *PlanSummaryHook) GetToolsForHookByName(ctx context.Context, name string) ([]taskengine.Tool, error) {
	return []taskengine.Tool{}, nil
}

var _ taskengine.HookRepo = (*PlanSummaryHook)(nil)
