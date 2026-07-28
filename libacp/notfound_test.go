package libacp_test

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/contenox/beam/libacp"
)

func TestIsNotFound(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "nil",
			err:  nil,
			want: false,
		},
		{
			name: "typed error with resource-not-found code",
			err:  libacp.NewError(libacp.ErrResourceNotFound, "no such resource"),
			want: true,
		},
		{
			name: "typed error with resource-not-found code and unrelated message",
			err:  libacp.NewError(libacp.ErrResourceNotFound, "gone"),
			want: true,
		},
		{
			name: "typed internal error whose message says not found",
			err:  libacp.NewError(libacp.ErrInternalError, "file not found: /tmp/x"),
			want: true,
		},
		{
			name: "typed error message match is case-insensitive",
			err:  libacp.NewError(libacp.ErrInternalError, "File Not Found"),
			want: true,
		},
		{
			name: "typed error wrapped by fmt.Errorf is still classified",
			err:  fmt.Errorf("read %s: %w", "/tmp/x", libacp.NewError(libacp.ErrResourceNotFound, "nope")),
			want: true,
		},
		{
			name: "typed error unrelated to existence",
			err:  libacp.NewError(libacp.ErrInvalidParams, "bad path"),
			want: false,
		},
		{
			name: "typed auth error",
			err:  libacp.NewError(libacp.ErrAuthRequired, "authenticate first"),
			want: false,
		},
		{
			// A raw error's text is never classified, even when suggestive.
			name: "raw error whose text contains not found is not classified",
			err:  errors.New("agent not found: contenox-acp"),
			want: false,
		},
		{
			name: "startup error must not be classified",
			err:  fmt.Errorf("spawn agent: %w", libacp.ErrAgentStartFailed),
			want: false,
		},
		{
			name: "exec.ErrNotFound must not be classified",
			err:  fmt.Errorf("lookup binary: %w", exec.ErrNotFound),
			want: false,
		},
		{
			name: "os.ErrNotExist alone is not an ACP classification",
			err:  os.ErrNotExist,
			want: false,
		},
		{
			name: "method not found is not a missing resource",
			err:  libacp.MethodNotFound("fs/read_text_file"),
			want: false,
		},
		{
			// Protocol-level codes must not trigger the message check.
			name: "invalid params saying not found is not a missing resource",
			err:  libacp.InvalidParams("session not found"),
			want: false,
		},
		{
			name: "auth required saying not found is not a missing resource",
			err:  libacp.NewError(libacp.ErrAuthRequired, "credentials not found"),
			want: false,
		},
		{
			name: "typed nil pointer in interface",
			err:  (*libacp.Error)(nil),
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := libacp.IsNotFound(tt.err); got != tt.want {
				t.Errorf("IsNotFound(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

func TestAsNotExist(t *testing.T) {
	notFound := libacp.NewError(libacp.ErrResourceNotFound, "no such resource")
	other := libacp.NewError(libacp.ErrInvalidParams, "bad path")
	raw := errors.New("agent not found: contenox-acp")

	tests := []struct {
		name        string
		err         error
		wantNotExit bool
		wantSame    bool
	}{
		{name: "nil passes through", err: nil, wantSame: true},
		{name: "typed not-found becomes os.ErrNotExist", err: notFound, wantNotExit: true},
		{name: "other typed error is returned unchanged", err: other, wantSame: true},
		{name: "raw not found text is returned unchanged", err: raw, wantSame: true},
		{
			name:     "startup error is returned unchanged",
			err:      libacp.ErrAgentStartFailed,
			wantSame: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := libacp.AsNotExist(tt.err)
			if tt.wantSame && got != tt.err {
				t.Fatalf("AsNotExist(%v) = %v, want identical error", tt.err, got)
			}
			if isNotExist := errors.Is(got, os.ErrNotExist); isNotExist != tt.wantNotExit {
				t.Fatalf("errors.Is(%v, os.ErrNotExist) = %v, want %v", got, isNotExist, tt.wantNotExit)
			}
		})
	}
}

// TestAsNotExistPreservesDetail pins that the original protocol error detail
// is still recoverable after mapping to os.ErrNotExist.
func TestAsNotExistPreservesDetail(t *testing.T) {
	orig := libacp.NewError(libacp.ErrResourceNotFound, "no such resource")
	got := libacp.AsNotExist(orig)
	if !errors.Is(got, os.ErrNotExist) {
		t.Fatalf("want os.ErrNotExist, got %v", got)
	}
	if want := orig.Error(); !strings.Contains(got.Error(), want) {
		t.Fatalf("mapped error %q lost original detail %q", got.Error(), want)
	}
}
