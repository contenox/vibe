//go:build !windows

package terminalservice

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/store/runtimetypes"
	libdb "github.com/contenox/contenox/libdbexec"
	"github.com/contenox/contenox/libtracker"
	"github.com/stretchr/testify/require"
)

func setupTerminalService(t *testing.T, maxSessions int) (context.Context, Service) {
	t.Helper()
	ctx := context.Background()
	db, err := libdb.NewSQLiteDBManager(ctx, filepath.Join(t.TempDir(), "terminal.db"), runtimetypes.SchemaSQLite)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })

	root := t.TempDir()
	cfg := Config{
		Enabled:      true,
		AllowedRoot:  root,
		DefaultShell: "/bin/sh",
		MaxSessions:  maxSessions,
	}
	svc, err := New(cfg, db, "node-test", "ws-test")
	require.NoError(t, err)
	return ctx, svc
}

func TestCreate_MultipleSessions(t *testing.T) {
	ctx, svc := setupTerminalService(t, 0)
	principal := "local-user"

	first, err := svc.Create(ctx, principal, CreateRequest{CWD: ""})
	require.NoError(t, err)
	require.NotEmpty(t, first.ID)

	second, err := svc.Create(ctx, principal, CreateRequest{CWD: ""})
	require.NoError(t, err)
	require.NotEmpty(t, second.ID)
	require.NotEqual(t, first.ID, second.ID)

	list, err := svc.List(ctx, principal, nil, 10)
	require.NoError(t, err)
	require.Len(t, list, 2)
}

func TestCreate_MaxSessionsCap(t *testing.T) {
	ctx, svc := setupTerminalService(t, 2)
	principal := "local-user"

	_, err := svc.Create(ctx, principal, CreateRequest{CWD: ""})
	require.NoError(t, err)
	_, err = svc.Create(ctx, principal, CreateRequest{CWD: ""})
	require.NoError(t, err)
	_, err = svc.Create(ctx, principal, CreateRequest{CWD: ""})
	require.ErrorIs(t, err, ErrTooManySessions)
}

func TestCloseOneLeavesOther(t *testing.T) {
	ctx, svc := setupTerminalService(t, 0)
	principal := "local-user"

	first, err := svc.Create(ctx, principal, CreateRequest{CWD: ""})
	require.NoError(t, err)
	second, err := svc.Create(ctx, principal, CreateRequest{CWD: ""})
	require.NoError(t, err)

	require.NoError(t, svc.Close(ctx, principal, first.ID))

	got, err := svc.Get(ctx, principal, second.ID)
	require.NoError(t, err)
	require.Equal(t, second.ID, got.ID)

	_, err = svc.Get(ctx, principal, first.ID)
	require.ErrorIs(t, err, ErrSessionNotFound)
}

func TestAttach_SecondConnectionPreemptsFirst(t *testing.T) {
	ctx, svc := setupTerminalService(t, 0)
	principal := "local-user"

	out, err := svc.Create(ctx, principal, CreateRequest{CWD: ""})
	require.NoError(t, err)

	firstExited := make(chan struct{})
	firstConn, firstPeer := net.Pipe()
	go func() {
		defer close(firstExited)
		defer firstConn.Close()
		defer firstPeer.Close()
		_ = svc.Attach(context.Background(), principal, out.ID, firstConn, nil)
	}()

	time.Sleep(50 * time.Millisecond)

	secondConn, secondPeer := net.Pipe()
	secondDone := make(chan error, 1)
	go func() {
		defer secondPeer.Close()
		secondDone <- svc.Attach(context.Background(), principal, out.ID, secondConn, nil)
	}()

	select {
	case err := <-secondDone:
		require.NoError(t, err)
	case <-time.After(500 * time.Millisecond):
		require.NoError(t, secondConn.Close())
		require.NoError(t, <-secondDone)
	}

	select {
	case <-firstExited:
	case <-time.After(2 * time.Second):
		t.Fatal("first attach did not exit after preempt")
	}
}

// recordingTracker records every Start and every error reported through it;
// concurrency-safe since the attach pumps report from their own goroutines.
type recordingTracker struct {
	mu     sync.Mutex
	events []trackedEvent
}

type trackedEvent struct {
	op, subject string
	kv          []any
	err         error
}

func (r *recordingTracker) Start(_ context.Context, op, subject string, kv ...any) (func(error), func(string, any), func()) {
	kvCopy := append([]any(nil), kv...)
	r.mu.Lock()
	r.events = append(r.events, trackedEvent{op: op, subject: subject, kv: kvCopy})
	r.mu.Unlock()
	return func(err error) {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.events = append(r.events, trackedEvent{op: op, subject: subject, kv: kvCopy, err: err})
		},
		func(string, any) {},
		func() {}
}

func (r *recordingTracker) matching(op, subject string, withErr bool) []trackedEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []trackedEvent
	for _, ev := range r.events {
		if ev.op == op && ev.subject == subject && (ev.err != nil) == withErr {
			out = append(out, ev)
		}
	}
	return out
}

func kvOf(ev trackedEvent, key string) any {
	for i := 0; i+1 < len(ev.kv); i += 2 {
		if k, ok := ev.kv[i].(string); ok && k == key {
			return ev.kv[i+1]
		}
	}
	return nil
}

func setupTrackedTerminalService(t *testing.T, tracker libtracker.ActivityTracker) (context.Context, Service) {
	t.Helper()
	ctx := context.Background()
	db, err := libdb.NewSQLiteDBManager(ctx, filepath.Join(t.TempDir(), "terminal.db"), runtimetypes.SchemaSQLite)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })

	cfg := Config{
		Enabled:      true,
		AllowedRoot:  t.TempDir(),
		DefaultShell: "/bin/sh",
	}
	svc, err := New(cfg, db, "node-test", "ws-test", WithTracker(tracker))
	require.NoError(t, err)
	return ctx, svc
}

// TestResizeFailureIsReportedToTracker pins that a pty resize failure behind a successful geometry write still surfaces, only through the tracker.
func TestResizeFailureIsReportedToTracker(t *testing.T) {
	tracker := &recordingTracker{}
	ctx, svc := setupTrackedTerminalService(t, tracker)
	principal := "local-user"

	out, err := svc.Create(ctx, principal, CreateRequest{CWD: ""})
	require.NoError(t, err)

	// Swap the session's pty for a closed descriptor: the store update still
	// succeeds, the ioctl behind it cannot.
	sess := svc.(*service).localByID(out.ID)
	require.NotNil(t, sess)
	realTTY := sess.tty
	t.Cleanup(func() { _ = realTTY.Close() })
	closedFD, err := os.Open(os.DevNull)
	require.NoError(t, err)
	require.NoError(t, closedFD.Close())
	sess.tty = closedFD

	require.NoError(t, svc.UpdateGeometry(ctx, principal, out.ID, 100, 40),
		"a failed pty resize must not fail the geometry update — the row is the durable fact")

	reported := tracker.matching("resize", "terminal_pty", true)
	require.Len(t, reported, 1, "the failed pty resize is reported exactly once")
	require.Contains(t, reported[0].err.Error(), "resize")
	require.Equal(t, out.ID, kvOf(reported[0], "session"))
	require.Equal(t, "pty", kvOf(reported[0], "backend"))
}

// TestAttachStreamsAreReportedToTracker pins that a pump's byte count and stop reason are only observable through the tracker.
func TestAttachStreamsAreReportedToTracker(t *testing.T) {
	tracker := &recordingTracker{}
	ctx, svc := setupTrackedTerminalService(t, tracker)
	principal := "local-user"

	out, err := svc.Create(ctx, principal, CreateRequest{CWD: ""})
	require.NoError(t, err)

	conn, peer := net.Pipe()
	done := make(chan error, 1)
	go func() { done <- svc.Attach(context.Background(), principal, out.ID, conn, nil) }()

	time.Sleep(50 * time.Millisecond)
	require.NoError(t, peer.Close()) // the client goes away: the ws->pty pump ends

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("attach did not return after the client went away")
	}

	// The pumps report from their own goroutines, which Attach does not join (it
	// joins only the pty reader), so poll rather than assume the report has landed
	// by the time Attach returns.
	var starts []trackedEvent
	require.Eventually(t, func() bool {
		starts = tracker.matching("stream_input", "terminal_pty", false)
		return len(starts) > 0
	}, 2*time.Second, 5*time.Millisecond, "the ws->pty pump must report how it ended")
	require.Equal(t, out.ID, kvOf(starts[0], "sessionID"))
	require.Equal(t, "pty", kvOf(starts[0], "backend"))
	require.NotNil(t, kvOf(starts[0], "bytes"), "the byte count the log line carried must survive")

	_ = svc.Close(ctx, principal, out.ID)
}

func TestReapIdle_OnlyDetached(t *testing.T) {
	ctx, svc := setupTerminalService(t, 0)
	principal := "local-user"

	impl := svc.(*service)
	impl.cfg.IdleTimeout = 10 * time.Millisecond

	out, err := svc.Create(ctx, principal, CreateRequest{CWD: ""})
	require.NoError(t, err)

	sess := impl.localByID(out.ID)
	require.NotNil(t, sess)
	sess.lastActivityNanos.Store(time.Now().Add(-time.Minute).UnixNano())

	require.NoError(t, svc.ReapIdle(ctx))

	_, err = svc.Get(ctx, principal, out.ID)
	require.ErrorIs(t, err, ErrSessionNotFound)
}
