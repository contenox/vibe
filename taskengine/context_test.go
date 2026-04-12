package taskengine

import (
	"context"
	"testing"
)

func TestMergeTemplateVars_preservesParent(t *testing.T) {
	ctx := WithTemplateVars(context.Background(), map[string]string{
		"model":    "m1",
		"provider": "p1",
		"chain":    "c1",
	})
	out := MergeTemplateVars(ctx, map[string]string{
		"request_id":      "req-1",
		"previous_output": "prev",
	})
	got, err := TemplateVarsFromContext(out)
	if err != nil {
		t.Fatal(err)
	}
	if got["model"] != "m1" || got["provider"] != "p1" || got["chain"] != "c1" {
		t.Fatalf("lost parent vars: %#v", got)
	}
	if got["request_id"] != "req-1" || got["previous_output"] != "prev" {
		t.Fatalf("overlay missing: %#v", got)
	}
}

func TestMergeTemplateVars_overlayOverrides(t *testing.T) {
	ctx := WithTemplateVars(context.Background(), map[string]string{"model": "old"})
	out := MergeTemplateVars(ctx, map[string]string{"model": "new"})
	got, err := TemplateVarsFromContext(out)
	if err != nil {
		t.Fatal(err)
	}
	if got["model"] != "new" {
		t.Fatalf("want model new, got %#v", got)
	}
}
