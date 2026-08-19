package agentinstance

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/contenox/contenox/libacp"
)

// Viewer is a consumer attached to one downstream session: it receives the
// session's streamed updates and, when it is the controller, answers the
// downstream agent's permission requests.
type Viewer interface {
	// ID uniquely identifies this viewer within a session; two viewers on the
	// same session must not share an ID.
	ID() string

	// Deliver receives one session update, in order. It must not block, since it
	// runs under the session lock; its returned error is advisory.
	Deliver(ctx context.Context, n libacp.SessionNotification) error

	// RequestPermission answers the downstream agent's session/request_permission.
	// Called only on the controller viewer, it may block awaiting a decision.
	RequestPermission(ctx context.Context, req libacp.RequestPermissionRequest) (libacp.RequestPermissionResponse, error)
}

// TerminalServer is an optional Viewer capability that services a downstream
// agent's terminal/* callbacks for the session it controls. It is routed only to
// the controller; otherwise terminal/* answers MethodNotFound.
type TerminalServer interface {
	CreateTerminal(ctx context.Context, req libacp.CreateTerminalRequest) (libacp.CreateTerminalResponse, error)
	TerminalOutput(ctx context.Context, req libacp.TerminalOutputRequest) (libacp.TerminalOutputResponse, error)
	WaitForTerminalExit(ctx context.Context, req libacp.WaitForTerminalExitRequest) (libacp.WaitForTerminalExitResponse, error)
	KillTerminal(ctx context.Context, req libacp.KillTerminalRequest) (libacp.KillTerminalResponse, error)
	ReleaseTerminal(ctx context.Context, req libacp.ReleaseTerminalRequest) (libacp.ReleaseTerminalResponse, error)
}

type FileSystemServer interface {
	ReadTextFile(ctx context.Context, req libacp.ReadTextFileRequest) (libacp.ReadTextFileResponse, error)
	WriteTextFile(ctx context.Context, req libacp.WriteTextFileRequest) (libacp.WriteTextFileResponse, error)
}

type InstanceFileSystem interface {
	FileSystemServer

	FileSystemCapabilities() libacp.FileSystemCapabilities
}

type sessionState struct {
	journal      *journal
	viewers      map[string]Viewer
	order        []string
	controllerID string
}

type viewerHub struct {
	instanceID  string
	journalSize int

	onAttach              func(sessionID libacp.SessionID, viewerID string, controller bool)
	onDetach              func(sessionID libacp.SessionID, viewerID string)
	onUnsupervisedDeny    func(sessionID libacp.SessionID)
	onUnsupervisedRequest func(ctx context.Context, req libacp.RequestPermissionRequest) (libacp.RequestPermissionResponse, error)

	fileSystem InstanceFileSystem
	terminal   TerminalServer

	mu       sync.Mutex
	sessions map[libacp.SessionID]*sessionState
}

func newViewerHub(instanceID string, journalSize int) *viewerHub {
	return &viewerHub{
		instanceID:  instanceID,
		journalSize: journalSize,
		sessions:    make(map[libacp.SessionID]*sessionState),
	}
}

func (h *viewerHub) session(id libacp.SessionID) *sessionState {
	s := h.sessions[id]
	if s == nil {
		s = &sessionState{
			journal: newJournal(h.journalSize),
			viewers: make(map[string]Viewer),
		}
		h.sessions[id] = s
	}
	return s
}

func (h *viewerHub) deliver(ctx context.Context, n libacp.SessionNotification) {
	h.mu.Lock()
	defer h.mu.Unlock()
	s := h.session(n.SessionID)
	s.journal.append(n)
	for _, id := range s.order {
		_ = s.viewers[id].Deliver(ctx, n)
	}
}

func (h *viewerHub) attach(ctx context.Context, sessionID libacp.SessionID, viewer Viewer) (controllerGranted bool, err error) {
	vid := viewer.ID()
	if vid == "" {
		return false, fmt.Errorf("agentinstance: viewer ID is required")
	}

	h.mu.Lock()
	s := h.session(sessionID)
	if _, dup := s.viewers[vid]; dup {
		h.mu.Unlock()
		return false, fmt.Errorf("agentinstance: viewer %q already attached to session %q", vid, sessionID)
	}
	s.viewers[vid] = viewer
	s.order = append(s.order, vid)
	if s.controllerID == "" {
		s.controllerID = vid
		controllerGranted = true
	}
	// Replay under the lock so a concurrent live update lands strictly after it.
	for _, n := range s.journal.snapshot() {
		_ = viewer.Deliver(ctx, n)
	}
	h.mu.Unlock()

	if h.onAttach != nil {
		h.onAttach(sessionID, vid, controllerGranted)
	}
	return controllerGranted, nil
}

func (h *viewerHub) detach(sessionID libacp.SessionID, viewerID string) error {
	h.mu.Lock()
	s := h.sessions[sessionID]
	if s == nil {
		h.mu.Unlock()
		return fmt.Errorf("agentinstance: session %q has no attached viewers", sessionID)
	}
	if _, ok := s.viewers[viewerID]; !ok {
		h.mu.Unlock()
		return fmt.Errorf("agentinstance: viewer %q not attached to session %q", viewerID, sessionID)
	}
	delete(s.viewers, viewerID)
	for i, id := range s.order {
		if id == viewerID {
			s.order = append(s.order[:i], s.order[i+1:]...)
			break
		}
	}
	if s.controllerID == viewerID {
		if len(s.order) > 0 {
			s.controllerID = s.order[0]
		} else {
			s.controllerID = ""
		}
	}
	if len(s.viewers) == 0 {
		delete(h.sessions, sessionID)
	}
	h.mu.Unlock()

	if h.onDetach != nil {
		h.onDetach(sessionID, viewerID)
	}
	return nil
}

func (h *viewerHub) requestPermission(ctx context.Context, req libacp.RequestPermissionRequest) (libacp.RequestPermissionResponse, error) {
	h.mu.Lock()
	var controller Viewer
	if s := h.sessions[req.SessionID]; s != nil && s.controllerID != "" {
		controller = s.viewers[s.controllerID]
	}
	h.mu.Unlock()

	if controller == nil {
		if answer := h.onUnsupervisedRequest; answer != nil {
			resp, err := answer(ctx, req)
			if err == nil {
				if permissionRefused(req, resp) {
					h.reportUnsupervisedDeny(req.SessionID)
				}
				return resp, nil
			}
		}
		h.reportUnsupervisedDeny(req.SessionID)
		return libacp.RequestPermissionResponse{
			Outcome: libacp.RequestPermissionOutcome{Outcome: libacp.PermissionOutcomeCancelled},
		}, nil
	}
	return controller.RequestPermission(ctx, req)
}

func (h *viewerHub) reportUnsupervisedDeny(sessionID libacp.SessionID) {
	if h.onUnsupervisedDeny != nil {
		h.onUnsupervisedDeny(sessionID)
	}
}

func permissionRefused(req libacp.RequestPermissionRequest, resp libacp.RequestPermissionResponse) bool {
	if resp.Outcome.Outcome != libacp.PermissionOutcomeSelected {
		return true
	}
	for _, opt := range req.Options {
		if opt.OptionID != resp.Outcome.OptionID {
			continue
		}
		return opt.Kind != libacp.PermissionAllowOnce && opt.Kind != libacp.PermissionAllowAlways
	}
	return true
}

func (h *viewerHub) terminalServer(sessionID libacp.SessionID) TerminalServer {
	h.mu.Lock()
	defer h.mu.Unlock()
	if s := h.sessions[sessionID]; s != nil && s.controllerID != "" {
		if ts, ok := s.viewers[s.controllerID].(TerminalServer); ok {
			return ts
		}
	}
	// The controller's terminal wins; a viewer-less unit falls back to the
	// instance-wide server the manager installed, exactly as fileSystemServer does.
	if h.terminal != nil {
		return h.terminal
	}
	return nil
}

func (h *viewerHub) instanceFileSystemCaps() libacp.FileSystemCapabilities {
	if h.fileSystem == nil {
		return libacp.FileSystemCapabilities{}
	}
	return h.fileSystem.FileSystemCapabilities()
}

func (h *viewerHub) instanceTerminalCap() bool {
	return h.terminal != nil
}

func (h *viewerHub) fileSystemServer(sessionID libacp.SessionID) FileSystemServer {
	h.mu.Lock()
	var controller FileSystemServer
	if s := h.sessions[sessionID]; s != nil && s.controllerID != "" {
		controller, _ = s.viewers[s.controllerID].(FileSystemServer)
	}
	h.mu.Unlock()
	if controller != nil {
		return controller
	}
	if h.fileSystem != nil {
		return h.fileSystem
	}
	return nil
}

func (h *viewerHub) closeSession(sessionID libacp.SessionID) {
	h.mu.Lock()
	s := h.sessions[sessionID]
	if s == nil {
		h.mu.Unlock()
		return
	}
	ids := append([]string(nil), s.order...)
	delete(h.sessions, sessionID)
	h.mu.Unlock()

	if h.onDetach != nil {
		for _, id := range ids {
			h.onDetach(sessionID, id)
		}
	}
}

func (h *viewerHub) viewerCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	viewers := 0
	for _, s := range h.sessions {
		viewers += len(s.viewers)
	}
	return viewers
}

func (h *viewerHub) journalSnapshot(sessionID libacp.SessionID) []libacp.SessionNotification {
	h.mu.Lock()
	s := h.sessions[sessionID]
	if s == nil {
		h.mu.Unlock()
		return nil
	}
	snapshot := s.journal.snapshot()
	h.mu.Unlock()
	return snapshot
}

func (h *viewerHub) agentText(sessionID libacp.SessionID) string {
	h.mu.Lock()
	s := h.sessions[sessionID]
	if s == nil {
		h.mu.Unlock()
		return ""
	}
	snapshot := s.journal.snapshot()
	h.mu.Unlock()

	var sb strings.Builder
	for _, n := range snapshot {
		if n.Update.SessionUpdate != libacp.SessionUpdateAgentMessageChunk {
			continue
		}
		if c := n.Update.Content; c != nil && c.Type == string(libacp.ContentKindText) {
			sb.WriteString(c.Text)
		}
	}
	return sb.String()
}
