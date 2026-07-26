package contenoxcli

import (
	"context"

	"github.com/contenox/beam/internal/services/presence"
	"github.com/contenox/beam/libacp"
)

// presenceAgent decorates the ACP transport to feed the fleet-presence reporter
// two facts it cannot see from outside the connection: WHO the client is (from
// the initialize handshake — Zed identifies itself, and all editors share the
// `contenox acp` subcommand, so the client name is the only thing that tells them
// apart on the board) and HOW MANY sessions are open (the session-lifecycle
// methods).
//
// It works by EMBEDDING the libacp.Agent interface: every method it does not
// override is promoted to the real transport unchanged, so this decorator adds
// presence bookkeeping without reimplementing — or even knowing — the transport's
// behavior. That is deliberately why presence stays out of acpsvc entirely: the
// runtime's ACP transport is under active change, and a read-only observer at the
// CLI seam has no business editing it. Every Update is best-effort and
// non-blocking (the reporter coalesces), so decorating never slows a real ACP
// call.
//
// The session count is an APPROXIMATION maintained by counting opens against
// closes — accurate for the common editor case (a handful of tabs opened and
// closed explicitly), and self-correcting on the next real open/close; a session
// that vanishes only when the whole connection drops is captured by the process
// exiting and its presence row aging out, not by this counter.
type presenceAgent struct {
	libacp.Agent
	reporter *presence.Reporter
}

func newPresenceAgent(agent libacp.Agent, reporter *presence.Reporter) libacp.Agent {
	return &presenceAgent{Agent: agent, reporter: reporter}
}

func (p *presenceAgent) Initialize(ctx context.Context, req libacp.InitializeRequest) (libacp.InitializeResponse, error) {
	resp, err := p.Agent.Initialize(ctx, req)
	// Capture the client identity regardless of the transport's own outcome: even
	// a setup-only initialize tells us which editor is attached.
	if req.ClientInfo != nil && req.ClientInfo.Name != "" {
		name := req.ClientInfo.Name
		p.reporter.Update(func(rec *presence.Record) { rec.ClientName = name })
	}
	return resp, err
}

func (p *presenceAgent) NewSession(ctx context.Context, req libacp.NewSessionRequest) (libacp.NewSessionResponse, error) {
	resp, err := p.Agent.NewSession(ctx, req)
	if err == nil {
		p.sessionDelta(1)
	}
	return resp, err
}

func (p *presenceAgent) LoadSession(ctx context.Context, req libacp.LoadSessionRequest) (libacp.LoadSessionResponse, error) {
	resp, err := p.Agent.LoadSession(ctx, req)
	if err == nil {
		p.sessionDelta(1)
	}
	return resp, err
}

func (p *presenceAgent) ResumeSession(ctx context.Context, req libacp.ResumeSessionRequest) (libacp.ResumeSessionResponse, error) {
	resp, err := p.Agent.ResumeSession(ctx, req)
	if err == nil {
		p.sessionDelta(1)
	}
	return resp, err
}

func (p *presenceAgent) CloseSession(ctx context.Context, req libacp.CloseSessionRequest) (libacp.CloseSessionResponse, error) {
	resp, err := p.Agent.CloseSession(ctx, req)
	if err == nil {
		p.sessionDelta(-1)
	}
	return resp, err
}

func (p *presenceAgent) DeleteSession(ctx context.Context, req libacp.DeleteSessionRequest) (libacp.DeleteSessionResponse, error) {
	resp, err := p.Agent.DeleteSession(ctx, req)
	if err == nil {
		p.sessionDelta(-1)
	}
	return resp, err
}

func (p *presenceAgent) sessionDelta(d int) {
	p.reporter.Update(func(rec *presence.Record) {
		rec.SessionCount += d
		if rec.SessionCount < 0 {
			rec.SessionCount = 0
		}
	})
}
