// Package shellenvservice persists the operator-defined environment variables
// contenox injects into shells it spawns, layered on top of the environment
// scrub so they always win. Values are plaintext in the kv table, so no secrets
// belong here.
package shellenvservice

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/contenox/contenox/internal/store/runtimetypes"
	libdb "github.com/contenox/contenox/libdbexec"
)

const globalKVKey = "shellenv.global"

// ErrInvalidName is returned when a variable name is not a usable environment
// variable name.
var ErrInvalidName = errors.New("shellenvservice: invalid environment variable name")

// Service reads and writes the global shell-env variable map.
type Service interface {
	// Get returns the global variables — an empty (never nil) map when none are
	// set, so a caller can range over it unconditionally.
	Get(ctx context.Context) (map[string]string, error)
	// Set replaces the global variables with vars after validating every name. An
	// empty map clears them.
	Set(ctx context.Context, vars map[string]string) error
}

// New builds the service over db. It opens a fresh non-transactional store per
// call, matching the runtime's other kv-backed services.
func New(db libdb.DBManager) Service { return &service{db: db} }

type service struct{ db libdb.DBManager }

func (s *service) Get(ctx context.Context) (map[string]string, error) {
	store := runtimetypes.New(s.db.WithoutTransaction())
	var vars map[string]string
	if err := store.GetKV(ctx, globalKVKey, &vars); err != nil {
		if errors.Is(err, libdb.ErrNotFound) {
			return map[string]string{}, nil // unset is an empty set, not an error
		}
		return nil, fmt.Errorf("shellenvservice: read global env: %w", err)
	}
	if vars == nil {
		vars = map[string]string{}
	}
	return vars, nil
}

func (s *service) Set(ctx context.Context, vars map[string]string) error {
	clean := make(map[string]string, len(vars))
	for name, value := range vars {
		if !ValidEnvName(name) {
			return fmt.Errorf("%w: %q (use letters, digits and underscores; not starting with a digit)", ErrInvalidName, name)
		}
		clean[name] = value
	}
	b, err := json.Marshal(clean)
	if err != nil {
		return fmt.Errorf("shellenvservice: marshal: %w", err)
	}
	store := runtimetypes.New(s.db.WithoutTransaction())
	if err := store.SetKV(ctx, globalKVKey, b); err != nil {
		return fmt.Errorf("shellenvservice: write global env: %w", err)
	}
	return nil
}

// ValidEnvName reports whether name is a usable environment-variable name: a
// non-empty run of ASCII letters, digits and underscores, not starting with a digit.
func ValidEnvName(name string) bool {
	if name == "" {
		return false
	}
	for i, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r == '_':
		case r >= '0' && r <= '9':
			if i == 0 {
				return false
			}
		default:
			return false
		}
	}
	return true
}
