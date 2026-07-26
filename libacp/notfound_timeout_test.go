package libacp_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/contenox/beam/libacp"
)

// A code that already states the failure must not be overridden by sniffing
// its message: a timeout whose text happens to contain "not found" is still a
// timeout, and misreading it as a missing resource both lies about the cause
// and discards the retryability IsRetryableError would otherwise grant.
func TestUnit_IsNotFound_TimeoutIsNotMisreadAsNotFound(t *testing.T) {
	e := libacp.AsError(fmt.Errorf("upstream model not found in cache: %w", context.DeadlineExceeded))
	t.Logf("code=%d msg=%q", e.Code, e.Message)
	if libacp.IsNotFound(e) {
		t.Fatal("a timeout was classified as a missing resource")
	}
}
