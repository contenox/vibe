package runtimetypes_test

import (
	"testing"

	"github.com/contenox/contenox/internal/store/runtimetypes"
	libdb "github.com/contenox/contenox/libdbexec"
)

func mustFiringStore(t *testing.T, exec libdb.Exec, workspaceID string, opts ...runtimetypes.EventFiringStoreOption) runtimetypes.EventFiringStore {
	t.Helper()
	s, err := runtimetypes.NewEventFiringStore(exec, workspaceID, opts...)
	if err != nil {
		t.Fatalf("firing store: %v", err)
	}
	return s
}
