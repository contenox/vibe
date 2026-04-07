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
	cfg, err := terminalservice.ParseEnv("true", root, "4", "/bin/bash")
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
	cfg, err := terminalservice.ParseEnv("true", root, "4", "/bin/bash")
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
	cfg, err := terminalservice.ParseEnv("true", root, "4", "/bin/bash")
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
	cfg, err := terminalservice.ParseEnv("true", root, "4", "/bin/bash")
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
	cfg, err := terminalservice.ParseEnv("true", root, "4", "/bin/bash")
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
