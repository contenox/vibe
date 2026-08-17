package contenoxcli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/relaycreds"
	"github.com/contenox/contenox/internal/services/hitlservice"
	"github.com/contenox/contenox/internal/surfaces/acpsvc"
	"github.com/contenox/contenox/libacp"
	"github.com/contenox/contenox/libtracker"
)

func relayTestFactory(*libacp.AgentSideConnection) libacp.Agent { return libacp.UnimplementedAgent{} }

func writePairing(t *testing.T, dir string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, relaycreds.Filename), []byte("{not json"), 0o600); err != nil {
		t.Fatalf("write credentials: %v", err)
	}
}

const unreachableRelay = "127.0.0.1:1"

func writeUnreachablePairing(t *testing.T, dir string) {
	t.Helper()
	if err := relaycreds.Save(dir, relaycreds.Credentials{
		Endpoint:      unreachableRelay,
		InstanceID:    "inst-relay-test",
		InstanceToken: "token-relay-test",
	}); err != nil {
		t.Fatalf("save credentials: %v", err)
	}
}

type recordingWriter struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (w *recordingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

func (w *recordingWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

const settleTimeout = 30 * time.Second

func requireGoroutinesSettleTo(t *testing.T, want int) {
	t.Helper()
	deadline := time.Now().Add(settleTimeout)
	for {
		got := runtime.NumGoroutine()
		if got <= want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("goroutines: %d wanted, %d still running", want, got)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestUnit_RemoteAttachmentsStartNothingWhenThereIsNothingToStart asserts nothing is dialed, no goroutine starts, and nothing is written when there is no pairing.
func TestUnit_RemoteAttachmentsStartNothingWhenThereIsNothingToStart(t *testing.T) {
	for _, tc := range []struct {
		name string
		dir  string
	}{
		{name: "nothing is paired", dir: t.TempDir()},
		{name: "there is no contenox directory at all", dir: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var warn, activity recordingWriter
			before := runtime.NumGoroutine()

			stop := serveRemoteAttachments(t.Context(), tc.dir, relayTestFactory, relayChainTriggers{}, nil,
				libtracker.NewTextActivityTracker(&activity), &warn)
			if stop == nil {
				t.Fatal("stop is nil; it must be safe to defer unconditionally")
			}
			if got := warn.String(); got != "" {
				t.Fatalf("an inert invocation wrote %q", got)
			}
			if got := activity.String(); got != "" {
				t.Fatalf("an inert invocation reached the relay machinery: %q", got)
			}
			requireGoroutinesSettleTo(t, before)
			stop()
		})
	}
}

// TestUnit_RelayTunnelTeardownJoinsEverythingItStarted asserts stop closes the connector and joins the tunnel before returning.
func TestUnit_RelayTunnelTeardownJoinsEverythingItStarted(t *testing.T) {
	dir := t.TempDir()
	writeUnreachablePairing(t, dir)

	before := runtime.NumGoroutine()
	stop, err := startRelayTunnel(t.Context(), dir, relayTestFactory, relayChainTriggers{}, nil, nil)
	if err != nil {
		t.Fatalf("startRelayTunnel: %v", err)
	}
	if got := runtime.NumGoroutine(); got <= before {
		t.Fatalf("goroutines: %d before, %d after start; the tunnel never ran", before, got)
	}
	stop()
	requireGoroutinesSettleTo(t, before)

	for range 4 {
		stop, err := startRelayTunnel(t.Context(), dir, relayTestFactory, relayChainTriggers{}, nil, nil)
		if err != nil {
			t.Fatalf("startRelayTunnel: %v", err)
		}
		stop()
	}
	requireGoroutinesSettleTo(t, before)
}

// TestUnit_RemoteAttachmentsReportAnUnreadablePairing asserts an unreadable pairing is reported as exactly one warning line, and stop still returns normally.
func TestUnit_RemoteAttachmentsReportAnUnreadablePairing(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writePairing(t, dir)

	var warn bytes.Buffer
	stop := serveRemoteAttachments(t.Context(), dir, relayTestFactory, relayChainTriggers{}, nil, nil, &warn)
	if stop == nil {
		t.Fatal("stop is nil; it must be safe to defer unconditionally")
	}
	stop()
	reported := warn.String()
	if reported == "" {
		t.Fatal("an unreadable pairing was not reported")
	}
	if lines := strings.Count(strings.TrimSuffix(reported, "\n"), "\n") + 1; lines != 1 {
		t.Fatalf("an unreadable pairing was reported over %d lines: %q", lines, reported)
	}
}

// TestUnit_RelayTunnelIsInertWithoutAPairing asserts an unpaired machine dials nothing, reports nothing, and gets a safe-to-defer stop.
func TestUnit_RelayTunnelIsInertWithoutAPairing(t *testing.T) {
	t.Parallel()
	for name, dir := range map[string]string{
		"no contenox directory": "",
		"nothing stored":        t.TempDir(),
	} {
		stop, err := startRelayTunnel(t.Context(), dir, relayTestFactory, relayChainTriggers{}, nil, nil)
		if err != nil {
			t.Fatalf("%s: startRelayTunnel: %v", name, err)
		}
		if stop == nil {
			t.Fatalf("%s: stop is nil; it must be safe to defer unconditionally", name)
		}
		stop()
	}
}

// TestUnit_RelayTunnelReportsAnUnreadablePairing asserts an unreadable pairing is reported as an error distinct from "not paired".
func TestUnit_RelayTunnelReportsAnUnreadablePairing(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writePairing(t, dir)

	stop, err := startRelayTunnel(t.Context(), dir, relayTestFactory, relayChainTriggers{}, nil, nil)
	if err == nil {
		t.Fatal("startRelayTunnel accepted an unreadable pairing")
	}
	if stop == nil {
		t.Fatal("stop is nil on the error path; it must be safe to defer unconditionally")
	}
	stop()
}

type fakeApprovalRouter struct {
	allowed bool
	err     error
	calls   int
}

func (r *fakeApprovalRouter) AskApproval(context.Context, hitlservice.ApprovalRequest) (bool, error) {
	r.calls++
	return r.allowed, r.err
}

// TestUnit_ApprovalsGoToTheRoutedClientAndFallBackOnlyWhenUnheld asserts the router answers first, and only ErrNoBoundSession falls back to the stdio transport.
func TestUnit_ApprovalsGoToTheRoutedClientAndFallBackOnlyWhenUnheld(t *testing.T) {
	t.Parallel()
	req := hitlservice.ApprovalRequest{ToolCallID: "call-1", ToolName: "local_fs.write_file"}

	for name, tc := range map[string]struct {
		router       *fakeApprovalRouter
		wantAllow    bool
		wantFellBack bool
	}{
		"an allow is the routed client's answer": {
			router:    &fakeApprovalRouter{allowed: true},
			wantAllow: true,
		},
		"a deny is an answer and is never re-asked": {
			router: &fakeApprovalRouter{allowed: false},
		},
		"a cancellation is an answer and is never re-asked": {
			router: &fakeApprovalRouter{err: context.Canceled},
		},
		"a dropped connection is not a reason to ask someone else": {
			router: &fakeApprovalRouter{err: libacp.ErrConnectionClosed},
		},
		"only an unheld session falls back": {
			router:       &fakeApprovalRouter{err: acpsvc.ErrNoBoundSession},
			wantFellBack: true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			fellBack := false
			ask := routedAskApproval(tc.router, func() *acpsvc.Transport {
				fellBack = true
				return nil
			})
			allowed, err := ask(t.Context(), req)

			if tc.router.calls != 1 {
				t.Fatalf("the router was consulted %d times, want exactly 1", tc.router.calls)
			}
			if fellBack != tc.wantFellBack {
				t.Fatalf("fell back to stdio = %v, want %v", fellBack, tc.wantFellBack)
			}
			if tc.wantFellBack {
				if err == nil {
					t.Fatal("a fallback with no stdio transport must report why")
				}
				return
			}
			if allowed != tc.wantAllow {
				t.Fatalf("allowed = %v, want %v", allowed, tc.wantAllow)
			}
			if !errors.Is(err, tc.router.err) {
				t.Fatalf("err = %v, want %v", err, tc.router.err)
			}
		})
	}
}

// TestUnit_ApprovalsWithoutARouterAreExactlyTheOldStdioPath asserts a nil or empty router asks only the process's own transport.
func TestUnit_ApprovalsWithoutARouterAreExactlyTheOldStdioPath(t *testing.T) {
	t.Parallel()
	req := hitlservice.ApprovalRequest{ToolCallID: "call-1", ToolName: "local_fs.write_file"}

	for name, router := range map[string]approvalRouter{
		"no router at all": nil,
		"an empty router":  acpsvc.NewSessionRouter(),
	} {
		t.Run(name, func(t *testing.T) {
			asked := 0
			ask := routedAskApproval(router, func() *acpsvc.Transport {
				asked++
				return nil
			})
			if _, err := ask(t.Context(), req); err == nil {
				t.Fatal("a callback with no transport yet must report why")
			}
			if asked != 1 {
				t.Fatalf("the stdio transport was consulted %d times, want exactly 1", asked)
			}
		})
	}
}

// A host has no connection of its own: every session it serves arrives over the
// relay, so a tool call must resolve the transport holding *that* session
// rather than a process-wide one.
func TestUnit_RoutedTransport_PrefersTheSessionsOwnConnection(t *testing.T) {
	router := acpsvc.NewSessionRouter()

	localCalls := 0
	resolve := routedTransport(router, func() *acpsvc.Transport {
		localCalls++
		return nil
	})

	// Nothing holds this session and there is no local connection: nil is the
	// correct answer, and the file/shell tools fall back to the OS from there.
	if got := resolve(t.Context()); got != nil {
		t.Fatalf("an unheld session with no local connection = %v, want nil", got)
	}
	if localCalls != 1 {
		t.Fatalf("the local connection was consulted %d times, want exactly 1", localCalls)
	}
}

// With no router at all — a surface serving exactly one connection — the local
// connection is still the answer, so the nil-safe path stays usable.
func TestUnit_RoutedTransport_FallsBackToTheLocalConnection(t *testing.T) {
	var local acpsvc.Transport
	resolve := routedTransport(acpsvc.NewSessionRouter(), func() *acpsvc.Transport { return &local })

	if got := resolve(t.Context()); got != &local {
		t.Fatalf("resolve = %v, want the local connection", got)
	}
}

// A resolver with neither router nor local connection must answer nil rather
// than panicking: that is the shape a headless host runs in.
func TestUnit_RoutedTransport_IsNilSafe(t *testing.T) {
	if got := routedTransport(nil, nil)(t.Context()); got != nil {
		t.Fatalf("resolve = %v, want nil", got)
	}
}

func TestUnit_RelayTunnelAttachesTheAskBridgeBeforeItDials(t *testing.T) {
	dir := t.TempDir()
	writeUnreachablePairing(t, dir)

	asks := newRelayAskBridge(&fakeAskInbox{}, libtracker.NoopTracker{})
	stop, err := startRelayTunnel(t.Context(), dir, relayTestFactory, relayChainTriggers{}, asks, nil)
	if err != nil {
		t.Fatalf("startRelayTunnel: %v", err)
	}
	instance, send := asks.link()
	if instance != "inst-relay-test" || send == nil {
		t.Fatalf("bridge link = %q/%v; a link held only after the first connection loses that connection's re-publish", instance, send != nil)
	}
	stop()
	if _, send := asks.link(); send != nil {
		t.Fatal("teardown left the bridge publishing into a closed connector")
	}
}

func TestUnit_RelayTunnelAttachesTheResumeBridge(t *testing.T) {
	dir := t.TempDir()
	writeUnreachablePairing(t, dir)

	resumes := newRelayResumeBridge(libtracker.NoopTracker{})
	stop, err := startRelayTunnel(t.Context(), dir, relayTestFactory, relayChainTriggers{resumes: resumes}, nil, nil)
	if err != nil {
		t.Fatalf("startRelayTunnel: %v", err)
	}
	instance, send := resumes.link()
	if instance != "inst-relay-test" || send == nil {
		t.Fatalf("resume link = %q/%v; a run resumed on a verdict could not report its outcome", instance, send != nil)
	}
	stop()
	if _, send := resumes.link(); send != nil {
		t.Fatal("teardown left an outcome to be sent into a closed connector")
	}
}
