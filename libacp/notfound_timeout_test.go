package libacp_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/contenox/contenox/libacp"
)

// Pins that a timeout whose message contains "not found" is not misclassified.
func TestUnit_IsNotFound_TimeoutIsNotMisreadAsNotFound(t *testing.T) {
	e := libacp.AsError(fmt.Errorf("upstream model not found in cache: %w", context.DeadlineExceeded))
	t.Logf("code=%d msg=%q", e.Code, e.Message)
	if libacp.IsNotFound(e) {
		t.Fatal("a timeout was classified as a missing resource")
	}
}
