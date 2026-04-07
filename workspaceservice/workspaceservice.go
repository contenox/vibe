// Package workspaceservice manages user-owned directory bindings (workspaces) under a server security ceiling.
package workspaceservice

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	apiframework "github.com/contenox/contenox/apiframework"
	libdb "github.com/contenox/contenox/libdbexec"
	"github.com/contenox/contenox/terminalservice"
	"github.com/contenox/contenox/workspacestore"
	"github.com/google/uuid"
)

var ErrNotFound = workspacestore.ErrNotFound

// WorkspaceDTO is returned to API clients (includes vfs-relative path for VFS listing).
type WorkspaceDTO struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Path      string    `json:"path"`
	VfsPath   string    `json:"vfsPath"`
	Shell     string    `json:"shell,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

type CreateInput struct {
	Name  string
	Path  string
	Shell string
}

type UpdateInput struct {
	Name  string
	Path  string
	Shell string
}

// Service is workspace business logic.
type Service interface {
	Create(ctx context.Context, principal string, in CreateInput) (*WorkspaceDTO, error)
	Get(ctx context.Context, principal, id string) (*WorkspaceDTO, error)
	List(ctx context.Context, principal string, cursor *time.Time, limit int) ([]*WorkspaceDTO, error)
	Update(ctx context.Context, principal, id string, in UpdateInput) (*WorkspaceDTO, error)
	Delete(ctx context.Context, principal, id string) error
}

type service struct {
	db          libdb.DBManager
	allowedRoot string
	vfsRoot     string
}

// New constructs a Service. allowedRoot is required (security ceiling). vfsRoot may be empty to skip VFS alignment checks.
func New(db libdb.DBManager, allowedRoot, vfsRoot string) Service {
	return &service{db: db, allowedRoot: strings.TrimSpace(allowedRoot), vfsRoot: strings.TrimSpace(vfsRoot)}
}

func (s *service) store() workspacestore.Store {
	return workspacestore.New(s.db.WithoutTransaction())
}

func (s *service) validatePath(absPath string) error {
	if err := terminalservice.CwdUnderRoot(s.allowedRoot, absPath); err != nil {
		return apiframework.BadRequest(err.Error())
	}
	if s.vfsRoot != "" {
		if err := terminalservice.CwdUnderRoot(s.vfsRoot, absPath); err != nil {
			return apiframework.BadRequest("path must be under the VFS root")
		}
	}
	return nil
}

func (s *service) toDTO(w *workspacestore.Workspace) (*WorkspaceDTO, error) {
	vfsPath, err := vfsRelPath(w.Path, s.vfsRoot)
	if err != nil {
		return nil, apiframework.BadRequest("path is outside VFS root")
	}
	return &WorkspaceDTO{
		ID: w.ID, Name: w.Name, Path: w.Path, VfsPath: vfsPath, Shell: w.Shell,
		CreatedAt: w.CreatedAt, UpdatedAt: w.UpdatedAt,
	}, nil
}

func vfsRelPath(absWorkspace, vfsRoot string) (string, error) {
	if vfsRoot == "" {
		return "", nil
	}
	root, err := filepath.Abs(filepath.Clean(vfsRoot))
	if err != nil {
		return "", err
	}
	abs, err := filepath.Abs(filepath.Clean(absWorkspace))
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(root, abs)
	if err != nil {
		return "", err
	}
	if rel == "." {
		return "", nil
	}
	if strings.HasPrefix(rel, "..") {
		return "", fmt.Errorf("outside vfs root")
	}
	return filepath.ToSlash(rel), nil
}

func normalizeAbsPath(p string) (string, error) {
	p = strings.TrimSpace(p)
	if p == "" {
		return "", apiframework.BadRequest("path is required")
	}
	abs, err := filepath.Abs(filepath.Clean(p))
	if err != nil {
		return "", apiframework.BadRequest("invalid path")
	}
	return abs, nil
}

func (s *service) Create(ctx context.Context, principal string, in CreateInput) (*WorkspaceDTO, error) {
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return nil, apiframework.BadRequest("name is required")
	}
	abs, err := normalizeAbsPath(in.Path)
	if err != nil {
		return nil, err
	}
	if err := s.validatePath(abs); err != nil {
		return nil, err
	}
	shell := strings.TrimSpace(in.Shell)
	if shell != "" {
		if err := terminalservice.ValidateShell(shell); err != nil {
			return nil, apiframework.BadRequest(err.Error())
		}
	}
	w := &workspacestore.Workspace{
		ID: uuid.NewString(), Principal: principal, Name: name, Path: abs, Shell: shell,
	}
	if err := s.store().Insert(ctx, w); err != nil {
		if isUniqueViolation(err) {
			return nil, apiframework.BadRequest("a workspace with this name already exists")
		}
		return nil, err
	}
	return s.toDTO(w)
}

func (s *service) Get(ctx context.Context, principal, id string) (*WorkspaceDTO, error) {
	w, err := s.store().GetByIDAndPrincipal(ctx, id, principal)
	if err != nil {
		if errors.Is(err, workspacestore.ErrNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return s.toDTO(w)
}

func (s *service) List(ctx context.Context, principal string, cursor *time.Time, limit int) ([]*WorkspaceDTO, error) {
	list, err := s.store().ListByPrincipal(ctx, principal, cursor, limit)
	if err != nil {
		return nil, err
	}
	out := make([]*WorkspaceDTO, 0, len(list))
	for _, w := range list {
		dto, err := s.toDTO(w)
		if err != nil {
			return nil, err
		}
		out = append(out, dto)
	}
	return out, nil
}

func (s *service) Update(ctx context.Context, principal, id string, in UpdateInput) (*WorkspaceDTO, error) {
	st := s.store()
	w, err := st.GetByIDAndPrincipal(ctx, id, principal)
	if err != nil {
		if errors.Is(err, workspacestore.ErrNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return nil, apiframework.BadRequest("name is required")
	}
	abs, err := normalizeAbsPath(in.Path)
	if err != nil {
		return nil, err
	}
	if err := s.validatePath(abs); err != nil {
		return nil, err
	}
	shell := strings.TrimSpace(in.Shell)
	if shell != "" {
		if err := terminalservice.ValidateShell(shell); err != nil {
			return nil, apiframework.BadRequest(err.Error())
		}
	}
	w.Name = name
	w.Path = abs
	w.Shell = shell
	if err := st.Update(ctx, w); err != nil {
		if errors.Is(err, workspacestore.ErrNotFound) {
			return nil, ErrNotFound
		}
		if isUniqueViolation(err) {
			return nil, apiframework.BadRequest("a workspace with this name already exists")
		}
		return nil, err
	}
	return s.toDTO(w)
}

func (s *service) Delete(ctx context.Context, principal, id string) error {
	err := s.store().DeleteByIDAndPrincipal(ctx, id, principal)
	if errors.Is(err, workspacestore.ErrNotFound) {
		return ErrNotFound
	}
	return err
}

func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "unique") || strings.Contains(msg, "duplicate")
}
