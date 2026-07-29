package llamasession

import (
	"errors"
	"fmt"

	"github.com/contenox/contenox/internal/modeld/llama"
)

type sessionLifecycle struct {
	closed       bool
	fatalErr     string
	residencyErr string
}

func (s *sessionLifecycle) closedErr() error {
	if s.fatalErr != "" {
		return fmt.Errorf("%w: %s", llama.ErrSessionFatal, s.fatalErr)
	}
	return llama.ErrSessionClosed
}

func (s *sessionLifecycle) markFatal(err error, close func()) error {
	if err == nil {
		err = llama.ErrSessionFatal
	}
	if s.fatalErr == "" {
		s.fatalErr = err.Error()
	}
	if close != nil {
		close()
	} else {
		s.close()
	}
	s.residencyErr = "session fatal: " + s.fatalErr
	if errors.Is(err, llama.ErrSessionFatal) {
		return err
	}
	return fmt.Errorf("%w: %v", llama.ErrSessionFatal, err)
}

func (s *sessionLifecycle) fatalize(err error, close func()) error {
	if err == nil || !errors.Is(err, llama.ErrSessionFatal) {
		return err
	}
	return s.markFatal(err, close)
}

func (s *sessionLifecycle) close() bool {
	if s.closed {
		return false
	}
	s.closed = true
	return true
}
