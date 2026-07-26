package localtools

import (
	"context"
	"os"
)

type FileIO interface {
	ReadFile(ctx context.Context, path string) ([]byte, error)
	WriteFile(ctx context.Context, path string, data []byte) error
}

func NewOSFileIO() FileIO {
	return osFileIO{}
}

type osFileIO struct{}

func (osFileIO) ReadFile(_ context.Context, path string) ([]byte, error) {
	return os.ReadFile(path)
}

func (osFileIO) WriteFile(_ context.Context, path string, data []byte) error {
	return os.WriteFile(path, data, 0644)
}
