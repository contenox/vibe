package llamasession

import (
	"errors"
	"fmt"
	"testing"

	"github.com/contenox/contenox/internal/modeld/llama"
)

func closeLifecycleForTest(s *sessionLifecycle, calls *int) func() {
	return func() {
		*calls = *calls + 1
		s.close()
	}
}

func TestSessionLifecycleMarkFatalClosesAndReports(t *testing.T) {
	var s sessionLifecycle
	closeCalls := 0
	err := s.markFatal(errors.New("boom"), closeLifecycleForTest(&s, &closeCalls))
	if !errors.Is(err, llama.ErrSessionFatal) {
		t.Fatalf("markFatal error = %v, want ErrSessionFatal", err)
	}
	if closeCalls != 1 || !s.closed {
		t.Fatalf("markFatal closeCalls=%d closed=%v, want one close and closed", closeCalls, s.closed)
	}
	if s.fatalErr != "boom" {
		t.Fatalf("fatalErr = %q, want recorded cause", s.fatalErr)
	}
	if s.residencyErr != "session fatal: boom" {
		t.Fatalf("residencyErr = %q, want fatal residency error", s.residencyErr)
	}
	if err := s.closedErr(); !errors.Is(err, llama.ErrSessionFatal) {
		t.Fatalf("closedErr = %v, want ErrSessionFatal", err)
	}
}

func TestSessionLifecyclePlainCloseIsNotFatal(t *testing.T) {
	var s sessionLifecycle
	if !s.close() {
		t.Fatal("first close returned false")
	}
	if s.close() {
		t.Fatal("second close returned true")
	}
	err := s.closedErr()
	if !errors.Is(err, llama.ErrSessionClosed) {
		t.Fatalf("closedErr = %v, want ErrSessionClosed", err)
	}
	if errors.Is(err, llama.ErrSessionFatal) {
		t.Fatal("plain close must not classify as fatal")
	}
	if s.fatalErr != "" || s.residencyErr != "" {
		t.Fatalf("plain close fatalErr=%q residencyErr=%q, want empty", s.fatalErr, s.residencyErr)
	}
}

func TestSessionLifecycleMarkFatalKeepsFirstCause(t *testing.T) {
	var s sessionLifecycle
	closeCalls := 0
	_ = s.markFatal(errors.New("first"), closeLifecycleForTest(&s, &closeCalls))
	_ = s.markFatal(errors.New("second"), closeLifecycleForTest(&s, &closeCalls))
	if s.fatalErr != "first" {
		t.Fatalf("fatalErr = %q, want first recorded cause", s.fatalErr)
	}
	if s.residencyErr != "session fatal: first" {
		t.Fatalf("residencyErr = %q, want first recorded cause", s.residencyErr)
	}
}

func TestSessionLifecycleFatalizeRoutesOnlyFatalErrors(t *testing.T) {
	var s sessionLifecycle
	closeCalls := 0
	plain := errors.New("transient")
	if err := s.fatalize(plain, closeLifecycleForTest(&s, &closeCalls)); err != plain {
		t.Fatalf("fatalize(plain) = %v, want pass-through", err)
	}
	if s.closed || s.fatalErr != "" || closeCalls != 0 {
		t.Fatalf("fatalize poisoned on non-fatal error: closed=%v fatalErr=%q closeCalls=%d", s.closed, s.fatalErr, closeCalls)
	}
	wrapped := fmt.Errorf("%w: kv remove failed", llama.ErrSessionFatal)
	if err := s.fatalize(wrapped, closeLifecycleForTest(&s, &closeCalls)); !errors.Is(err, llama.ErrSessionFatal) {
		t.Fatalf("fatalize(fatal) = %v, want ErrSessionFatal", err)
	}
	if !s.closed || s.fatalErr == "" || closeCalls != 1 {
		t.Fatalf("fatalize did not poison on fatal error: closed=%v fatalErr=%q closeCalls=%d", s.closed, s.fatalErr, closeCalls)
	}
}
