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

// relayTestFactory is the agent factory the wiring tests hand over. Nothing
// reaches it: no attachment is ever created on any path these tests take.
func relayTestFactory(*libacp.AgentSideConnection) libacp.Agent { return libacp.UnimplementedAgent{} }

// writePairing puts a syntactically broken enrolment in dir, which is the
// "paired and broken" case as distinct from "not paired".
func writePairing(t *testing.T, dir string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, relaycreds.Filename), []byte("{not json"), 0o600); err != nil {
		t.Fatalf("write credentials: %v", err)
	}
}

// unreachableRelay is a port nothing answers on. A connector built against it
// runs its whole lifecycle — start, dial, fail, back off, tear down — without
// a relay existing anywhere, which is what makes the teardown assertion below
// hermetic.
const unreachableRelay = "127.0.0.1:1"

// writeUnreachablePairing stores a well-formed enrolment for a relay that is
// not there. It is the only shape that starts a real tunnel inside a test:
// relaylink never blocks on a dial, so everything is constructed and started
// and only the connection itself fails.
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

// recordingWriter is an io.Writer a test reads back while relay machinery may
// still be writing to it from its own goroutines; an unlocked bytes.Buffer
// would be a data race rather than an assertion.
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

// settleTimeout bounds the wait for goroutines to finish exiting. A count that
// never comes back down inside it is the leak the assertion is looking for.
const settleTimeout = 30 * time.Second

// requireGoroutinesSettleTo fails unless the live goroutine count returns to
// want. Goroutines exit asynchronously, so the claim is that the count settles,
// not that it is instantaneous.
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

// TestUnit_RemoteAttachmentsStartNothingWhenThereIsNothingToStart is the
// "changes nothing" half of this seam, asserted on every observable a surface
// has: nothing is dialed, no goroutine is started, and not a line is written.
// A terminal UI is the reason the last one matters — its stderr is the screen
// it draws the transcript into, so a stray line is a corrupted scrollback.
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

			stop := serveRemoteAttachments(t.Context(), tc.dir, relayTestFactory,
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

// TestUnit_RelayTunnelTeardownJoinsEverythingItStarted covers the exit path
// every surface defers. A tunnel that outlived its process's teardown would go
// on serving remote clients against an engine and a database being closed
// underneath it, so the connector is closed and the tunnel joined before stop
// returns.
//
// The pre-assertion that the count rose is what keeps this honest: without it a
// tunnel that silently failed to start would pass the leak check trivially.
func TestUnit_RelayTunnelTeardownJoinsEverythingItStarted(t *testing.T) {
	dir := t.TempDir()
	writeUnreachablePairing(t, dir)

	before := runtime.NumGoroutine()
	stop, err := startRelayTunnel(t.Context(), dir, relayTestFactory, nil)
	if err != nil {
		t.Fatalf("startRelayTunnel: %v", err)
	}
	if got := runtime.NumGoroutine(); got <= before {
		t.Fatalf("goroutines: %d before, %d after start; the tunnel never ran", before, got)
	}
	stop()
	requireGoroutinesSettleTo(t, before)

	for range 4 {
		stop, err := startRelayTunnel(t.Context(), dir, relayTestFactory, nil)
		if err != nil {
			t.Fatalf("startRelayTunnel: %v", err)
		}
		stop()
	}
	requireGoroutinesSettleTo(t, before)
}

// TestUnit_RemoteAttachmentsReportAnUnreadablePairing: a pairing that exists
// and cannot be read is the operator's problem to hear about, since /pair wrote
// it and only /unpair removes it. It is a warning and never a failure — the
// surface's own connection is unaffected either way.
//
// Exactly one line, because a surface whose stderr is the screen it draws into
// pays for every extra one, and the caller returns normally, because a broken
// credential file costs this process remote clients and nothing else.
func TestUnit_RemoteAttachmentsReportAnUnreadablePairing(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writePairing(t, dir)

	var warn bytes.Buffer
	stop := serveRemoteAttachments(t.Context(), dir, relayTestFactory, nil, &warn)
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

// TestUnit_RelayTunnelIsInertWithoutAPairing is the "absent means absent" rule
// for this seam: an unpaired machine dials nothing, reports nothing, and gets a
// stop function it can defer without testing it first. A warning here would
// teach every user who never paired that something is wrong with their runtime.
func TestUnit_RelayTunnelIsInertWithoutAPairing(t *testing.T) {
	t.Parallel()
	for name, dir := range map[string]string{
		"no contenox directory": "",
		"nothing stored":        t.TempDir(),
	} {
		stop, err := startRelayTunnel(t.Context(), dir, relayTestFactory, nil)
		if err != nil {
			t.Fatalf("%s: startRelayTunnel: %v", name, err)
		}
		if stop == nil {
			t.Fatalf("%s: stop is nil; it must be safe to defer unconditionally", name)
		}
		stop()
	}
}

// TestUnit_RelayTunnelReportsAnUnreadablePairing separates "not paired" from
// "paired and broken" at the layer that can tell them apart, so the caller
// above only has to decide what to do about it.
func TestUnit_RelayTunnelReportsAnUnreadablePairing(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writePairing(t, dir)

	stop, err := startRelayTunnel(t.Context(), dir, relayTestFactory, nil)
	if err == nil {
		t.Fatal("startRelayTunnel accepted an unreadable pairing")
	}
	if stop == nil {
		t.Fatal("stop is nil on the error path; it must be safe to defer unconditionally")
	}
	stop()
}

// fakeApprovalRouter answers routedAskApproval with a scripted verdict and
// records that it was consulted, so a test can tell "the router answered" from
// "the router was skipped" without a live ACP connection behind either.
type fakeApprovalRouter struct {
	allowed bool
	err     error
	calls   int
}

func (r *fakeApprovalRouter) AskApproval(context.Context, hitlservice.ApprovalRequest) (bool, error) {
	r.calls++
	return r.allowed, r.err
}

// TestUnit_ApprovalsGoToTheRoutedClientAndFallBackOnlyWhenUnheld pins the
// dispatch policy the ACP profile installs. The router owns the decision
// because the engine is shared and the connections are not: a permission
// request raised by work a remote client is driving must be answered on that
// client, not through whichever transport the process bound at startup.
//
// The fallback is deliberately one condition wide. A deny, a cancellation and a
// dropped connection are answers from the connection that was asked, and
// retrying any of them against the stdio transport would put the same question
// to a second human and take the second verdict. Only ErrNoBoundSession leaves
// the question unasked.
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

// TestUnit_ApprovalsWithoutARouterAreExactlyTheOldStdioPath keeps the seam
// absent when it is absent. A runtime that wires no router — and one whose
// router holds nothing, which is every runtime nobody has attached to — asks
// the process's own transport and nothing else, which is what `contenox acp`
// did before remote attachments existed.
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
