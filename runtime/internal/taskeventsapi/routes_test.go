package taskeventsapi

import (
	"bufio"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	libauth "github.com/contenox/contenox/libauth"
	libbus "github.com/contenox/contenox/libbus"
	"github.com/contenox/contenox/runtime/taskengine"
	"github.com/stretchr/testify/require"
)

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

func TestAddRoutes_StreamsRequestScopedTaskEvents(t *testing.T) {
	bus := libbus.NewInMem()
	mux := http.NewServeMux()
	AddRoutes(mux, bus, fakeAuthReader{})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/task-events?requestId=req-1", nil)
	require.NoError(t, err)

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	sink := taskengine.NewBusTaskEventSink(bus)
	require.NoError(t, sink.PublishTaskEvent(context.Background(), taskengine.TaskEvent{
		Kind:      taskengine.TaskEventStepChunk,
		RequestID: "req-2",
		Content:   "ignored",
	}))
	require.NoError(t, sink.PublishTaskEvent(context.Background(), taskengine.TaskEvent{
		Kind:      taskengine.TaskEventStepChunk,
		RequestID: "req-1",
		Content:   "hello",
	}))

	reader := bufio.NewReader(resp.Body)
	line, err := reader.ReadString('\n')
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(line, "data: "))
	require.Contains(t, line, `"request_id":"req-1"`)
	require.Contains(t, line, `"content":"hello"`)
	require.NotContains(t, line, `"request_id":"req-2"`)
}

func TestAddRoutes_RequiresIdentity(t *testing.T) {
	bus := libbus.NewInMem()
	mux := http.NewServeMux()
	AddRoutes(mux, bus, fakeAuthReader{err: errors.New("unauthorized")})

	req := httptest.NewRequest(http.MethodGet, "/task-events?requestId=req-1", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	require.NotEqual(t, http.StatusOK, rec.Code)
}
