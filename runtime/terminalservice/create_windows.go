//go:build windows

package terminalservice

import "context"

func (s *service) Create(ctx context.Context, _ string, _ CreateRequest) (*CreateResponse, error) {
	return nil, ErrNotImplemented
}
