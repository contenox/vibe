//go:build !windows

package terminalservice

import (
	"context"
	"net"
	"path/filepath"
	"testing"
	"time"

	libdb "github.com/contenox/beam/internal/libdbexec"
	"github.com/contenox/beam/internal/store/runtimetypes"
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
