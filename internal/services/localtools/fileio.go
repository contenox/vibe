package localtools

import (
	"context"
	"errors"
)

type FileIO interface {
	ReadFile(ctx context.Context, path string) ([]byte, error)
	WriteFile(ctx context.Context, path string, data []byte) error
}

var ErrNoFilesystem = errors.New("no filesystem: the connected client provides none and contenox does not read the host")

type noFilesystemIO struct{}

func (noFilesystemIO) ReadFile(context.Context, string) ([]byte, error) { return nil, ErrNoFilesystem }

func (noFilesystemIO) WriteFile(context.Context, string, []byte) error { return ErrNoFilesystem }
