package taskengine

import (
	"context"
	"errors"
	"testing"
)

func TestUnit_HookToolsUnavailable_WrapsSentinel(t *testing.T) {
	err := HookToolsUnavailable("broken-mcp", errors.New("dial tcp: no such host"))
	if !errors.Is(err, ErrHookToolsUnavailable) {
		t.Fatalf("errors.Is: got %v, want ErrHookToolsUnavailable", err)
	}
}

func TestUnit_HookToolsUnavailable_PreservesCause(t *testing.T) {
	err := HookToolsUnavailable("broken-mcp", context.DeadlineExceeded)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("errors.Is: got %v, want context.DeadlineExceeded", err)
	}
}
