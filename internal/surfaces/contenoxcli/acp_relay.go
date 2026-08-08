// acp_relay.go attaches `contenox acp` to a paired relay, so a remote ACP
// client is served by the same agent factory the stdio path is served by. It is
// entirely optional: a machine with no pairing runs exactly the code it ran
// before, and nothing on the stdio path sequences on any of this.
package contenoxcli

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/contenox/contenox/internal/relayacp"
	"github.com/contenox/contenox/internal/relaycreds"
	"github.com/contenox/contenox/internal/relaylink"
	"github.com/contenox/contenox/internal/services/hitlservice"
	"github.com/contenox/contenox/internal/services/localtools"
	"github.com/contenox/contenox/internal/surfaces/acpsvc"
	"github.com/contenox/contenox/internal/version"
	"github.com/contenox/contenox/libacp"
	"github.com/contenox/contenox/librelay"
	"github.com/contenox/contenox/libtracker"
)

// approvalRouter is the one call routedAskApproval makes on
// [acpsvc.SessionRouter], named so the dispatch policy can be stated and tested
// without a live ACP connection behind it. *acpsvc.SessionRouter satisfies it.
type approvalRouter interface {
	AskApproval(ctx context.Context, req hitlservice.ApprovalRequest) (bool, error)
}

// routedAskApproval is the approval callback the ACP profile hands the engine:
// it asks the client driving the session the request was raised on, and falls
// back to the process's stdio transport when no client holds that session.
//
// Router first, because the engine is shared and the connections are not. Every
// transport this process builds — the stdio one and each relay attachment —
// registers with one router, so a permission request raised by work a phone
// started is answered on the phone. Before this, an approval was answered
// through whichever transport the process bound at startup, which is the stdio
// one: a card raised by remote work appeared at the desk and the phone waited
// on a question it was never shown.
//
// stdio is consulted on exactly one condition, [acpsvc.ErrNoBoundSession], and
// the narrowness is the point. A deny, a client cancellation and a dropped
// connection are all answers from the connection that was asked; retrying any
// of them against a second connection would ask a different human the same
// question and take the second verdict. Only "nobody owns this session" leaves
// the question unasked, and that is the case the stdio transport exists to
// cover — it is where a session created before anything attached still lives,
// and it reproduces the pre-router behaviour exactly for a runtime that never
// attaches anything.
//
// A stdio transport that does not hold the session either reports the durable
// ask's own message, which names the terminal command that answers it. That is
// the correct end of the chain: the ask outlives the connection, so an
// unattached question is answerable rather than lost.
//
// stdio is a func because the transport is built after the engine that consumes
// this callback; nil until the connection's factory runs.
func routedAskApproval(router approvalRouter, stdio func() *acpsvc.Transport) localtools.AskApproval {
	return func(ctx context.Context, req hitlservice.ApprovalRequest) (bool, error) {
		if router != nil {
			allowed, err := router.AskApproval(ctx, req)
			if !errors.Is(err, acpsvc.ErrNoBoundSession) {
				return allowed, err
			}
		}
		var t *acpsvc.Transport
		if stdio != nil {
			t = stdio()
		}
		if t == nil {
			return false, fmt.Errorf("acpsvc: HITL approval requested before transport initialization")
		}
		return t.AskApproval(ctx, req)
	}
}

// serveRemoteAttachments starts the relay tunnel for this invocation and
// returns the teardown, which is always safe to defer.
//
// optInBeta gates it because /pair mints the credential it reads, and /pair is
// gated. Gating the command that produces a thing but not the thing itself
// would leave an operator who turned the opt-in off still dialing on a file
// they can no longer see or remove.
//
// factory must be the raw transport factory, not the presence-wrapped one the
// stdio connection uses: an attachment must never overwrite the process's bound
// transport, which the toolset's cwd resolver and the approval path both read.
//
// warn receives one line when a pairing exists but could not be attached.
// Failure is never fatal — it costs this process remote clients and nothing
// else — and "not paired" is not a failure at all.
func serveRemoteAttachments(
	ctx context.Context,
	optInBeta bool,
	contenoxDir string,
	factory libacp.AgentFactory,
	tracker libtracker.ActivityTracker,
	warn io.Writer,
) func() {
	if !optInBeta {
		return func() {}
	}
	stop, err := startRelayTunnel(ctx, contenoxDir, factory, tracker)
	if err != nil {
		fmt.Fprintf(warn, "contenox acp: remote attachments are unavailable: %v\n", err)
	}
	return stop
}

// startRelayTunnel dials the relay this machine is paired with and serves
// remote ACP attachments through it, returning the function that tears it down
// again. It returns a usable stop function on every path, including the error
// path, so a caller never has to test one before deferring it.
//
// A machine with no pairing is not an error and not a warning: stop is a no-op
// and nothing is dialed. That is the "absent means absent" rule this repository
// applies to every optional seam — the relay is a feature a machine may not
// have, not a dependency it is missing. A pairing that exists and cannot be
// read is the opposite case and is reported, since /pair wrote it and only
// /unpair removes it.
//
// It never waits on the relay. The connector's Start returns before the first
// dial is attempted and reconnects on its own, so an unreachable relay costs
// this command nothing and is invisible to the editor on stdio.
//
// The tunnel and the connector cannot be built in one step — the connector
// needs a handler and the handler needs somewhere to send — so the send closure
// late-binds the connector. That is sound because the connector is assigned
// before Start, and the closure's only caller runs on a read loop that does not
// exist until Start.
//
// Teardown closes the connector first, so no further frame can reach an
// attachment while the tunnel is joining the ones it already has.
//
// Every attachment is served from the same acpsvc.Deps, and that is sound only
// because those Deps carry a SessionRouter: each attachment's own transport
// registers there, so an approval raised by work a remote client is driving is
// answered on that client rather than through the transport the process bound
// at startup. See routedAskApproval.
func startRelayTunnel(
	ctx context.Context,
	contenoxDir string,
	factory libacp.AgentFactory,
	tracker libtracker.ActivityTracker,
) (stop func(), err error) {
	noop := func() {}
	if contenoxDir == "" {
		return noop, nil
	}
	creds, err := relaycreds.Load(contenoxDir)
	if err != nil {
		if errors.Is(err, relaycreds.ErrNotEnrolled) {
			return noop, nil
		}
		return noop, err
	}

	var connector *relaylink.Connector
	tunnel, err := relayacp.New(relayacp.Config{
		Instance: creds.InstanceID,
		Send:     func(f librelay.Frame) error { return connector.Send(f) },
		Factory:  factory,
		Tracker:  tracker,
	})
	if err != nil {
		return noop, err
	}
	connector, err = relaylink.New(relaylink.Config{
		Endpoint:    creds.Endpoint,
		Instance:    creds.InstanceID,
		Credentials: relaylink.Credentials{Token: creds.InstanceToken, RelayPublicKey: creds.RelayPublicKey},
		Agent:       "contenox/" + version.Get(),
		Handler:     tunnel.Handle,
		Tracker:     tracker,
	})
	if err != nil {
		tunnel.Close()
		return noop, err
	}
	if err := connector.Start(ctx); err != nil {
		tunnel.Close()
		return noop, err
	}
	return func() {
		_ = connector.Close()
		tunnel.Close()
	}, nil
}
