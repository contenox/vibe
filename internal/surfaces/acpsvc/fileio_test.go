package acpsvc

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"

	"github.com/contenox/contenox/internal/services/localtools"
	libacp "github.com/contenox/contenox/libacp"
	"github.com/stretchr/testify/require"
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
func TestUnit_ACPFileIO_RefusesWhenNoClientIsAttached(t *testing.T) {
	fio := NewACPFileIO(func(context.Context) *Transport { return nil })
	_, err := fio.ReadFile(context.Background(), "whatever.txt")
	require.ErrorIs(t, err, localtools.ErrNoFilesystem)
	require.ErrorIs(t, fio.WriteFile(context.Background(), "whatever.txt", []byte("x")), localtools.ErrNoFilesystem)
}

func TestUnit_ACPFileIO_RefusesWhenNilResolver(t *testing.T) {
	fio := NewACPFileIO(nil)
	_, err := fio.ReadFile(context.Background(), "whatever.txt")
	require.ErrorIs(t, err, localtools.ErrNoFilesystem)
}

func TestUnit_ACPFileIO_RefusesWhenClientLacksFSCapability(t *testing.T) {
	tr := mockTransportForFS(libacp.FileSystemCapabilities{})
	fio := NewACPFileIO(func(context.Context) *Transport { return tr })
	_, err := fio.ReadFile(context.Background(), "whatever.txt")
	require.ErrorIs(t, err, localtools.ErrNoFilesystem)
}
