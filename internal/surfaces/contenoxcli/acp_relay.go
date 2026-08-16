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

type approvalRouter interface {
	AskApproval(ctx context.Context, req hitlservice.ApprovalRequest) (bool, error)
}

func routedAskApproval(router approvalRouter, local func() *acpsvc.Transport) localtools.AskApproval {
	return func(ctx context.Context, req hitlservice.ApprovalRequest) (bool, error) {
		if router != nil {
			allowed, err := router.AskApproval(ctx, req)
			if !errors.Is(err, acpsvc.ErrNoBoundSession) {
				return allowed, err
			}
		}
		var t *acpsvc.Transport
		if local != nil {
			t = local()
		}
		if t == nil {
			return false, fmt.Errorf("acpsvc: HITL approval requested before transport initialization")
		}
		return t.AskApproval(ctx, req)
	}
}

// routedTransport resolves which connection a proxied tool call acts through:
// the transport holding this call's session first, then the process's local
// connection. One process may be driving several sessions at once, so the
// transport is a property of the call, not of the process.
func routedTransport(router *acpsvc.SessionRouter, local func() *acpsvc.Transport) acpsvc.TransportResolver {
	return func(ctx context.Context) *acpsvc.Transport {
		if t := router.TransportForContext(ctx); t != nil {
			return t
		}
		if local == nil {
			return nil
		}
		return local()
	}
}

func serveRemoteAttachments(
	ctx context.Context,
	contenoxDir string,
	factory libacp.AgentFactory,
	triggers relayChainTriggers,
	tracker libtracker.ActivityTracker,
	warn io.Writer,
) func() {
	stop, err := startRelayTunnel(ctx, contenoxDir, factory, triggers, tracker)
	if err != nil {
		fmt.Fprintf(warn, "contenox: remote attachments are unavailable: %v\n", err)
	}
	return stop
}

func startRelayTunnel(
	ctx context.Context,
	contenoxDir string,
	factory libacp.AgentFactory,
	triggers relayChainTriggers,
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
	trig := newRelayTriggerHandler(triggers, tracker)
	trig.instance = creds.InstanceID
	trig.send = func(f librelay.Frame) error { return connector.Send(f) }
	connector, err = relaylink.New(relaylink.Config{
		Endpoint:    creds.Endpoint,
		Instance:    creds.InstanceID,
		Credentials: relaylink.Credentials{Token: creds.InstanceToken, RelayPublicKey: creds.RelayPublicKey},
		Agent:       "contenox/" + version.Get(),
		Handler: func(hctx context.Context, f librelay.Frame) {
			if f.Type == librelay.TypeChainTrigger {
				trig.handle(hctx, f)
				return
			}
			tunnel.Handle(hctx, f)
		},
		Tracker: tracker,
	})
	if err != nil {
		tunnel.Close()
		return noop, err
	}
	if err := connector.Start(ctx); err != nil {
		tunnel.Close()
		return noop, err
	}
	// Connector first: its close cancels in-flight chain contexts, so trig.wait
	// joins promptly.
	return func() {
		_ = connector.Close()
		tunnel.Close()
		trig.wait()
	}, nil
}
