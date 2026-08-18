package hitlservice

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// PolicySource reads a named HITL policy document for a tenant; any error
// (including not-found) makes the evaluator fall back to the built-in default
// policy.
type PolicySource interface {
	ReadPolicy(ctx context.Context, tenantID, name string) ([]byte, error)
}

type fsPolicySource struct{ dirs []string }

// NewFSPolicySource returns a PolicySource that looks up "<dir>/<name>" in
// each dir in order, returning the first hit; tenantID is ignored and empty
// dirs are skipped.
func NewFSPolicySource(dirs ...string) PolicySource {
	return &fsPolicySource{dirs: dirs}
}

func (f *fsPolicySource) ReadPolicy(_ context.Context, _, name string) ([]byte, error) {
	file := policyFileName(name)
	var lastErr error = os.ErrNotExist
	for _, dir := range f.dirs {
		if dir == "" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, file))
		if err == nil {
			return data, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		lastErr = err
	}
	return nil, fmt.Errorf("hitl policy %q not found: %w", file, lastErr)
}

// policyFileName is idempotent: a bare "strict" becomes "hitl-policy-strict.json";
// a value already ending in .json or carrying a path separator is left verbatim.
func policyFileName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" || strings.HasSuffix(name, ".json") || strings.ContainsAny(name, `/\`) {
		return name
	}
	return "hitl-policy-" + name + ".json"
}
