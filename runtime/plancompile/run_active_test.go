package plancompile

import (
	"context"
	"testing"
)

func TestRunActiveCompiled_nilDeps(t *testing.T) {
	_, err := RunActiveCompiled(context.Background(), nil, nil, nil, "x", "y", "z", "", nil, nil)
	if err == nil {
		t.Fatal("expected error")
	}
}
