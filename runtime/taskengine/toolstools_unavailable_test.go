package taskengine

import (
	"context"
	"errors"
	"testing"
)

func TestUnit_ToolsToolsUnavailable_WrapsSentinel(t *testing.T) {
	err := ToolsToolsUnavailable("broken-mcp", errors.New("dial tcp: no such host"))
	if !errors.Is(err, ErrToolsToolsUnavailable) {
		t.Fatalf("errors.Is: got %v, want ErrToolsToolsUnavailable", err)
	}
}

func TestUnit_ToolsToolsUnavailable_PreservesCause(t *testing.T) {
	err := ToolsToolsUnavailable("broken-mcp", context.DeadlineExceeded)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("errors.Is: got %v, want context.DeadlineExceeded", err)
	}
}
