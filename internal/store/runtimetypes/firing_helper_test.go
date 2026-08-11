package runtimetypes_test

import (
	"testing"

	"github.com/contenox/contenox/internal/store/runtimetypes"
	libdb "github.com/contenox/contenox/libdbexec"
)

// mustFiringStore builds the firing store fixtures use, failing the test on a
// constructor error instead of threading one through every call site.
func mustFiringStore(t *testing.T, exec libdb.Exec, workspaceID string, opts ...runtimetypes.EventFiringStoreOption) runtimetypes.EventFiringStore {
	t.Helper()
	s, err := runtimetypes.NewEventFiringStore(exec, workspaceID, opts...)
	if err != nil {
		t.Fatalf("firing store: %v", err)
	}
	return s
}
