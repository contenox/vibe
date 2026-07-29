package vscodeagent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/contenox/contenox/libtracker"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	"github.com/contenox/contenox/libacp"
)

type clientRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int64  `json:"id"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type clientIncomingResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *responseError  `json:"error,omitempty"`
}

type clientResponse struct {
	Result json.RawMessage
	Error  *responseError
}

func (s *Server) requestPermission(ctx context.Context, req libacp.RequestPermissionRequest) (libacp.RequestPermissionResponse, error) {
	var resp libacp.RequestPermissionResponse
	if err := s.callClient(ctx, libacp.MethodSessionRequestPermission, req, &resp); err != nil {
		return libacp.RequestPermissionResponse{}, err
	}
	return resp, nil
}

func (s *Server) callClient(ctx context.Context, method string, params any, result any) error {
	if s == nil {
		return fmt.Errorf("vscodeagent: server is nil")
	}
	s.sendMu.Lock()
	hasFramer := s.framer != nil
	s.sendMu.Unlock()
	if !hasFramer {
		return fmt.Errorf("vscodeagent: bridge connection is not available")
	}

	ch := make(chan clientResponse, 1)
	s.clientReqMu.Lock()
	s.clientReqNextID++
	id := s.clientReqNextID
	key := strconv.FormatInt(id, 10)
	s.clientReqPending[key] = ch
	s.clientReqMu.Unlock()

	if err := s.send(clientRequest{JSONRPC: jsonrpcVersion, ID: id, Method: method, Params: params}); err != nil {
		s.removeClientRequest(key)
		return fmt.Errorf("vscodeagent: write client request %s: %w", method, err)
	}

	select {
	case <-ctx.Done():
		s.removeClientRequest(key)
		_ = s.notify("$/cancelRequest", map[string]any{"id": id})
		return ctx.Err()
	case resp, ok := <-ch:
		if !ok {
			return fmt.Errorf("vscodeagent: bridge closed before %s completed", method)
		}
		if resp.Error != nil {
			return fmt.Errorf("vscodeagent: client request %s failed: %s", method, resp.Error.Message)
		}
		if result == nil {
			return nil
		}
		if len(bytes.TrimSpace(resp.Result)) == 0 {
			return fmt.Errorf("vscodeagent: client request %s returned no result", method)
		}
		if err := json.Unmarshal(resp.Result, result); err != nil {
			return fmt.Errorf("vscodeagent: unmarshal client response for %s: %w", method, err)
		}
		return nil
	}
}

func (s *Server) handleClientResponsePayload(payload []byte) (bool, error) {
	var resp clientIncomingResponse
	if err := json.Unmarshal(payload, &resp); err != nil {
		return false, err
	}
	if resp.JSONRPC != jsonrpcVersion {
		return false, nil
	}
	if strings.TrimSpace(resp.Method) != "" {
		return false, nil
	}
	if len(bytes.TrimSpace(resp.ID)) == 0 {
		return false, nil
	}
	if resp.Error == nil && len(bytes.TrimSpace(resp.Result)) == 0 {
		return false, nil
	}
	key := rpcIDKey(resp.ID)
	if key == "" {
		return true, nil
	}

	s.clientReqMu.Lock()
	ch, ok := s.clientReqPending[key]
	if ok {
		delete(s.clientReqPending, key)
	}
	s.clientReqMu.Unlock()
	if !ok {
		return true, nil
	}
	ch <- clientResponse{Result: resp.Result, Error: resp.Error}
	close(ch)
	return true, nil
}

func (s *Server) removeClientRequest(key string) {
	s.clientReqMu.Lock()
	delete(s.clientReqPending, key)
	s.clientReqMu.Unlock()
}

func (s *Server) closeClientRequests() {
	s.clientReqMu.Lock()
	pending := s.clientReqPending
	s.clientReqPending = make(map[string]chan clientResponse)
	s.clientReqMu.Unlock()
	for _, ch := range pending {
		close(ch)
	}
}

func (s *Server) sessionIDFromContext(ctx context.Context) string {
	if sid, ok := ctx.Value(runtimetypes.SessionIDContextKey).(string); ok && strings.TrimSpace(sid) != "" {
		return sid
	}
	if reqID, ok := ctx.Value(libtracker.ContextKeyRequestID).(string); ok && reqID != "" {
		if turn, ok := s.turnByRequestID(reqID); ok {
			return turn.SessionID
		}
	}
	return ""
}
