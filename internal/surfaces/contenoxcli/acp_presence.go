package contenoxcli

import (
	"context"

	"github.com/contenox/contenox/internal/services/presence"
	"github.com/contenox/libacp"
)

// presenceAgent decorates the ACP transport to feed the fleet-presence
// reporter the attached client name and open session count; other methods
// promote to the real transport unchanged. The session count is an
// approximation from counting opens/closes, self-correcting over time.
type presenceAgent struct {
	libacp.Agent
	reporter *presence.Reporter
}

func newPresenceAgent(agent libacp.Agent, reporter *presence.Reporter) libacp.Agent {
	return &presenceAgent{Agent: agent, reporter: reporter}
}

func (p *presenceAgent) Initialize(ctx context.Context, req libacp.InitializeRequest) (libacp.InitializeResponse, error) {
	resp, err := p.Agent.Initialize(ctx, req)
	// Capture the client identity regardless of the transport's own outcome.
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
