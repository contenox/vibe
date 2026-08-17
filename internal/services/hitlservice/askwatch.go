package hitlservice

import (
	"context"
	"strings"

	"github.com/contenox/contenox/internal/store/runtimetypes"
)

type AskResolution string

const (
	AskAnswered   AskResolution = "answered"
	AskExpired    AskResolution = "expired"
	AskSuperseded AskResolution = "superseded"
)

type AskWatcher interface {
	AskRecorded(ctx context.Context, row *runtimetypes.HITLApproval)
	AskResolved(ctx context.Context, askID string, reason AskResolution)
}

func SetAskWatcher(svc Service, w AskWatcher) {
	if s, ok := svc.(*service); ok {
		s.mu.Lock()
		s.askWatcher = w
		s.mu.Unlock()
	}
}

func (s *service) watcher() AskWatcher {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.askWatcher
}

func (s *service) askRecorded(ctx context.Context, row *runtimetypes.HITLApproval) {
	if row == nil || strings.TrimSpace(row.ID) == "" {
		return
	}
	if w := s.watcher(); w != nil {
		w.AskRecorded(ctx, row)
	}
}
