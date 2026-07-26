package taskengine_test

import (
	"context"
	"errors"
	"testing"

	"github.com/contenox/beam/internal/kernel/taskengine"
)

func TestUnit_ToolsToolsUnavailable_WrapsSentinel(t *testing.T) {
	err := taskengine.ToolsToolsUnavailable("broken-mcp", errors.New("dial tcp: no such host"))
	if !errors.Is(err, taskengine.ErrToolsToolsUnavailable) {
		t.Fatalf("errors.Is: got %v, want ErrToolsToolsUnavailable", err)
	}
}

func TestUnit_ToolsToolsUnavailable_PreservesCause(t *testing.T) {
	err := taskengine.ToolsToolsUnavailable("broken-mcp", context.DeadlineExceeded)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("errors.Is: got %v, want context.DeadlineExceeded", err)
	}
}
