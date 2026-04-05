package localhooks

import (
	"context"
	"errors"
	"testing"
)

func TestUnit_MCPError_PreservesKindAndCause(t *testing.T) {
	err := newMCPError(ErrMCPSessionUnavailable, `mcp "demo": session unavailable`, context.DeadlineExceeded)
	if !errors.Is(err, ErrMCPSessionUnavailable) {
		t.Fatalf("errors.Is: got %v, want ErrMCPSessionUnavailable", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("errors.Is: got %v, want context.DeadlineExceeded", err)
	}
}

func TestUnit_ShouldReconnectAfterMCPError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "session unavailable",
			err:  newMCPError(ErrMCPSessionUnavailable, `mcp "demo": session unavailable`, errors.New("dial tcp")),
			want: false,
		},
		{
			name: "tool error",
			err:  newMCPError(errMCPToolReturnedError, `mcp "demo"."ping": tool error`, errors.New("bad arguments")),
			want: false,
		},
		{
			name: "context canceled",
			err:  context.Canceled,
			want: false,
		},
		{
			name: "list tools transport failure",
			err:  newMCPError(ErrMCPListToolsFailed, `mcp "demo": list-tools failed`, errors.New("connection reset by peer")),
			want: true,
		},
	}

	for _, tt := range tests {
		if got := shouldReconnectAfterMCPError(tt.err); got != tt.want {
			t.Fatalf("%s: got %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestUnit_ErrOAuthNotAuthenticatedAlias(t *testing.T) {
	if ErrOAuthNotAuthenticated != ErrMCPOAuthNotAuthenticated {
		t.Fatalf("alias mismatch: got %v want %v", ErrOAuthNotAuthenticated, ErrMCPOAuthNotAuthenticated)
	}
}
