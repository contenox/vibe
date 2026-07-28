package acpsvc

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	libacp "github.com/contenox/contenox/libacp"
)

func TestUnit_MapACPNotExist_WrapsResourceNotFoundAsErrNotExist(t *testing.T) {
	t.Parallel()

	rpcNotFound := &libacp.Error{Code: libacp.ErrResourceNotFound, Message: "Resource not found"}
	if got := mapACPNotExist(rpcNotFound); !errors.Is(got, os.ErrNotExist) {
		t.Fatalf("rpc -32002 must satisfy os.ErrNotExist (fs.go new-file-write depends on it), got %v", got)
	}

	wrapped := fmt.Errorf("acp read failed: %w", &libacp.Error{Code: libacp.ErrResourceNotFound, Message: "nope"})
	if got := mapACPNotExist(wrapped); !errors.Is(got, os.ErrNotExist) {
		t.Fatalf("wrapped -32002 must still satisfy os.ErrNotExist, got %v", got)
	}

	byMessage := errors.New("ENOENT: file not found")
	if got := mapACPNotExist(byMessage); !errors.Is(got, os.ErrNotExist) {
		t.Fatalf("not-found message must satisfy os.ErrNotExist, got %v", got)
	}

	internal := &libacp.Error{Code: libacp.ErrInternalError, Message: "boom"}
	got := mapACPNotExist(internal)
	if errors.Is(got, os.ErrNotExist) {
		t.Fatalf("internal error must NOT be coerced to os.ErrNotExist, got %v", got)
	}
	if got != internal {
		t.Fatalf("non-not-found error must pass through unchanged, got %v", got)
	}

	generic := errors.New("connection reset")
	if got := mapACPNotExist(generic); errors.Is(got, os.ErrNotExist) || got != generic {
		t.Fatalf("generic error must pass through unchanged and not be os.ErrNotExist, got %v", got)
	}

	if mapACPNotExist(nil) != nil {
		t.Fatalf("nil must map to nil")
	}
}

func TestUnit_ACPFileIO_FallsBackToOSWhenClientLacksFSCapability(t *testing.T) {
	t.Parallel()
	tr := mockTransportForFS(libacp.FileSystemCapabilities{})
	io := NewACPFileIO(func() *Transport { return tr })
	ctx := context.Background()

	p := filepath.Join(t.TempDir(), "note.txt")

	if err := io.WriteFile(ctx, p, []byte("hello from os")); err != nil {
		t.Fatalf("WriteFile must fall back to os when client lacks fs.writeTextFile, got %v", err)
	}
	got, err := io.ReadFile(ctx, p)
	if err != nil {
		t.Fatalf("ReadFile must fall back to os when client lacks fs.readTextFile, got %v", err)
	}
	if string(got) != "hello from os" {
		t.Fatalf("os fallback round-trip mismatch: %q", got)
	}
}
