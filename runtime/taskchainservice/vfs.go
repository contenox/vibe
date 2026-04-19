package taskchainservice

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	libdb "github.com/contenox/contenox/libdbexec"
	"github.com/contenox/contenox/runtime/taskengine"
	"github.com/contenox/contenox/runtime/vfsservice"
)

// vfsStore persists task chains as JSON files via vfsservice.Service (same storage as /api/files).
type vfsStore struct {
	vfs vfsservice.Service
}

// NewVFS returns a Service backed by the given VFS. vfs must be the same instance used for file APIs.
func NewVFS(vfs vfsservice.Service) Service {
	if vfs == nil {
		return nil
	}
	return &vfsStore{vfs: vfs}
}

// NormalizeVFSPath returns a clean relative path or an error if it escapes the root.
func NormalizeVFSPath(p string) (string, error) {
	p = strings.TrimSpace(p)
	p = filepath.ToSlash(p)
	p = strings.TrimPrefix(p, "/")
	if p == "" {
		return "", fmt.Errorf("path is required")
	}
	if strings.Contains(p, "..") {
		return "", fmt.Errorf("invalid path")
	}
	if strings.Contains(p, "\x00") {
		return "", fmt.Errorf("invalid path")
	}
	return p, nil
}

func splitParentAndName(rel string) (parentID, name string) {
	rel = strings.Trim(rel, "/")
	if rel == "" {
		return "", ""
	}
	i := strings.LastIndex(rel, "/")
	if i < 0 {
		return "", rel
	}
	return rel[:i], rel[i+1:]
}

func validateChain(chain *taskengine.TaskChainDefinition) error {
	if chain == nil {
		return fmt.Errorf("task chain is required")
	}
	if chain.ID == "" {
		return fmt.Errorf("task chain ID is required")
	}
	if len(chain.Tasks) == 0 {
		return fmt.Errorf("task chain must contain at least one task")
	}
	return nil
}

func parseChainJSON(data []byte) (*taskengine.TaskChainDefinition, error) {
	var chain taskengine.TaskChainDefinition
	if err := json.Unmarshal(data, &chain); err != nil {
		return nil, err
	}
	return &chain, nil
}

func isJSONName(name string) bool {
	return strings.EqualFold(filepath.Ext(name), ".json")
}

func (s *vfsStore) listRootJSON(ctx context.Context) ([]vfsservice.File, error) {
	files, err := s.vfs.GetFilesByPath(ctx, "")
	if err != nil {
		return nil, err
	}
	var out []vfsservice.File
	for _, f := range files {
		if isJSONName(f.Name) {
			out = append(out, f)
		}
	}
	return out, nil
}

func (s *vfsStore) loadFileFull(ctx context.Context, fileID string) (*taskengine.TaskChainDefinition, error) {
	f, err := s.vfs.GetFileByID(ctx, fileID)
	if err != nil {
		return nil, err
	}
	chain, err := parseChainJSON(f.Data)
	if err != nil {
		return nil, fmt.Errorf("parse chain json: %w", err)
	}
	if chain.ID == "" || len(chain.Tasks) == 0 {
		return nil, fmt.Errorf("not a valid task chain document")
	}
	return chain, nil
}

// Get loads a chain by VFS path (e.g. chain-foo.json) or by logical id (scans root *.json for matching chain.id).
func (s *vfsStore) Get(ctx context.Context, ref string) (*taskengine.TaskChainDefinition, error) {
	if ref == "" {
		return nil, fmt.Errorf("task chain reference is required")
	}
	if norm, err := NormalizeVFSPath(ref); err == nil {
		if chain, err := s.loadFileFull(ctx, norm); err == nil {
			return chain, nil
		}
	}
	files, err := s.listRootJSON(ctx)
	if err != nil {
		return nil, fmt.Errorf("list chain files: %w", err)
	}
	for _, meta := range files {
		chain, err := s.loadFileFull(ctx, meta.ID)
		if err != nil {
			continue
		}
		if chain.ID == ref {
			return chain, nil
		}
	}
	return nil, fmt.Errorf("task chain %q: %w", ref, libdb.ErrNotFound)
}

// List returns the relative paths of all *.json files in the chain VFS root.
func (s *vfsStore) List(ctx context.Context) ([]string, error) {
	files, err := s.listRootJSON(ctx)
	if err != nil {
		return nil, err
	}
	paths := make([]string, 0, len(files))
	for _, f := range files {
		paths = append(paths, f.Path)
	}
	return paths, nil
}

// CreateAtPath writes a new JSON file at the given VFS path. Fails if the file already exists.
func (s *vfsStore) CreateAtPath(ctx context.Context, path string, chain *taskengine.TaskChainDefinition) error {
	if err := validateChain(chain); err != nil {
		return err
	}
	rel, err := NormalizeVFSPath(path)
	if err != nil {
		return err
	}
	parentID, name := splitParentAndName(rel)
	if name == "" {
		return fmt.Errorf("path must include a file name")
	}
	if !isJSONName(name) {
		return fmt.Errorf("chain file must have .json extension")
	}
	existing, gerr := s.vfs.GetFileByID(ctx, rel)
	if gerr == nil && existing != nil && len(existing.Data) > 0 {
		return fmt.Errorf("task chain file already exists: %s", rel)
	}
	data, err := json.MarshalIndent(chain, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal chain: %w", err)
	}
	_, err = s.vfs.CreateFile(ctx, &vfsservice.File{
		Name:        name,
		ParentID:    parentID,
		Data:        data,
		ContentType: "application/json",
	})
	if err != nil {
		return fmt.Errorf("create chain file: %w", err)
	}
	return nil
}

// UpdateAtPath replaces the file at path with chain JSON.
func (s *vfsStore) UpdateAtPath(ctx context.Context, path string, chain *taskengine.TaskChainDefinition) error {
	if err := validateChain(chain); err != nil {
		return err
	}
	rel, err := NormalizeVFSPath(path)
	if err != nil {
		return err
	}
	prev, err := s.vfs.GetFileByID(ctx, rel)
	if err != nil || prev == nil || len(prev.Data) == 0 {
		return fmt.Errorf("task chain file not found: %w", libdb.ErrNotFound)
	}
	data, err := json.MarshalIndent(chain, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal chain: %w", err)
	}
	_, err = s.vfs.UpdateFile(ctx, &vfsservice.File{
		ID:          rel,
		Data:        data,
		ContentType: "application/json",
	})
	if err != nil {
		return fmt.Errorf("update chain file: %w", err)
	}
	return nil
}

// DeleteByPath removes the chain file at path.
func (s *vfsStore) DeleteByPath(ctx context.Context, path string) error {
	rel, err := NormalizeVFSPath(path)
	if err != nil {
		return err
	}
	if err := s.vfs.DeleteFile(ctx, rel); err != nil {
		return fmt.Errorf("delete chain file: %w", err)
	}
	return nil
}
