package approvalapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/contenox/contenox/runtime/hitlservice"
	libauth "github.com/contenox/contenox/libauth"
	"github.com/contenox/contenox/libtracker"
	"github.com/contenox/contenox/runtime/taskengine"
	"github.com/contenox/contenox/runtime/vfsservice"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeAuthReader stubs middleware.AuthZReader.
type fakeAuthReader struct{ err error }
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

// fakeHITLService stubs hitlservice.Service for handler tests.
type fakeHITLService struct {
	respondOK bool
}

func (f *fakeHITLService) Evaluate(_ context.Context, _, _ string, _ map[string]any) (hitlservice.EvaluationResult, error) {
	return hitlservice.EvaluationResult{Action: hitlservice.ActionAllow, Reason: hitlservice.ReasonMatchedRule}, nil
}
func (f *fakeHITLService) RequestApproval(_ context.Context, _ hitlservice.ApprovalRequest, _ taskengine.TaskEventSink) (bool, error) {
	return false, nil
}
func (f *fakeHITLService) Respond(_ string, _ bool) bool {
	return f.respondOK
}

func newTestServer(t *testing.T, svc hitlservice.Service, authErr error) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	AddRoutes(mux, svc, fakeAuthReader{err: authErr})
	return httptest.NewServer(mux)
}

func postApproval(t *testing.T, srv *httptest.Server, approvalID string, approved bool) *http.Response {
	t.Helper()
	body, err := json.Marshal(respondBody{Approved: approved})
	require.NoError(t, err)
	resp, err := http.Post(srv.URL+"/approvals/"+approvalID, "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	return resp
}

func TestRespond_ReturnsNoContent_WhenApprovalFound(t *testing.T) {
	svc := &fakeHITLService{respondOK: true}
	srv := newTestServer(t, svc, nil)
	defer srv.Close()

	resp := postApproval(t, srv, "some-approval-id", true)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusNoContent, resp.StatusCode)
}

func TestRespond_ReturnsNotFound_WhenApprovalMissing(t *testing.T) {
	svc := &fakeHITLService{respondOK: false}
	srv := newTestServer(t, svc, nil)
	defer srv.Close()

	resp := postApproval(t, srv, "nonexistent-id", false)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestRespond_ReturnsUnauthorized_WhenAuthFails(t *testing.T) {
	svc := &fakeHITLService{respondOK: true}
	srv := newTestServer(t, svc, errors.New("unauthorized"))
	defer srv.Close()

	resp := postApproval(t, srv, "some-id", true)
	defer resp.Body.Close()
	assert.NotEqual(t, http.StatusNoContent, resp.StatusCode)
}

// nopKVReader satisfies hitlservice.KVReader returning not-found for any key.
type nopKVReader struct{}

func (nopKVReader) GetKV(_ context.Context, _ string, _ interface{}) error {
	return errors.New("not found")
}

func TestRespond_RealService_UnknownID_ReturnsNotFound(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	vfs := vfsservice.NewLocalFS(dir)
	svc := hitlservice.New(vfs, nopKVReader{}, libtracker.NoopTracker{})

	mux := http.NewServeMux()
	AddRoutes(mux, svc, fakeAuthReader{})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp := postApproval(t, srv, "completely-unknown-id", true)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}
