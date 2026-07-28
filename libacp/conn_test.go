package libacp_test

import (
	"context"
	"encoding/json"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/contenox/contenox/libacp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type pipeRW struct {
	r *io.PipeReader
	w *io.PipeWriter
}

func (p *pipeRW) Read(b []byte) (int, error)  { return p.r.Read(b) }
func (p *pipeRW) Write(b []byte) (int, error) { return p.w.Write(b) }
func (p *pipeRW) Close() error {
	_ = p.r.Close()
	return p.w.Close()
}

func newPipePair() (io.ReadWriteCloser, io.ReadWriteCloser) {
	ar, bw := io.Pipe()
	br, aw := io.Pipe()
	return &pipeRW{r: ar, w: aw}, &pipeRW{r: br, w: bw}
}

type stubAgent struct {
	libacp.UnimplementedAgent
	mu        sync.Mutex
	conn      *libacp.AgentSideConnection
	promptReq libacp.PromptRequest
}

func (a *stubAgent) Initialize(_ context.Context, req libacp.InitializeRequest) (libacp.InitializeResponse, error) {
	return libacp.InitializeResponse{
		ProtocolVersion: libacp.ProtocolVersion,
		AgentInfo:       &libacp.Implementation{Name: "stub", Version: "0.0.1"},
		AgentCapabilities: libacp.AgentCapabilities{
			LoadSession: false,
			PromptCapabilities: libacp.PromptCapabilities{
				EmbeddedContext: req.ClientCapabilities.FS.ReadTextFile,
			},
		},
	}, nil
}

func (a *stubAgent) NewSession(_ context.Context, _ libacp.NewSessionRequest) (libacp.NewSessionResponse, error) {
	return libacp.NewSessionResponse{SessionID: libacp.SessionID("sess-1")}, nil
}

func (a *stubAgent) Prompt(ctx context.Context, req libacp.PromptRequest) (libacp.PromptResponse, error) {
	a.mu.Lock()
	a.promptReq = req
	a.mu.Unlock()

	if err := a.conn.SessionUpdate(libacp.SessionNotification{
		SessionID: req.SessionID,
		Update:    libacp.NewAgentMessageChunk("hello "),
	}); err != nil {
		return libacp.PromptResponse{}, err
	}
	if err := a.conn.SessionUpdate(libacp.SessionNotification{
		SessionID: req.SessionID,
		Update:    libacp.NewAgentMessageChunk("world"),
	}); err != nil {
		return libacp.PromptResponse{}, err
	}
	return libacp.PromptResponse{StopReason: libacp.StopReasonEndTurn}, nil
}

func TestUnit_AgentSideConnection_InitializeAndPrompt(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	agentSide, clientSide := newPipePair()

	stub := &stubAgent{}
	conn := libacp.NewAgentSideConnection(agentSide, func(c *libacp.AgentSideConnection) libacp.Agent {
		stub.conn = c
		return stub
	})

	runErr := make(chan error, 1)
	go func() { runErr <- conn.Run(ctx) }()

	clientReader := bufReader(clientSide)
	clientWriter := bufWriter(clientSide)

	send := func(method string, id int64, params any) {
		paramsRaw, err := json.Marshal(params)
		require.NoError(t, err)
		req := libacp.NewRequest(libacp.NewRequestIDNumber(id), method, paramsRaw)
		require.NoError(t, clientWriter(req))
	}

	expectNotification := func(method string) libacp.Notification {
		line, err := clientReader()
		require.NoError(t, err)
		in, err := libacp.ParseIncoming(line)
		require.NoError(t, err)
		require.Equal(t, libacp.IncomingKindNotification, in.Kind)
		require.Equal(t, method, in.Notification.Method)
		return in.Notification
	}

	expectResponse := func(id int64) libacp.Response {
		line, err := clientReader()
		require.NoError(t, err)
		in, err := libacp.ParseIncoming(line)
		require.NoError(t, err)
		require.Equal(t, libacp.IncomingKindResponse, in.Kind)
		require.Equal(t, libacp.NewRequestIDNumber(id), in.Response.ID)
		return in.Response
	}

	send(libacp.MethodInitialize, 1, libacp.InitializeRequest{
		ProtocolVersion: libacp.ProtocolVersion,
		ClientCapabilities: libacp.ClientCapabilities{
			FS: libacp.FileSystemCapabilities{ReadTextFile: true},
		},
	})
	resp := expectResponse(1)
	require.Nil(t, resp.Error)
	var initResp libacp.InitializeResponse
	require.NoError(t, json.Unmarshal(resp.Result, &initResp))
	assert.Equal(t, libacp.ProtocolVersion, initResp.ProtocolVersion)
	assert.Equal(t, "stub", initResp.AgentInfo.Name)
	assert.True(t, initResp.AgentCapabilities.PromptCapabilities.EmbeddedContext)

	send(libacp.MethodSessionNew, 2, libacp.NewSessionRequest{
		Cwd:        "/tmp",
		McpServers: []libacp.McpServer{},
	})
	resp = expectResponse(2)
	require.Nil(t, resp.Error)
	var newSess libacp.NewSessionResponse
	require.NoError(t, json.Unmarshal(resp.Result, &newSess))
	assert.Equal(t, libacp.SessionID("sess-1"), newSess.SessionID)

	send(libacp.MethodSessionPrompt, 3, libacp.PromptRequest{
		SessionID: "sess-1",
		Prompt:    []libacp.ContentBlock{libacp.NewTextContent("say hi")},
	})

	n1 := expectNotification(libacp.MethodSessionUpdate)
	var sn1 libacp.SessionNotification
	require.NoError(t, json.Unmarshal(n1.Params, &sn1))
	assert.Equal(t, libacp.SessionUpdateAgentMessageChunk, sn1.Update.SessionUpdate)
	require.NotNil(t, sn1.Update.Content)
	assert.Equal(t, "hello ", sn1.Update.Content.Text)

	n2 := expectNotification(libacp.MethodSessionUpdate)
	var sn2 libacp.SessionNotification
	require.NoError(t, json.Unmarshal(n2.Params, &sn2))
	assert.Equal(t, "world", sn2.Update.Content.Text)

	resp = expectResponse(3)
	require.Nil(t, resp.Error)
	var promptResp libacp.PromptResponse
	require.NoError(t, json.Unmarshal(resp.Result, &promptResp))
	assert.Equal(t, libacp.StopReasonEndTurn, promptResp.StopReason)

	stub.mu.Lock()
	assert.Equal(t, libacp.SessionID("sess-1"), stub.promptReq.SessionID)
	stub.mu.Unlock()

	_ = clientSide.Close()
	select {
	case <-runErr:
	case <-time.After(time.Second):
		t.Fatal("connection did not shut down after client close")
	}
}

func bufReader(rw io.Reader) func() ([]byte, error) {
	var buf []byte
	tmp := make([]byte, 4096)
	return func() ([]byte, error) {
		for {
			for i, b := range buf {
				if b == '\n' {
					line := make([]byte, i)
					copy(line, buf[:i])
					buf = append([]byte{}, buf[i+1:]...)
					return line, nil
				}
			}
			n, err := rw.Read(tmp)
			if n > 0 {
				buf = append(buf, tmp[:n]...)
			}
			if err != nil {
				return nil, err
			}
		}
	}
}

func bufWriter(rw io.Writer) func(v any) error {
	return func(v any) error {
		data, err := json.Marshal(v)
		if err != nil {
			return err
		}
		data = append(data, '\n')
		_, err = rw.Write(data)
		return err
	}
}
