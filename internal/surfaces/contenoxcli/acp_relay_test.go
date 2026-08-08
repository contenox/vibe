package contenoxcli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/contenox/contenox/internal/relaycreds"
	"github.com/contenox/contenox/internal/services/hitlservice"
	"github.com/contenox/contenox/internal/surfaces/acpsvc"
	"github.com/contenox/contenox/libacp"
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

// TestUnit_RemoteAttachmentsAreInertWithoutTheOptIn keeps the gate at the same
// place /pair's is. /pair mints the credential the tunnel reads, so a runtime
// with the opt-in off must not dial on a pairing the operator can no longer see
// or remove — including one that is broken, which is why the broken file is the
// case asserted here.
func TestUnit_RemoteAttachmentsAreInertWithoutTheOptIn(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writePairing(t, dir)

	var warn bytes.Buffer
	stop := serveRemoteAttachments(t.Context(), false, dir, relayTestFactory, nil, &warn)
	if stop == nil {
		t.Fatal("stop is nil; it must be safe to defer unconditionally")
	}
	stop()
	if warn.Len() != 0 {
		t.Fatalf("a gated-off invocation reported %q", warn.String())
	}
}

// TestUnit_RemoteAttachmentsReportAnUnreadablePairing is the other half of that
// gate: with the opt-in on, a pairing that exists and cannot be read is the
// operator's problem to hear about, since /pair wrote it and only /unpair
// removes it. It is a warning and never a failure — the stdio path is
// unaffected either way.
func TestUnit_RemoteAttachmentsReportAnUnreadablePairing(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writePairing(t, dir)

	var warn bytes.Buffer
	stop := serveRemoteAttachments(t.Context(), true, dir, relayTestFactory, nil, &warn)
	if stop == nil {
		t.Fatal("stop is nil; it must be safe to defer unconditionally")
	}
	stop()
	if warn.Len() == 0 {
		t.Fatal("an unreadable pairing was not reported")
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
