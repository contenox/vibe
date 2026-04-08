package vfsservice

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	libdb "github.com/contenox/contenox/libdbexec"
)

// localFS implements Service against the local filesystem under a root directory.
// Operations that have no meaningful local-FS equivalent return ErrNotSupported.
type localFS struct {
	root string
}

// NewLocalFS returns a Service backed by the local filesystem rooted at root.
// Intended for the CLI and TUI.
func NewLocalFS(root string) Service {
	return &localFS{root: filepath.Clean(root)}
}

var _ Service = (*localFS)(nil)

// abs resolves path under l.root and rejects traversals that escape the root
// (Fix 1: path traversal vulnerability via ../.. sequences).
func (l *localFS) abs(path string) (string, error) {
	target := filepath.Join(l.root, filepath.FromSlash(path))
	// After Join+Clean, verify the result is still inside root.
	if target != l.root && !strings.HasPrefix(target, l.root+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q escapes root directory", path)
	}
	return target, nil
}

func (l *localFS) CreateFile(ctx context.Context, file *File) (*File, error) {
	if file.Name == "" {
		return nil, fmt.Errorf("name is required")
	}
	// Fix 2: check actual data length, not caller-supplied Size field.
	if int64(len(file.Data)) > MaxUploadSize {
		return nil, fmt.Errorf("file size exceeds maximum allowed size")
	}
	dir := l.root
	if file.ParentID != "" {
		p, err := l.abs(file.ParentID)
		if err != nil {
			return nil, err
		}
		dir = p
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir: %w", err)
	}
	dest := filepath.Join(dir, file.Name)
	if err := os.WriteFile(dest, file.Data, 0o644); err != nil {
		return nil, fmt.Errorf("write file: %w", err)
	}
	now := time.Now().UTC()
	rel, _ := filepath.Rel(l.root, dest)
	return &File{
		ID:          rel,
		Path:        filepath.ToSlash(rel),
		Name:        file.Name,
		ParentID:    file.ParentID,
		Size:        int64(len(file.Data)),
		ContentType: file.ContentType,
		Data:        file.Data,
		CreatedAt:   now,
		UpdatedAt:   now,
	}, nil
}

func (l *localFS) GetFileByID(ctx context.Context, id string) (*File, error) {
	path, err := l.abs(id)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, fmt.Errorf("file not found: %w", libdb.ErrNotFound)
	}
	if err != nil {
		return nil, err
	}
	info, _ := os.Stat(path)
	name := filepath.Base(path)
	rel, _ := filepath.Rel(l.root, path)
	return &File{
		ID:        rel,
		Path:      filepath.ToSlash(rel),
		Name:      name,
		Size:      info.Size(),
		Data:      data,
		UpdatedAt: info.ModTime().UTC(),
	}, nil
}

func (l *localFS) GetFolderByID(ctx context.Context, id string) (*Folder, error) {
	path, err := l.abs(id)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		return nil, fmt.Errorf("folder not found: %w", libdb.ErrNotFound)
	}
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("not a directory")
	}
	rel, _ := filepath.Rel(l.root, path)
	return &Folder{
		ID:   rel,
		Path: filepath.ToSlash(rel),
		Name: filepath.Base(path),
	}, nil
}

func (l *localFS) GetFilesByPath(ctx context.Context, path string) ([]File, error) {
	// Fix 7: treat "/" the same as root.
	if path == "/" {
		path = ""
	}
	dir, err := l.abs(path)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var files []File
	for _, e := range entries {
		rel, _ := filepath.Rel(l.root, filepath.Join(dir, e.Name()))
		info, _ := e.Info()
		f := File{
			ID:          rel,
			Path:        filepath.ToSlash(rel),
			Name:        e.Name(),
			IsDirectory: e.IsDir(),
		}
		if info != nil {
			f.Size = info.Size()
			f.UpdatedAt = info.ModTime().UTC()
		}
		files = append(files, f)
	}
	return files, nil
}

func (l *localFS) UpdateFile(ctx context.Context, file *File) (*File, error) {
	// Fix 2: check actual data length.
	if int64(len(file.Data)) > MaxUploadSize {
		return nil, fmt.Errorf("file size exceeds maximum allowed size")
	}
	path, err := l.abs(file.ID)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, file.Data, 0o644); err != nil {
		return nil, fmt.Errorf("write file: %w", err)
	}
	now := time.Now().UTC()
	rel, _ := filepath.Rel(l.root, path)
	return &File{
		ID:          rel,
		Path:        filepath.ToSlash(rel),
		Name:        filepath.Base(path),
		Size:        int64(len(file.Data)),
		ContentType: file.ContentType,
		Data:        file.Data,
		UpdatedAt:   now,
	}, nil
}

func (l *localFS) DeleteFile(ctx context.Context, id string) error {
	path, err := l.abs(id)
	if err != nil {
		return err
	}
	return os.Remove(path)
}

func (l *localFS) CreateFolder(ctx context.Context, parentID, name string) (*Folder, error) {
	base, err := l.abs(parentID)
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(base, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	rel, _ := filepath.Rel(l.root, dir)
	return &Folder{
		ID:       rel,
		Path:     filepath.ToSlash(rel),
		Name:     name,
		ParentID: parentID,
	}, nil
}

func (l *localFS) RenameFile(ctx context.Context, fileID, newName string) (*File, error) {
	// Fix 8: block slashes in new names.
	if strings.Contains(newName, "/") {
		return nil, fmt.Errorf("name cannot contain slashes")
	}
	src, err := l.abs(fileID)
	if err != nil {
		return nil, err
	}
	dst := filepath.Join(filepath.Dir(src), newName)
	if err := os.Rename(src, dst); err != nil {
		return nil, err
	}
	rel, _ := filepath.Rel(l.root, dst)
	return &File{
		ID:   rel,
		Path: filepath.ToSlash(rel),
		Name: newName,
	}, nil
}

func (l *localFS) RenameFolder(ctx context.Context, folderID, newName string) (*Folder, error) {
	// Fix 8: block slashes in new names.
	if strings.Contains(newName, "/") {
		return nil, fmt.Errorf("name cannot contain slashes")
	}
	src, err := l.abs(folderID)
	if err != nil {
		return nil, err
	}
	dir := filepath.Dir(src)
	dst := filepath.Join(dir, newName)
	if err := os.Rename(src, dst); err != nil {
		return nil, err
	}
	rel, _ := filepath.Rel(l.root, dst)
	parentRel, _ := filepath.Rel(l.root, dir)
	// Fix 10: filepath.Rel returns "." for the root directory; normalise to "".
	if parentRel == "." {
		parentRel = ""
	}
	return &Folder{
		ID:       rel,
		Path:     filepath.ToSlash(rel),
		Name:     newName,
		ParentID: filepath.ToSlash(parentRel),
	}, nil
}

func (l *localFS) DeleteFolder(ctx context.Context, folderID string) error {
	path, err := l.abs(folderID)
	if err != nil {
		return err
	}
	return os.RemoveAll(path)
}

func (l *localFS) MoveFile(ctx context.Context, fileID, newParentID string) (*File, error) {
	src, err := l.abs(fileID)
	if err != nil {
		return nil, err
	}
	dstDir, err := l.abs(newParentID)
	if err != nil {
		return nil, err
	}
	name := filepath.Base(src)
	if err := os.MkdirAll(dstDir, 0o755); err != nil {
		return nil, err
	}
	dst := filepath.Join(dstDir, name)
	if err := os.Rename(src, dst); err != nil {
		return nil, err
	}
	rel, _ := filepath.Rel(l.root, dst)
	return &File{
		ID:       rel,
		Path:     filepath.ToSlash(rel),
		Name:     name,
		ParentID: newParentID,
	}, nil
}

func (l *localFS) MoveFolder(ctx context.Context, folderID, newParentID string) (*Folder, error) {
	src, err := l.abs(folderID)
	if err != nil {
		return nil, err
	}
	dstDir, err := l.abs(newParentID)
	if err != nil {
		return nil, err
	}
	name := filepath.Base(src)
	if err := os.MkdirAll(dstDir, 0o755); err != nil {
		return nil, err
	}
	dst := filepath.Join(dstDir, name)
	if err := os.Rename(src, dst); err != nil {
		return nil, err
	}
	rel, _ := filepath.Rel(l.root, dst)
	return &Folder{
		ID:       rel,
		Path:     filepath.ToSlash(rel),
		Name:     name,
		ParentID: newParentID,
	}, nil
}
