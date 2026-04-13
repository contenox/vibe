package localhooks

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/contenox/contenox/planstore"
	"github.com/contenox/contenox/taskengine"
)

// fakeStore is a minimal planstore.Store mock that records the last summary/failure
// write so the tests can assert persist / fallback reached it (or didn't).
type fakeStore struct {
	planstore.Store // embed nil to satisfy interface; we only implement what we need below.
	lastSummary     map[string]string
	lastFailure     map[string]string
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		lastSummary: map[string]string{},
		lastFailure: map[string]string{},
	}
}

func (f *fakeStore) UpdatePlanStepSummary(ctx context.Context, stepID, summaryJSON, chatHistoryJSON string) error {
	f.lastSummary[stepID] = summaryJSON
	return nil
}

func (f *fakeStore) UpdatePlanStepSummaryFailure(ctx context.Context, stepID, raw, errMsg, fallback string) error {
	f.lastFailure[stepID] = raw + "|" + errMsg + "|" + fallback
	return nil
}

// Override these too so embedded nil doesn't panic when planservice code
// exercises them in the test binary. These two are the only ones the hook calls.
func (f *fakeStore) MoveSummaryToLastFailure(ctx context.Context, stepID string) error { return nil }

func TestPlanSummary_Persist_ValidJSON(t *testing.T) {
	store := newFakeStore()
	hook := NewPlanSummaryHook(store)

	ctx := taskengine.WithPlanStepContext(context.Background(), "plan-1", "step-1")
	doc := planstore.SummaryDoc{
		Outcome:         "success",
		Summary:         "wrote the file",
		Artifacts:       []planstore.SummaryArtifact{{Kind: "file", Ref: "NOTE.md"}},
		HandoverForNext: "NOTE.md is on disk; next step should read it.",
	}
	raw, _ := json.Marshal(doc)

	out, dt, err := hook.Exec(ctx, time.Now(), string(raw), false, &taskengine.HookCall{Name: "plan_summary", ToolName: "persist"})
	if err != nil {
		t.Fatal(err)
	}
	if out != "ok" {
		t.Fatalf("expected ok, got %v", out)
	}
	if dt != taskengine.DataTypeString {
		t.Fatalf("data type %v", dt)
	}
	if store.lastSummary["step-1"] == "" {
		t.Fatalf("persist tool should have written to store")
	}
}

func TestPlanSummary_Persist_InvalidJSON_ReturnsInvalidWithoutWriting(t *testing.T) {
	store := newFakeStore()
	hook := NewPlanSummaryHook(store)
	ctx := taskengine.WithPlanStepContext(context.Background(), "plan-1", "step-1")

	out, _, err := hook.Exec(ctx, time.Now(), "not json", false, &taskengine.HookCall{Name: "plan_summary", ToolName: "persist"})
	if err != nil {
		t.Fatal(err)
	}
	if out != "invalid" {
		t.Fatalf("expected invalid, got %v", out)
	}
	if got, ok := store.lastSummary["step-1"]; ok && got != "" {
		t.Fatalf("persist should NOT have written for invalid input, got %q", got)
	}
}

func TestPlanSummary_Persist_MissingRequiredField_ReturnsInvalid(t *testing.T) {
	store := newFakeStore()
	hook := NewPlanSummaryHook(store)
	ctx := taskengine.WithPlanStepContext(context.Background(), "plan-1", "step-1")

	// Missing handover_for_next.
	raw := `{"outcome":"success","summary":"did it"}`
	out, _, _ := hook.Exec(ctx, time.Now(), raw, false, &taskengine.HookCall{Name: "plan_summary", ToolName: "persist"})
	if out != "invalid" {
		t.Fatalf("expected invalid for missing handover, got %v", out)
	}
}

func TestPlanSummary_Persist_BadOutcomeEnum_ReturnsInvalid(t *testing.T) {
	store := newFakeStore()
	hook := NewPlanSummaryHook(store)
	ctx := taskengine.WithPlanStepContext(context.Background(), "plan-1", "step-1")

	raw := `{"outcome":"weird","summary":"did it","handover_for_next":"next"}`
	out, _, _ := hook.Exec(ctx, time.Now(), raw, false, &taskengine.HookCall{Name: "plan_summary", ToolName: "persist"})
	if out != "invalid" {
		t.Fatalf("expected invalid for bad outcome, got %v", out)
	}
}

func TestPlanSummary_Persist_StepDoneMarkerIsStripped(t *testing.T) {
	store := newFakeStore()
	hook := NewPlanSummaryHook(store)
	ctx := taskengine.WithPlanStepContext(context.Background(), "plan-1", "step-1")

	// Valid JSON with a trailing ===STEP_DONE=== marker should still validate.
	body := `{"outcome":"success","summary":"ok","handover_for_next":"hand"}` + "\n===STEP_DONE==="
	out, _, _ := hook.Exec(ctx, time.Now(), body, false, &taskengine.HookCall{Name: "plan_summary", ToolName: "persist"})
	if out != "ok" {
		t.Fatalf("expected ok after marker strip, got %v", out)
	}
}

func TestPlanSummary_Persist_MarkerOnlyIsInvalid(t *testing.T) {
	store := newFakeStore()
	hook := NewPlanSummaryHook(store)
	ctx := taskengine.WithPlanStepContext(context.Background(), "plan-1", "step-1")

	out, _, _ := hook.Exec(ctx, time.Now(), "===STEP_DONE===", false, &taskengine.HookCall{Name: "plan_summary", ToolName: "persist"})
	if out != "invalid" {
		t.Fatalf("marker-only input should be invalid, got %v", out)
	}
}

func TestPlanSummary_Fallback_AlwaysWritesAndReturnsDone(t *testing.T) {
	store := newFakeStore()
	hook := NewPlanSummaryHook(store)
	ctx := taskengine.WithPlanStepContext(context.Background(), "plan-1", "step-1")

	// Feed it a ChatHistory so renderFallbackExecutionResult has something to extract.
	hist := taskengine.ChatHistory{
		Messages: []taskengine.Message{
			{Role: "assistant", Content: "did the work"},
		},
	}
	out, _, err := hook.Exec(ctx, time.Now(), hist, false, &taskengine.HookCall{Name: "plan_summary", ToolName: "fallback"})
	if err != nil {
		t.Fatal(err)
	}
	if out != "done" {
		t.Fatalf("fallback should return done, got %v", out)
	}
	if store.lastFailure["step-1"] == "" {
		t.Fatalf("fallback should have written summary_error + fallback execution_result")
	}
}

func TestPlanSummary_Persist_NoCtx_Errors(t *testing.T) {
	store := newFakeStore()
	hook := NewPlanSummaryHook(store)

	// No WithPlanStepContext — hook must refuse rather than write to an unknown row.
	_, _, err := hook.Exec(context.Background(), time.Now(), `{"outcome":"success","summary":"x","handover_for_next":"y"}`, false,
		&taskengine.HookCall{Name: "plan_summary", ToolName: "persist"})
	if err == nil {
		t.Fatal("expected error when plan/step context is missing")
	}
}

func TestPlanSummary_UnknownTool_Errors(t *testing.T) {
	hook := NewPlanSummaryHook(newFakeStore())
	ctx := taskengine.WithPlanStepContext(context.Background(), "plan-1", "step-1")
	_, _, err := hook.Exec(ctx, time.Now(), "", false, &taskengine.HookCall{Name: "plan_summary", ToolName: "unknown"})
	if err == nil {
		t.Fatal("expected error for unknown tool")
	}
}
