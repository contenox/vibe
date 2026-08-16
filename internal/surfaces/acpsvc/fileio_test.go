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

// A host serves sessions no client is attached to — work driven over the relay
// before a transport binds, or a chain fired by a trigger with nobody watching.
// Such a call has no editor to proxy through, and must read the disk rather
// than fail: refusing here made file tools unusable outside an editor.
func TestUnit_ACPFileIO_FallsBackToOSWhenNoClientIsAttached(t *testing.T) {
	t.Parallel()
	io := NewACPFileIO(func(context.Context) *Transport { return nil })
	ctx := context.Background()

	p := filepath.Join(t.TempDir(), "unattached.txt")
	if err := io.WriteFile(ctx, p, []byte("written with no client")); err != nil {
		t.Fatalf("WriteFile with no attached transport must fall back to os, got %v", err)
	}
	got, err := io.ReadFile(ctx, p)
	if err != nil {
		t.Fatalf("ReadFile with no attached transport must fall back to os, got %v", err)
	}
	if string(got) != "written with no client" {
		t.Fatalf("os fallback round-trip mismatch: %q", got)
	}
}

// An unset resolver is the same situation as an unattached one, and must not
// panic on the way to the same answer.
func TestUnit_ACPFileIO_NilResolverFallsBackToOS(t *testing.T) {
	t.Parallel()
	io := NewACPFileIO(nil)
	ctx := context.Background()

	p := filepath.Join(t.TempDir(), "nil-resolver.txt")
	if err := io.WriteFile(ctx, p, []byte("still works")); err != nil {
		t.Fatalf("WriteFile with a nil resolver must fall back to os, got %v", err)
	}
	if got, err := io.ReadFile(ctx, p); err != nil || string(got) != "still works" {
		t.Fatalf("ReadFile with a nil resolver = %q, %v", got, err)
	}
}

func TestUnit_ACPFileIO_FallsBackToOSWhenClientLacksFSCapability(t *testing.T) {
	t.Parallel()
	tr := mockTransportForFS(libacp.FileSystemCapabilities{})
	io := NewACPFileIO(func(context.Context) *Transport { return tr })
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
