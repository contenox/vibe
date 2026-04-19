package vfsstore

import (
	"context"
	"database/sql"
	"fmt"

	libdb "github.com/contenox/contenox/libdbexec"
)

// Store defines all persistence operations for the VFS.
type Store interface {
	// FileTree operations
	ListFileIDsByParentID(ctx context.Context, parentID string) ([]string, error)
	// ListChildrenByParentID returns name + metadata for all children in one JOIN query.
	ListChildrenByParentID(ctx context.Context, parentID string) ([]ChildEntry, error)
	ListFileIDsByName(ctx context.Context, parentID, name string) ([]string, error)
	GetFileParentID(ctx context.Context, id string) (string, error)
	GetFileNameByID(ctx context.Context, id string) (string, error)
	CreateFileNameID(ctx context.Context, id, parentID, name string) error
	DeleteFileNameID(ctx context.Context, id string) error
	UpdateFileNameByID(ctx context.Context, id, name string) error
	UpdateFileParentID(ctx context.Context, id, newParentID string) error

	// File operations
	CreateFile(ctx context.Context, file *File) error
	GetFileByID(ctx context.Context, id string) (*File, error)
	UpdateFile(ctx context.Context, file *File) error
	DeleteFile(ctx context.Context, id string) error
	ListFiles(ctx context.Context) ([]string, error)
	EstimateFileCount(ctx context.Context) (int64, error)
	EnforceMaxFileCount(ctx context.Context, maxCount int64) error

	// Blob operations
	CreateBlob(ctx context.Context, blob *Blob) error
	GetBlobByID(ctx context.Context, id string) (*Blob, error)
	DeleteBlob(ctx context.Context, id string) error
	// UpdateBlob updates blob data and meta in-place without delete+insert churn.
	UpdateBlob(ctx context.Context, id string, data, meta []byte) error
}

type store struct {
	Exec libdb.Exec
}

// New creates a new VFS store instance.
func New(exec libdb.Exec) Store {
	return &store{Exec: exec}
}

func checkRowsAffected(result sql.Result) error {
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}
	if rows == 0 {
		return libdb.ErrNotFound
	}
	return nil
}
