package taskchainservice

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/contenox/contenox/internal/services/localfileservice"
	libdb "github.com/contenox/contenox/libdbexec"
)

type Service interface {
	Get(ctx context.Context, ref string) (*taskengine.TaskChainDefinition, error)
	List(ctx context.Context) ([]string, error)
	CreateAtPath(ctx context.Context, path string, chain *taskengine.TaskChainDefinition) error
	UpdateAtPath(ctx context.Context, path string, chain *taskengine.TaskChainDefinition) error
	DeleteByPath(ctx context.Context, path string) error
}

type localStore struct {
	files localfileservice.Service
}

func NewLocal(files localfileservice.Service) Service {
	if files == nil {
		return nil
	}
	return &localStore{files: files}
}

func NormalizePath(p string) (string, error) {
	rel, err := localfileservice.NormalizeRelPath(p, false)
	if err != nil {
		return "", err
	}
	if !strings.EqualFold(filepath.Ext(rel), ".json") {
		return "", fmt.Errorf("chain file must have .json extension")
	}
	return rel, nil
}

func validateChain(chain *taskengine.TaskChainDefinition) error {
	if chain == nil {
		return fmt.Errorf("task chain is required")
	}
	if strings.TrimSpace(chain.ID) == "" {
		return fmt.Errorf("task chain ID is required")
	}
	if len(chain.Tasks) == 0 {
		return fmt.Errorf("task chain must contain at least one task")
	}
	// The full load-time linter gates both writes and reads, so a chain that
	// cannot execute is refused rather than failing mid-run.
	return taskengine.LintChain(chain)
}

func (s *localStore) Get(ctx context.Context, ref string) (*taskengine.TaskChainDefinition, error) {
	if strings.TrimSpace(ref) == "" {
		return nil, fmt.Errorf("task chain reference is required")
	}
	if path, err := NormalizePath(ref); err == nil {
		if chain, err := s.loadPath(ctx, path); err == nil {
			if lintErr := taskengine.LintChain(chain); lintErr != nil {
				return nil, disabledChainError(path, lintErr)
			}
			return chain, nil
		}
	}
	paths, err := s.List(ctx)
	if err != nil {
		return nil, err
	}
	for _, path := range paths {
		chain, err := s.loadPath(ctx, path)
		if err != nil {
			continue
		}
		if chain.ID == ref {
			// The id resolves to this file, so a lint failure surfaces as "found but
			// refused" rather than rolling on to NotFound.
			if lintErr := taskengine.LintChain(chain); lintErr != nil {
				return nil, disabledChainError(path, lintErr)
			}
			return chain, nil
		}
	}
	return nil, fmt.Errorf("task chain %q: %w", ref, libdb.ErrNotFound)
}

func disabledChainError(path string, lintErr error) error {
	return fmt.Errorf("task chain file %q is disabled until it is fixed: %w", path, lintErr)
}

func (s *localStore) List(ctx context.Context) ([]string, error) {
	entries, err := s.files.List(ctx, ".")
	if err != nil {
		return nil, err
	}
	paths := []string{}
	for _, entry := range entries {
		if entry.IsDirectory || !strings.EqualFold(filepath.Ext(entry.Path), ".json") {
			continue
		}
		chain, err := s.loadPath(ctx, entry.Path)
		if err != nil || chain.ID == "" || len(chain.Tasks) == 0 {
			continue
		}
		paths = append(paths, entry.Path)
	}
	return paths, nil
}

func (s *localStore) CreateAtPath(ctx context.Context, path string, chain *taskengine.TaskChainDefinition) error {
	if err := validateChain(chain); err != nil {
		return err
	}
	path, err := NormalizePath(path)
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(chain, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal chain: %w", err)
	}
	if _, err := s.files.Write(ctx, path, data, true); err != nil {
		return fmt.Errorf("create chain file: %w", err)
	}
	return nil
}

func (s *localStore) UpdateAtPath(ctx context.Context, path string, chain *taskengine.TaskChainDefinition) error {
	if err := validateChain(chain); err != nil {
		return err
	}
	path, err := NormalizePath(path)
	if err != nil {
		return err
	}
	if _, err := s.files.Stat(ctx, path); err != nil {
		return fmt.Errorf("task chain file not found: %w", err)
	}
	data, err := json.MarshalIndent(chain, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal chain: %w", err)
	}
	if _, err := s.files.Write(ctx, path, data, false); err != nil {
		return fmt.Errorf("update chain file: %w", err)
	}
	return nil
}

func (s *localStore) DeleteByPath(ctx context.Context, path string) error {
	path, err := NormalizePath(path)
	if err != nil {
		return err
	}
	if err := s.files.Delete(ctx, path); err != nil {
		return fmt.Errorf("delete chain file: %w", err)
	}
	return nil
}

func (s *localStore) loadPath(ctx context.Context, path string) (*taskengine.TaskChainDefinition, error) {
	data, _, err := s.files.Read(ctx, path)
	if err != nil {
		return nil, err
	}
	var chain taskengine.TaskChainDefinition
	if err := json.Unmarshal(data, &chain); err != nil {
		return nil, fmt.Errorf("parse chain json: %w", err)
	}
	return &chain, nil
}
