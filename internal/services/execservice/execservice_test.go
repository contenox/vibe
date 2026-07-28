package execservice

import (
	"context"
	"errors"
	"testing"

	"github.com/contenox/contenox/internal/errdefs"
)

func TestUnit_ExecService_Execute_RejectsEmptyPrompt(t *testing.T) {
	svc := NewExec(context.Background(), nil)

	_, err := svc.Execute(context.Background(), &TaskRequest{Prompt: ""})
	if err == nil {
		t.Fatal("expected error on empty prompt")
	}
	if !errors.Is(err, errdefs.ErrEmptyRequestBody) {
		t.Fatalf("expected ErrEmptyRequestBody, got: %v", err)
	}

	_, err = svc.Execute(context.Background(), nil)
	if err == nil {
		t.Fatal("expected error on nil request")
	}
	if !errors.Is(err, errdefs.ErrEmptyRequest) {
		t.Fatalf("expected ErrEmptyRequest, got: %v", err)
	}
}
