package libacp_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	libacp "github.com/contenox/beam/libacp"
)

type fakeTerminalPeer struct {
	mu sync.Mutex

	createErr error
	createID  string

	// wait blocks until either ctx dies or waitDone is closed.
	waitBlocks bool
	waitDone   chan struct{}
	waitResp   libacp.WaitForTerminalExitResponse
	waitErr    error

	outputResp libacp.TerminalOutputResponse
	outputErr  error

	created  int
	killed   int
	released int
	outputs  int

	releaseCtxErr error // ctx.Err() observed inside ReleaseTerminal
	outputCtxErr  error
}

func (f *fakeTerminalPeer) CreateTerminal(ctx context.Context, req libacp.CreateTerminalRequest) (libacp.CreateTerminalResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.created++
	if f.createErr != nil {
		return libacp.CreateTerminalResponse{}, f.createErr
	}
	id := f.createID
	if id == "" {
		id = "term-1"
	}
	return libacp.CreateTerminalResponse{TerminalID: id}, nil
}

func (f *fakeTerminalPeer) WaitForTerminalExit(ctx context.Context, req libacp.WaitForTerminalExitRequest) (libacp.WaitForTerminalExitResponse, error) {
	if f.waitBlocks {
		select {
		case <-ctx.Done():
			return libacp.WaitForTerminalExitResponse{}, ctx.Err()
		case <-f.waitDone:
		}
	}
	return f.waitResp, f.waitErr
}

func (f *fakeTerminalPeer) TerminalOutput(ctx context.Context, req libacp.TerminalOutputRequest) (libacp.TerminalOutputResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.outputs++
	f.outputCtxErr = ctx.Err()
	if f.outputErr != nil {
		return libacp.TerminalOutputResponse{}, f.outputErr
	}
	return f.outputResp, nil
}

func (f *fakeTerminalPeer) KillTerminal(ctx context.Context, req libacp.KillTerminalRequest) (libacp.KillTerminalResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.killed++
	return libacp.KillTerminalResponse{}, nil
}

func (f *fakeTerminalPeer) ReleaseTerminal(ctx context.Context, req libacp.ReleaseTerminalRequest) (libacp.ReleaseTerminalResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.released++
	f.releaseCtxErr = ctx.Err()
	return libacp.ReleaseTerminalResponse{}, nil
}

func intp(v int) *int            { return &v }
func strp(v string) *string      { return &v }
func newFake() *fakeTerminalPeer { return &fakeTerminalPeer{waitDone: make(chan struct{})} }

func TestRunTerminal_Completion(t *testing.T) {
	tests := []struct {
		name     string
		wait     libacp.WaitForTerminalExitResponse
		output   libacp.TerminalOutputResponse
		wantCode int
		wantSig  *string
		wantOut  string
		wantTrun bool
	}{
		{
			name:     "normal exit",
			wait:     libacp.WaitForTerminalExitResponse{ExitCode: intp(0)},
			output:   libacp.TerminalOutputResponse{Output: "hello\n"},
			wantCode: 0,
			wantOut:  "hello\n",
		},
		{
			name:     "non-zero exit from wait response",
			wait:     libacp.WaitForTerminalExitResponse{ExitCode: intp(3)},
			output:   libacp.TerminalOutputResponse{Output: "boom"},
			wantCode: 3,
			wantOut:  "boom",
		},
		{
			name: "exit code falls back to ExitStatus",
			wait: libacp.WaitForTerminalExitResponse{},
			output: libacp.TerminalOutputResponse{
				Output:     "fallback",
				ExitStatus: &libacp.TerminalExitStatus{ExitCode: intp(7)},
			},
			wantCode: 7,
			wantOut:  "fallback",
		},
		{
			name:     "signal maps to -1",
			wait:     libacp.WaitForTerminalExitResponse{Signal: strp("SIGKILL")},
			output:   libacp.TerminalOutputResponse{Output: "killed"},
			wantCode: -1,
			wantSig:  strp("SIGKILL"),
			wantOut:  "killed",
		},
		{
			name: "signal with explicit non-zero code keeps the code",
			wait: libacp.WaitForTerminalExitResponse{ExitCode: intp(9), Signal: strp("SIGTERM")},
			// no output exit status
			output:   libacp.TerminalOutputResponse{},
			wantCode: 9,
			wantSig:  strp("SIGTERM"),
		},
		{
			name: "signal falls back to ExitStatus",
			wait: libacp.WaitForTerminalExitResponse{},
			output: libacp.TerminalOutputResponse{
				ExitStatus: &libacp.TerminalExitStatus{Signal: strp("SIGSEGV")},
			},
			wantCode: -1,
			wantSig:  strp("SIGSEGV"),
		},
		{
			name:     "truncated is surfaced not interpreted",
			wait:     libacp.WaitForTerminalExitResponse{ExitCode: intp(0)},
			output:   libacp.TerminalOutputResponse{Output: "part", Truncated: true},
			wantCode: 0,
			wantOut:  "part",
			wantTrun: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake()
			f.waitResp = tc.wait
			f.outputResp = tc.output

			var createdID string
			res, err := libacp.RunTerminal(context.Background(), f, libacp.CreateTerminalRequest{Command: "echo"}, func(id string) {
				createdID = id
			})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if createdID != "term-1" {
				t.Fatalf("onCreated got %q", createdID)
			}
			if res.ExitCode != tc.wantCode {
				t.Errorf("exit code = %d, want %d", res.ExitCode, tc.wantCode)
			}
			if res.Output != tc.wantOut {
				t.Errorf("output = %q, want %q", res.Output, tc.wantOut)
			}
			if res.Truncated != tc.wantTrun {
				t.Errorf("truncated = %v, want %v", res.Truncated, tc.wantTrun)
			}
			switch {
			case tc.wantSig == nil && res.Signal != nil:
				t.Errorf("signal = %q, want nil", *res.Signal)
			case tc.wantSig != nil && res.Signal == nil:
				t.Errorf("signal = nil, want %q", *tc.wantSig)
			case tc.wantSig != nil && *res.Signal != *tc.wantSig:
				t.Errorf("signal = %q, want %q", *res.Signal, *tc.wantSig)
			}
			if res.Cancelled || res.TimedOut {
				t.Errorf("unexpected cancelled=%v timedOut=%v", res.Cancelled, res.TimedOut)
			}
			if f.released != 1 {
				t.Errorf("released %d times, want 1", f.released)
			}
			if f.killed != 0 {
				t.Errorf("killed %d times, want 0", f.killed)
			}
		})
	}
}

func TestRunTerminal_DeadlineIsTimeoutNotCancellation(t *testing.T) {
	f := newFake()
	f.waitBlocks = true
	f.outputResp = libacp.TerminalOutputResponse{Output: "partial"}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	res, err := libacp.RunTerminal(ctx, f, libacp.CreateTerminalRequest{Command: "sleep"}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.TimedOut || res.Cancelled {
		t.Fatalf("timedOut=%v cancelled=%v, want timedOut only", res.TimedOut, res.Cancelled)
	}
	if res.ExitCode != -1 {
		t.Errorf("exit code = %d, want -1", res.ExitCode)
	}
	if res.Output != "partial" {
		t.Errorf("output = %q, want partial output fetched despite dead ctx", res.Output)
	}
	if f.killed != 1 {
		t.Errorf("killed %d times, want 1", f.killed)
	}
	if f.released != 1 {
		t.Errorf("released %d times, want 1", f.released)
	}
}

func TestRunTerminal_CancellationIsNotTimeout(t *testing.T) {
	f := newFake()
	f.waitBlocks = true
	f.outputResp = libacp.TerminalOutputResponse{Output: "so far"}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()

	res, err := libacp.RunTerminal(ctx, f, libacp.CreateTerminalRequest{Command: "sleep"}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.Cancelled || res.TimedOut {
		t.Fatalf("cancelled=%v timedOut=%v, want cancelled only", res.Cancelled, res.TimedOut)
	}
	if res.ExitCode != -1 {
		t.Errorf("exit code = %d, want -1", res.ExitCode)
	}
	if res.Output != "so far" {
		t.Errorf("output = %q, want output fetched after cancellation", res.Output)
	}
	if f.killed != 1 {
		t.Errorf("killed %d times, want 1", f.killed)
	}
}

// The release must survive a request context that is already dead by the time
// the deferred cleanup runs, otherwise the peer leaks terminals on every
// cancelled turn.
func TestRunTerminal_ReleasesOnDeadContext(t *testing.T) {
	f := newFake()
	f.waitBlocks = true

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already dead before we even start

	res, err := libacp.RunTerminal(ctx, f, libacp.CreateTerminalRequest{Command: "sleep"}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.Cancelled {
		t.Errorf("cancelled = false, want true")
	}
	if f.released != 1 {
		t.Fatalf("released %d times, want 1", f.released)
	}
	if f.releaseCtxErr != nil {
		t.Errorf("release ran on a dead context (%v), want a detached live one", f.releaseCtxErr)
	}
	if f.outputCtxErr != nil {
		t.Errorf("output ran on a dead context (%v), want a detached live one", f.outputCtxErr)
	}
}

func TestRunTerminal_CreateFailureDoesNotRelease(t *testing.T) {
	f := newFake()
	f.createErr = errors.New("nope")

	called := false
	res, err := libacp.RunTerminal(context.Background(), f, libacp.CreateTerminalRequest{Command: "echo"}, func(string) { called = true })
	if err == nil {
		t.Fatal("expected error")
	}
	if called {
		t.Error("onCreated must not fire when create fails")
	}
	if res.ExitCode != -1 {
		t.Errorf("exit code = %d, want -1", res.ExitCode)
	}
	if f.released != 0 {
		t.Errorf("released %d times, want 0", f.released)
	}
}

func TestRunTerminal_WaitFailureWithLiveContextIsAnError(t *testing.T) {
	f := newFake()
	f.waitErr = errors.New("transport blew up")

	res, err := libacp.RunTerminal(context.Background(), f, libacp.CreateTerminalRequest{Command: "echo"}, nil)
	if err == nil {
		t.Fatal("expected error")
	}
	if res.Cancelled || res.TimedOut {
		t.Errorf("a transport failure must not be reported as cancel/timeout")
	}
	if f.killed != 0 {
		t.Errorf("killed %d times, want 0", f.killed)
	}
	if f.released != 1 {
		t.Errorf("released %d times, want 1", f.released)
	}
}

func TestRunTerminal_OutputFailureKeepsCancellationCause(t *testing.T) {
	f := newFake()
	f.waitBlocks = true
	f.outputErr = errors.New("output gone")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	res, err := libacp.RunTerminal(ctx, f, libacp.CreateTerminalRequest{Command: "sleep"}, nil)
	if err == nil {
		t.Fatal("expected error")
	}
	if !res.Cancelled {
		t.Error("cancellation cause must survive an output failure")
	}
	if f.released != 1 {
		t.Errorf("released %d times, want 1", f.released)
	}
}
