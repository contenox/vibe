package enginebridge

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"sync"

	libacp "github.com/contenox/contenox/libacp"
)

// defaultTerminalOutputLimit bounds a terminal's retained output when the
// agent sends no OutputByteLimit; the newest bytes win, since errors cluster
// at the tail.
const defaultTerminalOutputLimit = int64(1 << 20)

// terminals is the client half of the ACP terminal capability: each Create
// runs one process and the other four methods address it by id until Release.
type terminals struct {
	mu    sync.Mutex
	procs map[string]*termProc
	seq   int
}

type termProc struct {
	cmd  *exec.Cmd
	done chan struct{}

	mu        sync.Mutex
	buf       []byte
	truncated bool
	limit     int64
	exit      *libacp.TerminalExitStatus
}

// Write retains the tail of the process output within limit; termProc is the
// process's stdout and stderr at once, so the lock also serializes the two
// streams' interleaving.
func (p *termProc) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.buf = append(p.buf, b...)
	if over := int64(len(p.buf)) - p.limit; over > 0 {
		p.buf = p.buf[over:]
		p.truncated = true
	}
	return len(b), nil
}

func (p *termProc) snapshot() (string, bool, *libacp.TerminalExitStatus) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return string(p.buf), p.truncated, p.exit
}

func (t *terminals) create(cmd *exec.Cmd, limit *int64) (string, *termProc) {
	p := &termProc{cmd: cmd, done: make(chan struct{}), limit: defaultTerminalOutputLimit}
	if limit != nil && *limit > 0 {
		p.limit = *limit
	}
	cmd.Stdout = p
	cmd.Stderr = p

	t.mu.Lock()
	defer t.mu.Unlock()
	if t.procs == nil {
		t.procs = make(map[string]*termProc)
	}
	t.seq++
	id := fmt.Sprintf("beam-term-%d", t.seq)
	t.procs[id] = p
	return id, p
}

func (t *terminals) get(id string) (*termProc, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	p, ok := t.procs[id]
	if !ok {
		return nil, fmt.Errorf("enginebridge: unknown terminal %q", id)
	}
	return p, nil
}

func (t *terminals) release(id string) *termProc {
	t.mu.Lock()
	defer t.mu.Unlock()
	p := t.procs[id]
	delete(t.procs, id)
	return p
}

func (c *bridgeClient) CreateTerminal(_ context.Context, req libacp.CreateTerminalRequest) (libacp.CreateTerminalResponse, error) {
	b := c.b
	if b.root == "" {
		return libacp.CreateTerminalResponse{}, errNoWorkspace
	}
	cwd := req.Cwd
	if cwd == "" {
		cwd = b.root
	}
	contained, err := b.containPath(cwd)
	if err != nil {
		return libacp.CreateTerminalResponse{}, err
	}

	cmd := exec.Command(req.Command, req.Args...)
	cmd.Dir = contained
	cmd.Env = os.Environ()
	for _, v := range req.Env {
		cmd.Env = append(cmd.Env, v.Name+"="+v.Value)
	}
	setProcGroup(cmd)

	id, p := b.terms.create(cmd, req.OutputByteLimit)
	if err := cmd.Start(); err != nil {
		b.terms.release(id)
		return libacp.CreateTerminalResponse{}, err
	}
	go func() {
		_ = cmd.Wait()
		status := exitStatus(cmd)
		p.mu.Lock()
		p.exit = &status
		p.mu.Unlock()
		close(p.done)
	}()
	return libacp.CreateTerminalResponse{TerminalID: id}, nil
}

func (c *bridgeClient) TerminalOutput(_ context.Context, req libacp.TerminalOutputRequest) (libacp.TerminalOutputResponse, error) {
	p, err := c.b.terms.get(req.TerminalID)
	if err != nil {
		return libacp.TerminalOutputResponse{}, err
	}
	out, truncated, exit := p.snapshot()
	return libacp.TerminalOutputResponse{Output: out, Truncated: truncated, ExitStatus: exit}, nil
}

func (c *bridgeClient) WaitForTerminalExit(ctx context.Context, req libacp.WaitForTerminalExitRequest) (libacp.WaitForTerminalExitResponse, error) {
	p, err := c.b.terms.get(req.TerminalID)
	if err != nil {
		return libacp.WaitForTerminalExitResponse{}, err
	}
	select {
	case <-p.done:
	case <-ctx.Done():
		return libacp.WaitForTerminalExitResponse{}, ctx.Err()
	}
	_, _, exit := p.snapshot()
	if exit == nil {
		return libacp.WaitForTerminalExitResponse{}, nil
	}
	return libacp.WaitForTerminalExitResponse{ExitCode: exit.ExitCode, Signal: exit.Signal}, nil
}

func (c *bridgeClient) KillTerminal(_ context.Context, req libacp.KillTerminalRequest) (libacp.KillTerminalResponse, error) {
	p, err := c.b.terms.get(req.TerminalID)
	if err != nil {
		return libacp.KillTerminalResponse{}, err
	}
	killProcTree(p.cmd)
	return libacp.KillTerminalResponse{}, nil
}

func (c *bridgeClient) ReleaseTerminal(_ context.Context, req libacp.ReleaseTerminalRequest) (libacp.ReleaseTerminalResponse, error) {
	p := c.b.terms.release(req.TerminalID)
	if p != nil {
		select {
		case <-p.done:
		default:
			killProcTree(p.cmd)
		}
	}
	return libacp.ReleaseTerminalResponse{}, nil
}
