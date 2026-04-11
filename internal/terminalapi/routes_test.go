package terminalapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	libauth "github.com/contenox/contenox/libauth"
	libdb "github.com/contenox/contenox/libdbexec"
	"github.com/contenox/contenox/runtimetypes"
	"github.com/contenox/contenox/terminalservice"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/websocket"
)

func testTerminalDB(t *testing.T) libdb.DBManager {
	t.Helper()
	ctx := context.Background()
	db, err := libdb.NewSQLiteDBManager(ctx, filepath.Join(t.TempDir(), "terminal.sqlite"), runtimetypes.SchemaSQLite)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

type fakeAuthReader struct {
	err error
}

type fakePermissions struct{}

func (fakePermissions) RequireAuthorisation(string, int) (bool, error) { return true, nil }

func (f fakeAuthReader) GetIdentity(context.Context) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	return "user-1", nil
}

func (fakeAuthReader) GetUsername(context.Context) (string, error) { return "user", nil }
func (fakeAuthReader) GetPermissions(context.Context) (libauth.Authz, error) {
	return fakePermissions{}, nil
}
func (fakeAuthReader) GetTokenString(context.Context) (string, error) { return "token", nil }
func (fakeAuthReader) GetExpiresAt(context.Context) (time.Time, error) {
	return time.Now().Add(time.Hour), nil
}

func TestAddRoutes_CreateRequiresAuth(t *testing.T) {
	root := t.TempDir()
	cfg, err := terminalservice.ParseEnv("true", root, "/bin/bash", "")
	require.NoError(t, err)
	svc, err := terminalservice.New(cfg, testTerminalDB(t), "test-node")
	require.NoError(t, err)

	mux := http.NewServeMux()
	AddRoutes(mux, svc, fakeAuthReader{err: errors.New("nope")}, true)

	req := httptest.NewRequest(http.MethodPost, "/terminal/sessions", strings.NewReader(`{"cwd":"`+root+`"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	require.NotEqual(t, http.StatusCreated, rec.Code)
}

func TestAddRoutes_CreateBadCwd(t *testing.T) {
	root := t.TempDir()
	cfg, err := terminalservice.ParseEnv("true", root, "/bin/bash", "")
	require.NoError(t, err)
	svc, err := terminalservice.New(cfg, testTerminalDB(t), "test-node")
	require.NoError(t, err)

	mux := http.NewServeMux()
	AddRoutes(mux, svc, fakeAuthReader{}, true)

	outside := filepath.Join(t.TempDir(), "escape")
	req := httptest.NewRequest(http.MethodPost, "/terminal/sessions", strings.NewReader(`{"cwd":"`+outside+`"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	require.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestAddRoutes_ListSessionsEmpty(t *testing.T) {
	root := t.TempDir()
	cfg, err := terminalservice.ParseEnv("true", root, "/bin/bash", "")
	require.NoError(t, err)
	svc, err := terminalservice.New(cfg, testTerminalDB(t), "test-node")
	require.NoError(t, err)

	mux := http.NewServeMux()
	AddRoutes(mux, svc, fakeAuthReader{}, true)

	req := httptest.NewRequest(http.MethodGet, "/terminal/sessions", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "[]\n", rec.Body.String())
}

func TestAddRoutes_DeleteNotFound(t *testing.T) {
	root := t.TempDir()
	cfg, err := terminalservice.ParseEnv("true", root, "/bin/bash", "")
	require.NoError(t, err)
	svc, err := terminalservice.New(cfg, testTerminalDB(t), "test-node")
	require.NoError(t, err)

	mux := http.NewServeMux()
	AddRoutes(mux, svc, fakeAuthReader{}, true)

	req := httptest.NewRequest(http.MethodDelete, "/terminal/sessions/does-not-exist", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	require.Equal(t, http.StatusNotFound, rec.Code)
}

func TestWebSocket_DataFlow(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PTY not implemented on Windows")
	}
	if _, err := os.Stat("/bin/bash"); err != nil {
		t.Skip("no /bin/bash")
	}

	root := t.TempDir()
	cfg, err := terminalservice.ParseEnv("true", root, "/bin/bash", "")
	require.NoError(t, err)
	svc, err := terminalservice.New(cfg, testTerminalDB(t), "test-node")
	require.NoError(t, err)

	mux := http.NewServeMux()
	AddRoutes(mux, svc, fakeAuthReader{}, true)

	srv := httptest.NewServer(mux)
	defer srv.Close()

	// Create session
	createReq, err := http.NewRequest(http.MethodPost, srv.URL+"/terminal/sessions",
		strings.NewReader(`{"cwd":"`+root+`","cols":80,"rows":24}`))
	require.NoError(t, err)
	createReq.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(createReq)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusCreated, resp.StatusCode)

	var out struct {
		ID     string `json:"id"`
		WSPath string `json:"wsPath"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))

	// Connect WebSocket
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + out.WSPath
	t.Logf("WS URL: %s", wsURL)

	// Verify the WS endpoint is reachable (should get upgrade-required or similar, not 404)
	probeResp, probeErr := http.Get(srv.URL + out.WSPath)
	if probeErr == nil {
		t.Logf("Probe: status=%d", probeResp.StatusCode)
		probeResp.Body.Close()
	}

	ws, err := websocket.Dial(wsURL, "", srv.URL)
	require.NoError(t, err)
	defer ws.Close()

	// Wait for shell to initialize, then send a command
	time.Sleep(300 * time.Millisecond)
	_, err = ws.Write([]byte("echo WSTEST_OK\n"))
	require.NoError(t, err)

	// Read output — should contain our echo
	buf := make([]byte, 4096)
	received := ""
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		_ = ws.SetReadDeadline(time.Now().Add(2 * time.Second))
		n, err := ws.Read(buf)
		if n > 0 {
			chunk := string(buf[:n])
			t.Logf("WS read: %d bytes: %q", n, chunk)
			received += chunk
		}
		if strings.Contains(received, "WSTEST_OK") {
			break
		}
		if err != nil {
			t.Logf("WS read error: %v", err)
			break
		}
	}

	t.Logf("Total received: %q", received)
	require.Contains(t, received, "WSTEST_OK", "Expected echo output in WebSocket data")
}

// readUntil drains the websocket until needle is observed or deadline elapses.
func readUntil(t *testing.T, ws *websocket.Conn, needle string, deadline time.Time) string {
	t.Helper()
	buf := make([]byte, 4096)
	var received string
	for time.Now().Before(deadline) {
		_ = ws.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		n, err := ws.Read(buf)
		if n > 0 {
			received += string(buf[:n])
		}
		if strings.Contains(received, needle) {
			return received
		}
		if err != nil {
			continue
		}
	}
	return received
}

// TestWebSocket_ReattachAfterDisconnect verifies that closing a websocket
// detaches without destroying the session and that a second attach observes
// the shell state set by the first.
func TestWebSocket_ReattachAfterDisconnect(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PTY not implemented on Windows")
	}
	if _, err := os.Stat("/bin/bash"); err != nil {
		t.Skip("no /bin/bash")
	}

	root := t.TempDir()
	cfg, err := terminalservice.ParseEnv("true", root, "/bin/bash", "")
	require.NoError(t, err)
	svc, err := terminalservice.New(cfg, testTerminalDB(t), "test-node")
	require.NoError(t, err)
	defer func() { _ = svc.CloseAll(context.Background()) }()

	mux := http.NewServeMux()
	AddRoutes(mux, svc, fakeAuthReader{}, true)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// Create a session.
	createReq, err := http.NewRequest(http.MethodPost, srv.URL+"/terminal/sessions",
		strings.NewReader(`{"cwd":"`+root+`","cols":80,"rows":24}`))
	require.NoError(t, err)
	createReq.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(createReq)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusCreated, resp.StatusCode)

	var out struct {
		ID     string `json:"id"`
		WSPath string `json:"wsPath"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + out.WSPath

	ws1, err := websocket.Dial(wsURL, "", srv.URL)
	require.NoError(t, err)
	time.Sleep(300 * time.Millisecond)
	_, err = ws1.Write([]byte("REATTACH_MARKER=hello_world\n"))
	require.NoError(t, err)
	_, err = ws1.Write([]byte("echo first_attach_$REATTACH_MARKER\n"))
	require.NoError(t, err)
	got := readUntil(t, ws1, "first_attach_hello_world", time.Now().Add(5*time.Second))
	require.Contains(t, got, "first_attach_hello_world")

	require.NoError(t, ws1.Close())
	// Wait for the attach goroutines to release the busy flag.
	time.Sleep(150 * time.Millisecond)

	getResp, err := http.Get(srv.URL + "/terminal/sessions/" + out.ID)
	require.NoError(t, err)
	getResp.Body.Close()
	require.Equal(t, http.StatusOK, getResp.StatusCode, "session row was deleted on detach")

	ws2, err := websocket.Dial(wsURL, "", srv.URL)
	require.NoError(t, err)
	defer ws2.Close()
	time.Sleep(300 * time.Millisecond)
	_, err = ws2.Write([]byte("echo second_attach_$REATTACH_MARKER\n"))
	require.NoError(t, err)
	got = readUntil(t, ws2, "second_attach_hello_world", time.Now().Add(5*time.Second))
	require.Contains(t, got, "second_attach_hello_world",
		"shell state was lost between attaches; session was destroyed on disconnect")
}

// TestReapIdle_ClosesDetachedSessions verifies that ReapIdle closes a session
// whose last activity is older than the configured idle timeout.
func TestReapIdle_ClosesDetachedSessions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PTY not implemented on Windows")
	}
	if _, err := os.Stat("/bin/bash"); err != nil {
		t.Skip("no /bin/bash")
	}

	root := t.TempDir()
	cfg, err := terminalservice.ParseEnv("true", root, "/bin/bash", "1ns")
	require.NoError(t, err)
	svc, err := terminalservice.New(cfg, testTerminalDB(t), "test-node")
	require.NoError(t, err)
	defer func() { _ = svc.CloseAll(context.Background()) }()

	ctx := context.Background()
	const principal = "user-1"

	created, err := svc.Create(ctx, principal, terminalservice.CreateRequest{CWD: root, Cols: 80, Rows: 24})
	require.NoError(t, err)
	time.Sleep(5 * time.Millisecond)
	require.NoError(t, svc.ReapIdle(ctx))

	_, err = svc.Get(ctx, principal, created.ID)
	require.ErrorIs(t, err, terminalservice.ErrSessionNotFound)
}

// TestSingleSlot_SecondCreateRejected verifies that the per-process service
// hosts one session at a time and that closing the first frees the slot.
func TestSingleSlot_SecondCreateRejected(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PTY not implemented on Windows")
	}
	if _, err := os.Stat("/bin/bash"); err != nil {
		t.Skip("no /bin/bash")
	}

	root := t.TempDir()
	cfg, err := terminalservice.ParseEnv("true", root, "/bin/bash", "0")
	require.NoError(t, err)
	svc, err := terminalservice.New(cfg, testTerminalDB(t), "test-node")
	require.NoError(t, err)
	defer func() { _ = svc.CloseAll(context.Background()) }()

	ctx := context.Background()
	const principal = "user-1"

	first, err := svc.Create(ctx, principal, terminalservice.CreateRequest{CWD: root, Cols: 80, Rows: 24})
	require.NoError(t, err)

	_, err = svc.Create(ctx, principal, terminalservice.CreateRequest{CWD: root, Cols: 80, Rows: 24})
	require.ErrorIs(t, err, terminalservice.ErrTooManySessions)

	_, err = svc.Create(ctx, "user-2", terminalservice.CreateRequest{CWD: root, Cols: 80, Rows: 24})
	require.ErrorIs(t, err, terminalservice.ErrTooManySessions)

	require.NoError(t, svc.Close(ctx, principal, first.ID))
	second, err := svc.Create(ctx, principal, terminalservice.CreateRequest{CWD: root, Cols: 80, Rows: 24})
	require.NoError(t, err)
	require.NotEqual(t, first.ID, second.ID)
}

// TestReapIdle_ZeroTimeoutDisabled verifies that a zero IdleTimeout disables
// reaping.
func TestReapIdle_ZeroTimeoutDisabled(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PTY not implemented on Windows")
	}
	if _, err := os.Stat("/bin/bash"); err != nil {
		t.Skip("no /bin/bash")
	}

	root := t.TempDir()
	cfg, err := terminalservice.ParseEnv("true", root, "/bin/bash", "0")
	require.NoError(t, err)
	require.Equal(t, time.Duration(0), cfg.IdleTimeout)
	svc, err := terminalservice.New(cfg, testTerminalDB(t), "test-node")
	require.NoError(t, err)
	defer func() { _ = svc.CloseAll(context.Background()) }()

	ctx := context.Background()
	const principal = "user-1"
	created, err := svc.Create(ctx, principal, terminalservice.CreateRequest{CWD: root, Cols: 80, Rows: 24})
	require.NoError(t, err)
	time.Sleep(10 * time.Millisecond)
	require.NoError(t, svc.ReapIdle(ctx))

	got, err := svc.Get(ctx, principal, created.ID)
	require.NoError(t, err)
	require.Equal(t, created.ID, got.ID)
}
